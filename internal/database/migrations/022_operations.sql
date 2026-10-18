-- Operational visibility and lifecycle state for the management console.

-- Time-bucketed counters flushed in batches from memory; never written per request.
CREATE TABLE traffic_buckets (
 instance_id uuid NOT NULL REFERENCES instances(id),
 bucket timestamptz NOT NULL,
 scope text NOT NULL CHECK(scope IN ('instance','feed','source')),
 ref text NOT NULL DEFAULT '' CHECK(octet_length(ref)<=64),
 metric text NOT NULL CHECK(metric IN ('pulls','publishes','bytes_out','bytes_in','cache_hits','cache_misses','errors')),
 value bigint NOT NULL CHECK(value>=0),
 PRIMARY KEY(instance_id,scope,ref,metric,bucket)
);
CREATE INDEX traffic_buckets_bucket_idx ON traffic_buckets(bucket);

-- Per-asset download counters, flushed in batches.
CREATE TABLE asset_downloads (
 asset_id uuid PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
 instance_id uuid NOT NULL,
 downloads bigint NOT NULL CHECK(downloads>=0),
 last_downloaded_at timestamptz NOT NULL
);

-- Periodically persisted upstream health (the live view is an in-memory ring).
CREATE TABLE remote_source_health (
 instance_id uuid NOT NULL,
 source_id uuid NOT NULL,
 last_success_at timestamptz,
 last_error_at timestamptz,
 last_status integer CHECK(last_status IS NULL OR last_status BETWEEN 0 AND 999),
 last_error text CHECK(last_error IS NULL OR octet_length(last_error)<=256),
 p95_ms integer CHECK(p95_ms IS NULL OR p95_ms>=0),
 requests bigint NOT NULL DEFAULT 0,
 failures bigint NOT NULL DEFAULT 0,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(instance_id,source_id),
 FOREIGN KEY(instance_id,source_id) REFERENCES sources(instance_id,id) ON DELETE CASCADE
);

CREATE TABLE instance_settings (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 session_max_age_seconds integer CHECK(session_max_age_seconds IS NULL OR session_max_age_seconds BETWEEN 1 AND 28800),
 session_idle_timeout_seconds integer CHECK(session_idle_timeout_seconds IS NULL OR session_idle_timeout_seconds BETWEEN 1 AND 1800),
 default_anonymous_read boolean,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE retention_runs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 trigger text NOT NULL CHECK(trigger IN ('manual','scheduled')),
 dry_run boolean NOT NULL,
 state text NOT NULL CHECK(state IN ('running','succeeded','failed')),
 requested_by uuid,
 started_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 report jsonb CHECK(report IS NULL OR octet_length(report::text)<=1048576)
);
CREATE INDEX retention_runs_instance_idx ON retention_runs(instance_id,started_at DESC);
CREATE UNIQUE INDEX retention_runs_one_running ON retention_runs(instance_id) WHERE state='running';

-- At most one audit streaming sink per instance. Credentials live only in
-- encrypted_secrets (purpose 'audit-sink').
CREATE TABLE audit_sinks (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 kind text NOT NULL CHECK(kind IN ('syslog','http')),
 endpoint text NOT NULL CHECK(octet_length(endpoint) BETWEEN 1 AND 2048),
 secret_ref uuid,
 enabled boolean NOT NULL DEFAULT true,
 checkpoint bigint NOT NULL DEFAULT 0,
 last_forwarded_at timestamptz,
 last_error text CHECK(last_error IS NULL OR octet_length(last_error)<=256),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now()
);
