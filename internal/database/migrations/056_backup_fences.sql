-- Backup fences. A running `ant backup` (possibly a separate process or job)
-- inserts a row before it takes its metadata snapshot and keeps expires_at in the
-- future with a heartbeat. While any unexpired row exists, garbage collection
-- and the expedited cache/pressure sweep do not delete blob bytes, so a blob the
-- snapshot lists cannot disappear before the backup has copied or checked it.
-- A crashed backup stops heartbeating and its fence expires by itself. The table
-- is ephemeral and is excluded from backups.
CREATE TABLE backup_fences (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 holder text NOT NULL DEFAULT '',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL
);
CREATE INDEX backup_fences_expiry_idx ON backup_fences(expires_at);
