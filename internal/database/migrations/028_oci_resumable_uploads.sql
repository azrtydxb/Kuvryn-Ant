-- Upload sessions stage either in a local file ('file') or in the blob store's
-- resumable staging ('resumable'). NULL means not yet decided (no bytes yet).
ALTER TABLE oci_uploads ADD COLUMN staging text CHECK(staging IN ('file','resumable'));
UPDATE oci_uploads SET staging='file';
