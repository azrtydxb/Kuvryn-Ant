-- Replication resilience: bounded change-history retention with a low-water
-- mark, source-attachment change records for back-fill, reseed and back-fill
-- state on the destination, and failure/reseed counters.

-- Every change with seq <= low_water has been (or may be) purged. A consumer
-- whose cursor is below low_water can no longer be served incrementally and
-- must reseed.
ALTER TABLE replication_heads
 ADD COLUMN low_water bigint NOT NULL DEFAULT 0 CHECK(low_water>=0),
 ADD COLUMN purged_at timestamptz,
 ADD COLUMN purged_total bigint NOT NULL DEFAULT 0 CHECK(purged_total>=0);
ALTER TABLE replication_changes DROP CONSTRAINT replication_changes_kind_check;
ALTER TABLE replication_changes ADD CONSTRAINT replication_changes_kind_check CHECK(kind IN ('asset','tag','component','attachment'));
CREATE INDEX replication_changes_created_idx ON replication_changes(instance_id,created_at) WHERE seq IS NOT NULL;

-- A source attached to a feed changes what the feed exports without touching
-- any asset row, so an attachment of a source that already holds content is a
-- change (op 'put'; item_key carries the feed id, the coordinate is empty).
-- Attaching an empty source records nothing: its future content produces
-- ordinary asset changes. Detaching is not recorded; replicated content stays
-- at destinations and deletions follow the destination's deletion policy.
CREATE FUNCTION replication_record_attachment() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE skind text;
BEGIN
 SELECT kind INTO skind FROM sources WHERE id=NEW.source_id;
 IF NOT FOUND OR skind='replica' THEN RETURN NULL; END IF;
 IF NOT EXISTS(SELECT 1 FROM components WHERE source_id=NEW.source_id) THEN RETURN NULL; END IF;
 INSERT INTO replication_changes(instance_id,source_id,kind,op,coordinate,item_key)
  VALUES(NEW.instance_id,NEW.source_id,'attachment','put','',NEW.feed_id::text);
 RETURN NULL;
END $$;
CREATE TRIGGER replication_attachments_change AFTER INSERT ON feed_sources FOR EACH ROW EXECUTE FUNCTION replication_record_attachment();

-- Destination side. seed_generation increases with every (re)seed and is
-- stamped into asset provenance so a finished reseed can tell replicated items
-- the source no longer lists from items it re-confirmed.
ALTER TABLE replication_jobs
 ADD COLUMN seed_generation bigint NOT NULL DEFAULT 0 CHECK(seed_generation>=0),
 ADD COLUMN reseeding boolean NOT NULL DEFAULT false,
 ADD COLUMN reseeds bigint NOT NULL DEFAULT 0 CHECK(reseeds>=0),
 ADD COLUMN last_reseed_at timestamptz,
 ADD COLUMN source_low_water bigint CHECK(source_low_water>=0),
 ADD COLUMN failures_total bigint NOT NULL DEFAULT 0 CHECK(failures_total>=0),
 ADD COLUMN backfills_completed bigint NOT NULL DEFAULT 0 CHECK(backfills_completed>=0),
 ADD COLUMN resumed_bytes bigint NOT NULL DEFAULT 0 CHECK(resumed_bytes>=0);

-- Sources attached to the remote feed after the job's seed point. Each entry
-- is queued in the same transaction that advances the checkpoint past the
-- attachment change and is drained (assets, then tags) before the next change
-- page, so a restart resumes at the stored cursor.
CREATE TABLE replication_backfills (
 job_id uuid NOT NULL REFERENCES replication_jobs(id) ON DELETE CASCADE,
 source_id uuid NOT NULL,
 origin_name text NOT NULL DEFAULT '',
 phase text NOT NULL DEFAULT 'assets' CHECK(phase IN ('assets','tags')),
 cursor text NOT NULL DEFAULT '',
 done bigint NOT NULL DEFAULT 0,
 seq bigint NOT NULL,
 queued_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(job_id,source_id)
);
