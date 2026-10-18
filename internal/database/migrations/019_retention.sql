-- Retention policies, two-phase blob garbage collection and integrity scanning.

-- Remote-cache eviction orders by last access. Readers may record access with
-- retention.Touch; absent values fall back to created_at.
ALTER TABLE assets ADD COLUMN last_accessed_at timestamptz;

-- A policy targets exactly one feed or one source. A source policy overrides the
-- policies of its feeds. Release content in local/replica sources is only
-- eligible when allow_release_delete is set.
CREATE TABLE retention_policies (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_id uuid,
 source_id uuid,
 keep_last integer CHECK(keep_last IS NULL OR keep_last>=1),
 max_age_seconds bigint CHECK(max_age_seconds IS NULL OR max_age_seconds>0),
 max_idle_seconds bigint CHECK(max_idle_seconds IS NULL OR max_idle_seconds>0),
 min_keep integer NOT NULL DEFAULT 1 CHECK(min_keep>=0),
 include_tagged boolean NOT NULL DEFAULT false,
 allow_release_delete boolean NOT NULL DEFAULT false,
 enabled boolean NOT NULL DEFAULT true,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK((feed_id IS NULL) <> (source_id IS NULL)),
 CHECK(keep_last IS NOT NULL OR max_age_seconds IS NOT NULL OR max_idle_seconds IS NOT NULL),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id),
 FOREIGN KEY(instance_id,source_id) REFERENCES sources(instance_id,id)
);
CREATE UNIQUE INDEX retention_policies_feed_idx ON retention_policies(instance_id,feed_id) WHERE feed_id IS NOT NULL;
CREATE UNIQUE INDEX retention_policies_source_idx ON retention_policies(instance_id,source_id) WHERE source_id IS NOT NULL;

-- Mark phase output. A candidate is only swept by a later run, after the grace
-- period, and only if the sweep transaction re-checks it as still unreferenced.
CREATE TABLE gc_candidates (
 instance_id uuid NOT NULL,
 dedup_domain_id uuid NOT NULL,
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 kind text NOT NULL CHECK(kind IN ('row','object')),
 first_seen_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(dedup_domain_id,digest),
 FOREIGN KEY(instance_id,dedup_domain_id) REFERENCES dedup_domains(instance_id,id)
);
CREATE INDEX gc_candidates_instance_idx ON gc_candidates(instance_id,first_seen_at);

-- Resumable scan positions (object listing and integrity batches).
CREATE TABLE retention_cursors (
 instance_id uuid NOT NULL REFERENCES instances(id),
 name text NOT NULL CHECK(name<>''),
 position text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,name)
);

CREATE TABLE blob_integrity_findings (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 instance_id uuid NOT NULL,
 dedup_domain_id uuid NOT NULL,
 digest text NOT NULL CHECK(digest ~ '^[0-9a-f]{64}$'),
 kind text NOT NULL CHECK(kind IN ('missing','size','digest')),
 detected_at timestamptz NOT NULL DEFAULT now(),
 resolved_at timestamptz,
 FOREIGN KEY(instance_id,dedup_domain_id) REFERENCES dedup_domains(instance_id,id)
);
CREATE UNIQUE INDEX blob_integrity_open_idx ON blob_integrity_findings(dedup_domain_id,digest,kind) WHERE resolved_at IS NULL;
