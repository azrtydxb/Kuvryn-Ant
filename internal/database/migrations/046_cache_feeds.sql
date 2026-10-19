-- Cache feeds: a feed purpose for build caches (buildx registry layer cache,
-- sccache over WebDAV). A cache feed is evictable by retention and storage
-- pressure without allow_release_delete, is never replicated, is hidden from
-- package search by default and lets an OCI tag move.
--
-- cache_max_idle_seconds / cache_max_bytes are the per-feed cache policy and
-- only apply to purpose='cache': content idle (latest of created, last
-- accessed, last downloaded) for longer than max_idle is evicted, and when
-- max_bytes is set the least recently used content is evicted until the feed is
-- under it.
ALTER TABLE feeds ADD COLUMN purpose text NOT NULL DEFAULT 'artifacts' CHECK(purpose IN ('artifacts','cache'));
ALTER TABLE feeds ADD COLUMN cache_max_idle_seconds bigint NOT NULL DEFAULT 604800 CHECK(cache_max_idle_seconds>0);
ALTER TABLE feeds ADD COLUMN cache_max_bytes bigint CHECK(cache_max_bytes IS NULL OR cache_max_bytes>0);
ALTER TABLE feeds ADD CONSTRAINT feeds_cache_format CHECK(purpose='artifacts' OR format IN ('oci','raw'));

-- version_refs records, for versions published to a cache feed, which other
-- versions of the same component a manifest names (an image's config and
-- layers, an index's manifests). A superseded manifest is released together
-- with the layers nothing else references. It is stored by version string, like
-- version_children, and is only written for cache feeds.
CREATE TABLE version_refs (
 component_id uuid NOT NULL,
 parent_version_id uuid NOT NULL,
 ref_version text NOT NULL CHECK(ref_version<>''),
 PRIMARY KEY(parent_version_id,ref_version),
 FOREIGN KEY(component_id,parent_version_id) REFERENCES versions(component_id,id) ON DELETE CASCADE
);
CREATE INDEX version_refs_ref_idx ON version_refs(component_id,ref_version);

-- Cache feeds are never replicated. Their churn must not feed the change
-- sequence either (every recorded change serializes on the per-instance head
-- row), so the change triggers skip sources attached to a cache feed. The
-- functions below are the 020/024 versions with that one extra condition.
CREATE INDEX IF NOT EXISTS feed_sources_source_idx ON feed_sources(source_id);

CREATE FUNCTION replication_cache_source(src uuid) RETURNS boolean LANGUAGE sql STABLE AS $$
 SELECT EXISTS(SELECT 1 FROM feed_sources fs JOIN feeds f ON f.id=fs.feed_id WHERE fs.source_id=src AND f.purpose='cache')
$$;

CREATE OR REPLACE FUNCTION replication_record_asset() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r assets%ROWTYPE; src uuid; skind text; coord text; ver text;
BEGIN
 IF TG_OP='DELETE' THEN r:=OLD; ELSE r:=NEW; END IF;
 SELECT c.source_id,s.kind,c.coordinate,v.version INTO src,skind,coord,ver
  FROM versions v JOIN components c ON c.id=v.component_id JOIN sources s ON s.id=c.source_id WHERE v.id=r.version_id;
 IF NOT FOUND OR skind='replica' OR replication_cache_source(src) THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,version,item_key)
  VALUES(r.instance_id,src,'asset',CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'put' END,coord,ver,r.asset_key);
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION replication_record_tag() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE r tags%ROWTYPE; inst uuid; src uuid; skind text; coord text; ver text;
BEGIN
 IF TG_OP='DELETE' THEN r:=OLD; ELSE r:=NEW; END IF;
 SELECT c.instance_id,c.source_id,s.kind,c.coordinate INTO inst,src,skind,coord
  FROM components c JOIN sources s ON s.id=c.source_id WHERE c.id=r.component_id;
 IF NOT FOUND OR skind='replica' OR replication_cache_source(src) THEN RETURN NULL; END IF;
 SELECT version INTO ver FROM versions WHERE id=r.version_id;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,version,item_key)
  VALUES(inst,src,'tag',CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'put' END,coord,COALESCE(ver,''),r.name);
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION replication_record_subject() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE src uuid; skind text; coord text; ver text; akey text;
BEGIN
 SELECT c.source_id,s.kind,c.coordinate,v.version,a.asset_key INTO src,skind,coord,ver,akey
  FROM assets a JOIN versions v ON v.id=a.version_id JOIN components c ON c.id=v.component_id JOIN sources s ON s.id=c.source_id WHERE a.id=NEW.asset_id;
 IF NOT FOUND OR skind='replica' OR replication_cache_source(src) THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,version,item_key) VALUES(NEW.instance_id,src,'asset','put',coord,ver,akey);
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION replication_record_component() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE skind text; r components%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN r:=OLD; ELSE
  r:=NEW;
  IF to_jsonb(OLD)->'metadata' IS NOT DISTINCT FROM to_jsonb(NEW)->'metadata' AND to_jsonb(OLD)->'namespace' IS NOT DISTINCT FROM to_jsonb(NEW)->'namespace' THEN
   RETURN NULL;
  END IF;
 END IF;
 SELECT kind INTO skind FROM sources WHERE id=r.source_id;
 IF NOT FOUND OR skind='replica' OR replication_cache_source(r.source_id) THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate)
  VALUES(r.instance_id,r.source_id,'component',CASE WHEN TG_OP='DELETE' THEN 'delete' ELSE 'put' END,r.coordinate);
 RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION replication_record_attachment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE skind text;
BEGIN
 SELECT kind INTO skind FROM sources WHERE id=NEW.source_id;
 IF NOT FOUND OR skind='replica' THEN RETURN NULL; END IF;
 IF EXISTS(SELECT 1 FROM feeds WHERE id=NEW.feed_id AND purpose='cache') THEN RETURN NULL; END IF;
 IF NOT EXISTS(SELECT 1 FROM components WHERE source_id=NEW.source_id) THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,item_key)
  VALUES(NEW.instance_id,NEW.source_id,'attachment','put','',NEW.feed_id::text);
 RETURN NULL;
END $$;
