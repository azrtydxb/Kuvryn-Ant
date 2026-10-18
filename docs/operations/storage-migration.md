# Storage migration (disk and S3)

Kuvryn Ant can move all blobs between the local-disk and S3 backends while it
keeps serving. Repository URLs do not change, because blobs are addressed by
digest and the active backend is resolved at request time.

## Prerequisites

- Both backends must be configured. `ANT_STORAGE_DIRECTORY` is always required;
  for S3 set `ANT_S3_REGION`, `ANT_S3_BUCKET`, `ANT_S3_CREDENTIALS_FILE` (and the
  optional endpoint, prefix and CA file). `ANT_STORAGE_BACKEND` only chooses the
  backend used until the first migration; afterwards the persisted active
  backend wins.
- Run it as the instance administrator, on the host (`ant storage ...`) or from
  Settings → Storage in the console, or through
  `POST /api/v1/storage/migrations`.

## Procedure

1. Dry run: `ant storage migrate --to s3 --dry-run`. Nothing is copied. The report
   states exactly how many blobs and bytes a real run would copy, and it fails
   for any blob whose source bytes are unreadable or corrupt.
2. Migrate: `ant storage migrate --to s3 [--throttle-mb-per-sec 50]`.
   - Every blob row of the instance is copied. Each copy is verified by size and
     SHA-256 by reading it back from the target. A target object that already
     exists is verified the same way and replaced if corrupt.
   - Progress is checkpointed to PostgreSQL with a cursor. Interrupt with
     Ctrl-C, a crash or a restart, then run the same command (or press Resume in
     the console) to continue from the cursor.
   - After the copy, extra catch-up passes pick up blobs published while the job
     ran. When a pass copies nothing and no blob failed, the active backend
     switches in one database transaction. If any blob failed, nothing switches.
   - Reads are served throughout: from the source until the switch, then from the
     target, falling back to the old store for any blob the target lacks.
3. The old store is left intact. While it is retained, another migration is
   refused. Verify the new backend, then run
   `ant storage cleanup --dry-run` and `ant storage cleanup`. Cleanup verifies
   (and, if needed, repairs) every blob on the active backend before deleting its
   copy from the old one. Orphan bytes without a database row are not touched;
   scheduled garbage collection handles those on the active backend only.

`ant storage status` prints the active and retained backends and the latest job.

## Notes

- Only one unfinished job per instance. A job whose owner crashed is taken over
  after 90 seconds without a heartbeat.
- A blob committed between the final catch-up pass and the switch is still
  readable through the fallback and is copied by cleanup before the old copy is
  deleted.
- S3 multipart uploads can be made resumable across restarts with
  `blob.S3Config.StateStore` and `StageResumable`; garbage collection aborts
  multipart uploads idle longer than `retention.Options.StaleUploadAge`
  (default 24 hours) and reports them as `stale_uploads_aborted`.
