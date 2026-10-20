# Storage migration

Kuvryn Ant can move all blobs between the local-disk and S3 backends while it keeps serving. Repository URLs do not change, because blobs are addressed by digest and the active backend is resolved at request time. See [Blob storage](../install/storage.md) for configuring each backend.

## Prerequisites

- **Both backends must be configured** in the environment of the process that runs the migration (and of the server, so it can read from either):
  - Disk: `ANT_DATA_DIR` (always used; it also holds upload staging).
  - S3: `ANT_S3_REGION`, `ANT_S3_BUCKET`, `ANT_S3_CREDENTIALS_FILE`, and optionally `ANT_S3_ENDPOINT`, `ANT_S3_PREFIX`, `ANT_S3_PATH_STYLE`, `ANT_S3_CA_FILE`.
- `ANT_STORAGE_BACKEND` only chooses the backend used **until the first migration**. Afterwards the persisted active backend (stored in PostgreSQL) wins, and every command and the server agree on it.
- Run the migration as the instance administrator: on the host with `ant storage ...`, from the console (**Settings, Storage**), or through the API.

## Procedure

### 1. Dry run

```sh
ant storage migrate --to s3 --dry-run
```

Nothing is copied. The report states exactly how many blobs and bytes a real run would copy, and it fails for any blob whose source bytes are unreadable or corrupt.

### 2. Migrate

```sh
ant storage migrate --to s3 --throttle-mb-per-sec 50
```

- Every blob row of the instance is copied. Each copy is verified by size and SHA-256 by reading it back from the target. A target object that already exists is verified the same way and replaced if corrupt.
- Progress is checkpointed to PostgreSQL with a cursor. Interrupt it with Ctrl-C, a crash or a restart, then run the same command again (or press **Resume** in the console) to continue from the cursor.
- After the main copy, catch-up passes pick up blobs published while the job ran. When a pass copies nothing and no blob failed, the active backend switches in one database transaction. If any blob failed, nothing switches.
- Reads are served throughout: from the source until the switch, then from the target, falling back to the old store for any blob the target lacks.

`--throttle-mb-per-sec` limits copy bandwidth (0, the default, is unlimited).

### 3. Verify and clean up

The old store is left intact after the switch, and while it is retained another migration is refused. Verify the new backend (pulls, `ant storage status`, `GET /api/v1/storage/migration`), then remove the old copies:

```sh
ant storage cleanup --dry-run
ant storage cleanup
```

Cleanup verifies (and, if needed, repairs) every blob on the active backend before deleting its copy from the old one. Orphan bytes without a database row are not touched; scheduled garbage collection handles those, on the active backend only.

### Status

```sh
ant storage status
```

prints the active and retained backends and the latest job as JSON.

## CLI reference

```sh
ant storage status
ant storage migrate --to disk|s3 [--dry-run] [--throttle-mb-per-sec N]
ant storage cleanup [--dry-run]
```

| Error                                                                   | Meaning                                                    |
| ----------------------------------------------------------------------- | ---------------------------------------------------------- |
| `another storage job is running; wait for it or let it go idle for 90s` | Only one unfinished job per instance                       |
| `previous storage still retained; run ant storage cleanup first`        | Clean up the previous backend before migrating again       |
| `invalid target: it must be configured, differ from the active backend` | The target backend is not configured, or is already active |

The command is safe to run while the service is serving. The configured `ANT_INSTANCE_ID`, if set, must match the bootstrapped instance.

## API

| Endpoint                                                   | Purpose                                                                                                                                                                                                                                                          |
| ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /api/v1/storage/migration`                            | Active backend, whether the previous backend still holds data awaiting cleanup, and the latest job                                                                                                                                                               |
| `GET /api/v1/storage/migrations`                           | Recent migration and cleanup jobs, newest first                                                                                                                                                                                                                  |
| `POST /api/v1/storage/migrations`                          | Start a job in the background (202): `{"action":"migrate","to":"s3","dry_run":false,"throttle_bytes_per_second":52428800}` or `{"action":"cleanup","dry_run":true}`. An interrupted job of the same shape is resumed instead; one unfinished job at a time (409) |
| `GET /api/v1/storage/migrations/{id}`                      | Poll a job                                                                                                                                                                                                                                                       |
| `POST /api/v1/storage/migrations/{id}/pause`, `.../resume` | Pause at the next checkpoint (keeping the cursor), or resume a paused or abandoned job                                                                                                                                                                           |

All require instance admin and mutations require an `Idempotency-Key`.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
curl -fsS -X POST $ANT/api/v1/storage/migrations -H "$H" -H "$J" -H 'Idempotency-Key: migrate-s3-1' \
  -d '{"action":"migrate","to":"s3","dry_run":true}'
```

## Notes

- A job whose owner crashed is taken over after 90 seconds without a heartbeat.
- A blob committed between the final catch-up pass and the switch is still readable through the fallback, and is copied by cleanup before the old copy is deleted.
- Garbage collection aborts S3 multipart uploads that have been idle for longer than 24 hours and reports them as `stale_uploads_aborted`.
- In Kubernetes, switching to S3 does not remove the need for the data volume: upload staging stays on `ANT_DATA_DIR`.
