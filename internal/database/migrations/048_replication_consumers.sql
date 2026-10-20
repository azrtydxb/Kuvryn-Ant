-- Outgoing replication visibility. Replication is pull based: the destination
-- owns the job, so the authority only sees consumers when they call the export
-- API. This records, per (consumer, feed), the last pull, the change sequence
-- last served against the head at that time, the bytes served and the last
-- result. A consumer is a bearer principal (token mode) or a federation member
-- (federation mode). Rows are written in batches by the export tracker, never
-- on the request path, and history begins when the tracker first runs.
CREATE TABLE replication_consumers (
 instance_id uuid NOT NULL,
 feed_id uuid NOT NULL,
 consumer_kind text NOT NULL CHECK(consumer_kind IN ('principal','member')),
 consumer_id text NOT NULL CHECK(consumer_id<>''),
 first_seen timestamptz NOT NULL,
 last_seen timestamptz NOT NULL,
 pulls bigint NOT NULL DEFAULT 0 CHECK(pulls>=0),
 bytes_served bigint NOT NULL DEFAULT 0 CHECK(bytes_served>=0),
 last_sequence bigint CHECK(last_sequence IS NULL OR last_sequence>=0),
 head_sequence bigint CHECK(head_sequence IS NULL OR head_sequence>=0),
 job_name text NOT NULL DEFAULT '',
 last_result text NOT NULL DEFAULT 'ok' CHECK(last_result IN ('ok','error')),
 last_error_at timestamptz,
 last_error text NOT NULL DEFAULT '',
 PRIMARY KEY(instance_id,feed_id,consumer_kind,consumer_id),
 FOREIGN KEY(instance_id,feed_id) REFERENCES feeds(instance_id,id) ON DELETE CASCADE
);
CREATE INDEX replication_consumers_seen_idx ON replication_consumers(instance_id,last_seen);

-- Coarse lag history: at most one sample per consumer, feed and 5 minutes,
-- kept for 7 days.
CREATE TABLE replication_consumer_samples (
 instance_id uuid NOT NULL,
 feed_id uuid NOT NULL,
 consumer_kind text NOT NULL,
 consumer_id text NOT NULL,
 bucket timestamptz NOT NULL,
 last_sequence bigint NOT NULL,
 head_sequence bigint NOT NULL,
 PRIMARY KEY(instance_id,feed_id,consumer_kind,consumer_id,bucket),
 FOREIGN KEY(instance_id,feed_id,consumer_kind,consumer_id) REFERENCES replication_consumers(instance_id,feed_id,consumer_kind,consumer_id) ON DELETE CASCADE
);
