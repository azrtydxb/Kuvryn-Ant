-- Online blob storage migration between backends (disk <-> S3) without changing
-- repository URLs. storage_active is the durable switch; storage_migrations holds
-- resumable job state with a persisted cursor.

CREATE TABLE storage_active (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 backend text NOT NULL CHECK(backend IN ('disk','s3')),
 -- Set after a switch: the old store, still intact and used as a read fallback
 -- until an explicit cleanup job removes its data.
 previous_backend text CHECK(previous_backend IS NULL OR previous_backend IN ('disk','s3')),
 switched_at timestamptz NOT NULL DEFAULT now(),
 CHECK(previous_backend IS NULL OR previous_backend<>backend)
);

CREATE TABLE storage_migrations (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 kind text NOT NULL CHECK(kind IN ('migrate','cleanup')),
 -- migrate: copy from from_backend to to_backend, then switch.
 -- cleanup: from_backend is the old store being emptied; to_backend is active.
 from_backend text NOT NULL CHECK(from_backend IN ('disk','s3')),
 to_backend text NOT NULL CHECK(to_backend IN ('disk','s3')),
 dry_run boolean NOT NULL DEFAULT false,
 state text NOT NULL CHECK(state IN ('running','paused','failed','completed')),
 owner text,
 throttle_bytes_per_second bigint NOT NULL DEFAULT 0 CHECK(throttle_bytes_per_second>=0),
 total_blobs bigint NOT NULL DEFAULT 0 CHECK(total_blobs>=0),
 total_bytes bigint NOT NULL DEFAULT 0 CHECK(total_bytes>=0),
 processed_blobs bigint NOT NULL DEFAULT 0 CHECK(processed_blobs>=0),
 processed_bytes bigint NOT NULL DEFAULT 0 CHECK(processed_bytes>=0),
 copied_blobs bigint NOT NULL DEFAULT 0 CHECK(copied_blobs>=0),
 copied_bytes bigint NOT NULL DEFAULT 0 CHECK(copied_bytes>=0),
 skipped_blobs bigint NOT NULL DEFAULT 0 CHECK(skipped_blobs>=0),
 deleted_blobs bigint NOT NULL DEFAULT 0 CHECK(deleted_blobs>=0),
 deleted_bytes bigint NOT NULL DEFAULT 0 CHECK(deleted_bytes>=0),
 failed_blobs bigint NOT NULL DEFAULT 0 CHECK(failed_blobs>=0),
 pass integer NOT NULL DEFAULT 1 CHECK(pass>=1),
 pass_copied bigint NOT NULL DEFAULT 0 CHECK(pass_copied>=0),
 -- Last blobs.id fully handled in the current pass; NULL before the first.
 cursor uuid,
 last_error text CHECK(last_error IS NULL OR octet_length(last_error)<=256),
 requested_by uuid,
 started_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 switched_at timestamptz,
 CHECK(from_backend<>to_backend)
);
CREATE INDEX storage_migrations_instance_idx ON storage_migrations(instance_id,started_at DESC);
-- At most one unfinished job per instance; a crashed one stays 'running' with a
-- stale updated_at until another process takes it over.
CREATE UNIQUE INDEX storage_migrations_one_open ON storage_migrations(instance_id) WHERE state IN ('running','paused');
