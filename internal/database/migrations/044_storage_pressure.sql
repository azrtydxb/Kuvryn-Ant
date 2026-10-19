-- Instance-wide retention defaults and storage-pressure eviction (spec 6.4).
--
-- retention_settings holds the editable instance defaults; values supplied by
-- the deployment environment override them at read time and are never written
-- here. retention_pressure_state is the last observation of the pressure check
-- so the console and metrics survive restarts.
CREATE TABLE retention_settings (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 default_enabled boolean NOT NULL DEFAULT false,
 default_keep_last integer CHECK(default_keep_last IS NULL OR default_keep_last>=1),
 default_max_age_seconds bigint CHECK(default_max_age_seconds IS NULL OR default_max_age_seconds>0),
 default_max_idle_seconds bigint CHECK(default_max_idle_seconds IS NULL OR default_max_idle_seconds>0),
 default_min_keep integer NOT NULL DEFAULT 1 CHECK(default_min_keep>=0),
 default_include_tagged boolean NOT NULL DEFAULT false,
 pressure_enabled boolean NOT NULL DEFAULT false,
 pressure_high_percent integer NOT NULL DEFAULT 85 CHECK(pressure_high_percent BETWEEN 2 AND 99),
 pressure_low_percent integer NOT NULL DEFAULT 75 CHECK(pressure_low_percent BETWEEN 1 AND 98),
 capacity_bytes bigint CHECK(capacity_bytes IS NULL OR capacity_bytes>0),
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 updated_at timestamptz NOT NULL DEFAULT now(),
 CHECK(pressure_low_percent<pressure_high_percent)
);

CREATE TABLE retention_pressure_state (
 instance_id uuid PRIMARY KEY REFERENCES instances(id),
 state text NOT NULL CHECK(state IN ('disabled','no_capacity','invalid','ok','pressure','unevictable')),
 detail text NOT NULL DEFAULT '',
 checked_at timestamptz NOT NULL DEFAULT now(),
 used_bytes bigint NOT NULL DEFAULT 0,
 capacity_bytes bigint NOT NULL DEFAULT 0,
 last_evicted_at timestamptz,
 last_evicted_versions integer NOT NULL DEFAULT 0,
 last_evicted_bytes bigint NOT NULL DEFAULT 0,
 total_evicted_versions bigint NOT NULL DEFAULT 0,
 total_evicted_bytes bigint NOT NULL DEFAULT 0
);

-- A candidate normally becomes sweepable one grace period after it was first
-- seen. Blobs freed by pressure eviction carry an explicit, shorter sweep_after.
-- The sweep transaction still locks the blobs row and re-checks references, so
-- shortening the wait never weakens the deletion protocol.
ALTER TABLE gc_candidates ADD COLUMN sweep_after timestamptz;
