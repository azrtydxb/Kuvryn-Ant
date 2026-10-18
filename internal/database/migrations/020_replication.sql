-- Independent-instance replication (spec 6.7, architecture A09).
--
-- Source side: a durable, per-instance, gapless change sequence. Row triggers
-- record changes with a NULL seq; one deferred constraint trigger assigns the
-- sequence at commit time while holding the per-instance head row lock. That
-- lock is held until the transaction commits, so sequence order equals commit
-- order and a reader that has seen seq N can never later see a lower seq.
-- Replica-kind sources are never recorded: an authority only exports its own
-- content (local and cached pull-through), never content it replicated.
CREATE TABLE replication_heads (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 seq bigint NOT NULL DEFAULT 0 CHECK(seq>=0),
 changed_at timestamptz
);
CREATE TABLE replication_changes (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 instance_id uuid NOT NULL,
 seq bigint CHECK(seq>0),
 txid bigint NOT NULL DEFAULT txid_current(),
 source_id uuid NOT NULL,
 kind text NOT NULL CHECK(kind IN ('asset','tag','component')),
 op text NOT NULL CHECK(op IN ('put','delete')),
 coordinate text NOT NULL,
 version text NOT NULL DEFAULT '',
 item_key text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX replication_changes_seq_idx ON replication_changes(instance_id,seq) WHERE seq IS NOT NULL;
CREATE INDEX replication_changes_pending_idx ON replication_changes(instance_id,txid) WHERE seq IS NULL;

CREATE FUNCTION replication_assign_seq() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE base bigint; n bigint;
BEGIN
 IF NOT EXISTS(SELECT 1 FROM replication_changes WHERE instance_id=NEW.instance_id AND txid=txid_current() AND seq IS NULL) THEN
  RETURN NULL;
 END IF;
 INSERT INTO replication_heads(instance_id) VALUES(NEW.instance_id) ON CONFLICT(instance_id) DO NOTHING;
 SELECT seq INTO base FROM replication_heads WHERE instance_id=NEW.instance_id FOR UPDATE;
 WITH pending AS (
  SELECT id,row_number() OVER (ORDER BY id) AS rn FROM replication_changes WHERE instance_id=NEW.instance_id AND txid=txid_current() AND seq IS NULL
 ), upd AS (
  UPDATE replication_changes c SET seq=base+pending.rn FROM pending WHERE c.id=pending.id RETURNING 1
 ) SELECT count(*) INTO n FROM upd;
 UPDATE replication_heads SET seq=base+n,changed_at=clock_timestamp() WHERE instance_id=NEW.instance_id;
 RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER replication_changes_assign_seq AFTER INSERT ON replication_changes
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.seq IS NULL) EXECUTE FUNCTION replication_assign_seq();

CREATE FUNCTION replication_record_asset() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r assets%ROWTYPE; src uuid; skind text; coord text; ver text;
BEGIN
 IF TG_OP='DELETE' THEN r:=OLD; ELSE r:=NEW; END IF;
 SELECT c.source_id,s.kind,c.coordinate,v.version INTO src,skind,coord,ver
  FROM versions v JOIN components c ON c.id=v.component_id JOIN sources s ON s.id=c.source_id WHERE v.id=r.version_id;
 IF NOT FOUND OR skind='replica' THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,version,item_key)
  VALUES(r.instance_id,src,'asset',CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'put' END,coord,ver,r.asset_key);
 RETURN NULL;
END $$;
CREATE TRIGGER replication_assets_change AFTER INSERT OR UPDATE OR DELETE ON assets FOR EACH ROW EXECUTE FUNCTION replication_record_asset();

CREATE FUNCTION replication_record_tag() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r tags%ROWTYPE; inst uuid; src uuid; skind text; coord text; ver text;
BEGIN
 IF TG_OP='DELETE' THEN r:=OLD; ELSE r:=NEW; END IF;
 SELECT c.instance_id,c.source_id,s.kind,c.coordinate INTO inst,src,skind,coord
  FROM components c JOIN sources s ON s.id=c.source_id WHERE c.id=r.component_id;
 IF NOT FOUND OR skind='replica' THEN RETURN NULL; END IF;
 SELECT version INTO ver FROM versions WHERE id=r.version_id;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,version,item_key)
  VALUES(inst,src,'tag',CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'put' END,coord,COALESCE(ver,''),r.name);
 RETURN NULL;
END $$;
CREATE TRIGGER replication_tags_change AFTER INSERT OR UPDATE OR DELETE ON tags FOR EACH ROW EXECUTE FUNCTION replication_record_tag();

-- Index/subject metadata updates re-send the owning asset.
CREATE FUNCTION replication_record_subject() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE src uuid; skind text; coord text; ver text; akey text;
BEGIN
 SELECT c.source_id,s.kind,c.coordinate,v.version,a.asset_key INTO src,skind,coord,ver,akey
  FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN sources s ON s.id=c.source_id WHERE a.id=NEW.asset_id;
 IF NOT FOUND OR skind='replica' THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,version,item_key) VALUES(NEW.instance_id,src,'asset','put',coord,ver,akey);
 RETURN NULL;
END $$;
CREATE TRIGGER replication_subjects_change AFTER INSERT OR UPDATE ON artifact_subjects FOR EACH ROW EXECUTE FUNCTION replication_record_subject();

