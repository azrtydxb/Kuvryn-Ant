-- Prewarm jobs (spec 6.5): warm a feed's remote cache from a package list, a
-- lockfile, an image list or everything another feed references, now or on a
-- schedule. A job is the durable definition, a run one execution of it, and the
-- per-item rows are the durable checkpoint a restart resumes from.
CREATE TABLE prewarm_jobs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 feed_id uuid NOT NULL,
 name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 200),
 source_kind text NOT NULL CHECK(source_kind IN ('packages','lockfile','images','feed')),
 lockfile_type text CHECK(lockfile_type IS NULL OR lockfile_type IN ('package-lock.json','pnpm-lock.yaml','go.sum','Cargo.lock','requirements.txt','poetry.lock')),
 source_feed_id uuid,
 items jsonb NOT NULL DEFAULT '[]' CHECK(jsonb_typeof(items)='array' AND octet_length(items::text)<=16777216),
 options jsonb NOT NULL DEFAULT '{}' CHECK(jsonb_typeof(options)='object' AND octet_length(options::text)<=65536),
 concurrency integer NOT NULL DEFAULT 4 CHECK(concurrency BETWEEN 1 AND 32),
 rate_per_second integer NOT NULL DEFAULT 10 CHECK(rate_per_second BETWEEN 0 AND 1000),
 interval_seconds integer CHECK(interval_seconds IS NULL OR interval_seconds BETWEEN 60 AND 2678400),
 enabled boolean NOT NULL DEFAULT true,
 next_run_at timestamptz,
 revision bigint NOT NULL DEFAULT 1 CHECK(revision>0),
 created_by uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE,
 CHECK((source_kind='lockfile')=(lockfile_type IS NOT NULL)),
 CHECK((source_kind='feed')=(source_feed_id IS NOT NULL))
);
CREATE INDEX prewarm_jobs_feed_idx ON prewarm_jobs(instance_id,feed_id,id);
CREATE INDEX prewarm_jobs_due_idx ON prewarm_jobs(instance_id,next_run_at) WHERE enabled AND interval_seconds IS NOT NULL;

CREATE TABLE prewarm_runs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 instance_id uuid NOT NULL REFERENCES instances(id),
 job_id uuid NOT NULL REFERENCES prewarm_jobs(id) ON DELETE CASCADE,
 feed_id uuid NOT NULL,
 trigger text NOT NULL CHECK(trigger IN ('manual','schedule','retry')),
 state text NOT NULL CHECK(state IN ('queued','running','succeeded','partial','failed','canceled')),
 cancel_requested boolean NOT NULL DEFAULT false,
 principal_id uuid NOT NULL,
 retry_of uuid,
 total integer NOT NULL DEFAULT 0,
 warmed integer NOT NULL DEFAULT 0,
 cached integer NOT NULL DEFAULT 0,
 failed integer NOT NULL DEFAULT 0,
 skipped integer NOT NULL DEFAULT 0,
 bytes bigint NOT NULL DEFAULT 0,
 error text CHECK(error IS NULL OR octet_length(error)<=256),
 created_at timestamptz NOT NULL DEFAULT now(),
 started_at timestamptz,
 finished_at timestamptz
);
CREATE INDEX prewarm_runs_job_idx ON prewarm_runs(instance_id,job_id,created_at DESC,id DESC);
CREATE INDEX prewarm_runs_feed_idx ON prewarm_runs(instance_id,feed_id,created_at DESC,id DESC);
-- One active run per job: a schedule tick or a repeated request cannot stack runs.
CREATE UNIQUE INDEX prewarm_runs_one_active ON prewarm_runs(job_id) WHERE state IN ('queued','running');

CREATE TABLE prewarm_run_items (
 run_id uuid NOT NULL REFERENCES prewarm_runs(id) ON DELETE CASCADE,
 seq integer NOT NULL CHECK(seq>=0),
 name text NOT NULL CHECK(octet_length(name) BETWEEN 1 AND 1024),
 version text NOT NULL DEFAULT '' CHECK(octet_length(version)<=512),
 integrity text NOT NULL DEFAULT '' CHECK(octet_length(integrity)<=4096),
 aux text NOT NULL DEFAULT '' CHECK(octet_length(aux)<=1024),
 state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','warmed','cached','failed','skipped')),
 bytes bigint NOT NULL DEFAULT 0,
 attempts integer NOT NULL DEFAULT 0,
 error text CHECK(error IS NULL OR octet_length(error)<=256),
 finished_at timestamptz,
 PRIMARY KEY(run_id,seq)
);
CREATE INDEX prewarm_run_items_state_idx ON prewarm_run_items(run_id,state,seq);
