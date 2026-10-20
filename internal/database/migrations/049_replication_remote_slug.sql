-- The slug of the remote feed a replication job pulls, learned from the peer's
-- head response on every run. It lets the console show a name instead of a
-- UUID; NULL until the peer (a newer version) reports it.
ALTER TABLE replication_jobs ADD COLUMN remote_feed_slug text;