-- The update trigger is column-agnostic (no UPDATE OF / WHEN reference): it
-- compares in the function body so schema repair tooling that rebuilds the
-- namespace column is not blocked by a trigger dependency.
CREATE FUNCTION replication_record_component() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE skind text; r components%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN r:=OLD; ELSE
  r:=NEW;
  IF to_jsonb(OLD)->'metadata' IS NOT DISTINCT FROM to_jsonb(NEW)->'metadata' AND to_jsonb(OLD)->'namespace' IS NOT DISTINCT FROM to_jsonb(NEW)->'namespace' THEN
   RETURN NULL;
  END IF;
 END IF;
 SELECT kind INTO skind FROM sources WHERE id=r.source_id;
 IF NOT FOUND OR skind='replica' THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate)
  VALUES(r.instance_id,r.source_id,'component',CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'put' END,r.coordinate);
 RETURN NULL;
END $$;
CREATE TRIGGER replication_components_update AFTER UPDATE ON components FOR EACH ROW EXECUTE FUNCTION replication_record_component();
CREATE TRIGGER replication_components_delete AFTER DELETE ON components FOR EACH ROW EXECUTE FUNCTION replication_record_component();

-- Destination side. Peer credentials live in encrypted_secrets with a purpose
-- bound to the exact peer; the API never returns them. CA references name
-- operator-configured bundles, never request-supplied paths.
CREATE TABLE replication_peers (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$'),
 url text NOT NULL CHECK(octet_length(url) BETWEEN 8 AND 2048),
 ca_ref text CHECK(ca_ref ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$'),
 token_secret_id uuid NOT NULL,
 token_purpose text GENERATED ALWAYS AS ('replication-peer:'||id::text) STORED,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,name), UNIQUE(instance_id,id),
 FOREIGN KEY(instance_id,token_secret_id,token_purpose) REFERENCES encrypted_secrets(instance_id,id,purpose)
);
CREATE TABLE replication_jobs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(name ~ '^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$'),
 peer_id uuid NOT NULL,
 remote_feed_id uuid NOT NULL,
 dest_feed_id uuid NOT NULL,
 replica_source_id uuid NOT NULL,
 mode text NOT NULL DEFAULT 'continuous' CHECK(mode IN ('continuous','scheduled','manual')),
 interval_seconds integer NOT NULL DEFAULT 300 CHECK(interval_seconds BETWEEN 1 AND 604800),
 include_cached boolean NOT NULL DEFAULT true,
 filters jsonb NOT NULL DEFAULT '{}'::jsonb CHECK(jsonb_typeof(filters)='object' AND octet_length(filters::text)<=16384),
 deletion_policy text NOT NULL DEFAULT 'ignore' CHECK(deletion_policy IN ('ignore','propagate')),
 enabled boolean NOT NULL DEFAULT true,
 phase text NOT NULL DEFAULT 'new' CHECK(phase IN ('new','seed_assets','seed_tags','incremental')),
 seed_head bigint CHECK(seed_head>=0),
 seed_cursor text NOT NULL DEFAULT '',
 seed_total bigint,
 seed_done bigint NOT NULL DEFAULT 0,
 checkpoint bigint NOT NULL DEFAULT 0 CHECK(checkpoint>=0),
 checkpoint_change_at timestamptz,
 remote_head bigint,
 remote_head_at timestamptz,
 remote_checked_at timestamptz,
 next_run_at timestamptz,
 run_requested boolean NOT NULL DEFAULT false,
 lease_owner uuid,
 lease_until timestamptz,
 last_attempt_at timestamptz,
 last_success_at timestamptz,
 last_error text,
 consecutive_failures integer NOT NULL DEFAULT 0,
 assets_applied bigint NOT NULL DEFAULT 0,
 blobs_transferred bigint NOT NULL DEFAULT 0,
 blobs_deduplicated bigint NOT NULL DEFAULT 0,
 bytes_transferred bigint NOT NULL DEFAULT 0,
 deletions_applied bigint NOT NULL DEFAULT 0,
 conflicts bigint NOT NULL DEFAULT 0,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE(instance_id,name), UNIQUE(instance_id,id), UNIQUE(replica_source_id),
 CHECK((lease_owner IS NULL)=(lease_until IS NULL)),
 FOREIGN KEY(instance_id,peer_id) REFERENCES replication_peers(instance_id,id),
 FOREIGN KEY(instance_id,dest_feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,replica_source_id) REFERENCES sources(instance_id,id)
);
CREATE INDEX replication_jobs_due_idx ON replication_jobs(next_run_at) WHERE enabled;
-- Tombstones received while the job's policy is 'ignore'. A later switch to
-- 'propagate' can apply them; a subsequent re-publish of the same key clears them.
CREATE TABLE replication_held_deletions (
 job_id uuid NOT NULL REFERENCES replication_jobs(id) ON DELETE CASCADE,
 kind text NOT NULL CHECK(kind IN ('asset','tag','component')),
 coordinate text NOT NULL,
 version text NOT NULL DEFAULT '',
 item_key text NOT NULL DEFAULT '',
 seq bigint NOT NULL,
 held_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(job_id,kind,coordinate,version,item_key)
);
