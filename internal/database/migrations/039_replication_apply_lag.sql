-- Lateness of the most recently applied incremental changes: the age of the
-- oldest change in the page when it was applied. Status lag reads 0 once a
-- burst is caught up, so this keeps the lateness visible for a while.
ALTER TABLE replication_jobs ADD COLUMN last_apply_lag_seconds double precision NOT NULL DEFAULT 0, ADD COLUMN last_apply_lag_at timestamptz;
