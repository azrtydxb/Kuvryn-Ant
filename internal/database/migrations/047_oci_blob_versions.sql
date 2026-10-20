-- OCI blobs (image config and layers) are stored as a version of their
-- repository's component, keyed by digest, carrying a single 'blob' asset. That
-- row is the storage carrier for reference checks, access accounting, eviction
-- and garbage collection, but it is not a version a person pulls. blob_only marks
-- such carriers so package version counts and listings show manifests only.
--
-- The flag is derived from the asset, never supplied by a client: inserting a
-- 'blob' asset into a version of an OCI source's component (and nothing else)
-- marks it, and a later non-blob asset (a manifest fetched under the same
-- digest) clears it. This covers publish, pull-through, replication and import
-- alike. Existing rows are classified by `ant repair oci-blob-versions`.
ALTER TABLE versions ADD COLUMN blob_only boolean NOT NULL DEFAULT false;

CREATE FUNCTION mark_oci_blob_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.asset_key='blob' THEN
  UPDATE versions v SET blob_only=true
   FROM components c JOIN sources s ON s.id=c.source_id
   WHERE v.id=NEW.version_id AND c.id=v.component_id AND s.format IN ('oci','oci-artifacts') AND NOT v.blob_only
    AND NOT EXISTS(SELECT 1 FROM assets a WHERE a.version_id=v.id AND a.asset_key<>'blob');
 ELSE
  UPDATE versions SET blob_only=false WHERE id=NEW.version_id AND blob_only;
 END IF;
 RETURN NULL;
END $$;

CREATE TRIGGER assets_mark_oci_blob_version AFTER INSERT ON assets FOR EACH ROW EXECUTE FUNCTION mark_oci_blob_version();
