# Upgrading

This page covers schema migrations, rollback limits, the supported upgrade
paths, and the Helm procedure (spec section 6.13, architecture decision A05).

## Schema migration behavior

- Migrations are embedded in the binary as contiguous, numbered SQL files
  (`internal/database/migrations/NNN_name.sql`). Each applied migration is
  recorded in `schema_migrations` with its name and SHA-256 checksum.
- The service applies all pending migrations at startup, before it serves
  traffic, in a single PostgreSQL transaction guarded by an advisory lock. If any
  migration fails, the whole transaction rolls back and the process exits; the
  previous schema is intact. Concurrent starters serialize on the lock.
- A history that is non-contiguous, has a checksum mismatch, or contains a
  version newer than the binary knows is refused. A binary never runs against a
  schema newer than itself.
- Migrations are forward-only. There are no down migrations.
- Large migrations run inside the startup window. The chart allows five minutes
  for startup (startup probe: 60 failures at 5 s intervals in
  `deploy/chart/templates/deployment.yaml`); rehearse on a restored copy and
  raise that threshold deliberately if a migration needs longer.
- Inspect state without changing anything with `ant diagnostics` (see
  `migrations.json`: applied vs binary version and pending count).

## Rollback constraints

- Rolling the image back does not undo migrations. A previous binary refuses to
  start on a newer schema. The supported rollback is therefore: stop the
  service, restore the pre-upgrade backup (PostgreSQL metadata and the blobs it
  references, with `ant restore` into an **empty** database; see below) and start the
  previous image. Anything written after the upgrade is lost.
- Never promise a rollback across a migration marked irreversible in the release
  notes (for example one that drops or rewrites data). Such releases state this
  explicitly and require the backup step below.
- The root key and previous-key manifest must be the same ones that protected
  the backup (see [backup and restore](backup-restore.md)).

## Supported-version policy

Documented upgrades are supported from the current and the preceding minor
release to the next release (architecture decision A05). Upgrading across more
than one minor requires stepping through each intermediate minor; skipping is
not tested and not supported. Patch releases within a minor are always safe to
apply in order. Breaking management API changes require a new major API version
and a migration guide; protocol compatibility follows the client matrix, not the
management API version. While only one minor exists, every upgrade within it is
in scope.

## Pre-upgrade checklist

1. Read the release notes for irreversible migrations and new required settings.
2. Take a consistent backup and verify it:

   ```sh
   ant backup --keep 3 s3://<bucket>/<prefix>/backups/pre-upgrade-$(date +%Y%m%d-%H%M)
   ant backup verify s3://<bucket>/<prefix>/backups/pre-upgrade-<date>
   ```

   **Never write the pre-upgrade backup onto the data PVC (`/var/lib/ant`).**
   With S3 blobs, `ant backup` copies metadata only (`--blobs auto` resolves to
   `reference`), but even so write it to object storage: an `s3://` location needs
   no local disk, and `ant backup` refuses a local target that would exceed 80%
   filesystem usage (`--max-disk-percent`). For Kubernetes run these with
   `kubectl exec deployment/ant-ant -- ant backup ...`. On disk deployments use a
   separate volume, not the data PVC. Do not run `ant gc` or retention sweeps
   between the backup and the end of the upgrade (a reference backup depends on
   the blobs staying in the bucket). CloudNativePG backups of PostgreSQL plus a
   metadata backup are an equivalent pre-upgrade safety net. Store the manifest
   SHA-256 outside the backup. Watch `ant_data_dir_bytes_free` (see
   [observability](observability.md)).

3. Run `ant diagnostics` and confirm database and storage checks pass and the
   applied schema version equals the running binary's version (no half-applied
   state).
4. Optionally run `ant repair metadata --dry-run` and resolve findings first, so
   a pre-existing inconsistency is not blamed on the upgrade.
5. Confirm the keyring (`ANT_ROOT_KEY_FILE`, `ANT_PREVIOUS_ROOT_KEYS_FILE`) is
   backed up separately.

## Helm upgrade procedure

The Deployment is a single replica with the `Recreate` strategy, so an upgrade
has downtime equal to drain time plus start and migration time.

```sh
helm diff upgrade ant deploy/chart -f YOUR_PROTECTED_VALUES_FILE   # optional plugin
helm lint deploy/chart
helm upgrade ant deploy/chart -f YOUR_PROTECTED_VALUES_FILE --wait --timeout 10m
kubectl rollout status deployment/ant-ant
kubectl logs deployment/ant-ant | grep -i "schema\|listening"
```

Verify after the rollout:

- `GET /health/ready` returns 200 and `GET /api/v1/status` (admin token) shows
  database, blob storage and a held writer lease.
- `ant diagnostics` reports `current: true` in `migrations.json`.
- Smoke a pull and a publish through one package protocol.

Do not run bootstrap again for an existing instance. If startup fails with
`schema migration N failed; transaction rolled back`, the schema is unchanged:
fix the cause (or roll the image back, which is safe because nothing was
applied) and retry. If it fails with `unsupported schema version`, the database
is newer than the image: deploy a newer image or restore the backup.

To roll back after a successful migration: `helm rollback` is not sufficient, because
the older image refuses the newer schema, and `ant restore` never overwrites data:
without `--force` it refuses any database that already holds tables, and `--force`
only accepts a Kuvryn schema in which every table has zero rows. The upgraded,
populated database therefore cannot be restored over. Restore into an empty
database instead:

1. Scale to zero (`kubectl scale deployment/ant-ant --replicas=0`).
2. Provision an empty database (a new database on the same PostgreSQL server, or the
   same one after dropping and recreating it / its schema), and point
   `ANT_DATABASE_URL` of the restore command at it. Keep the same blob store and the
   same keyring.
3. Restore the pre-upgrade backup with the **pre-upgrade** `ant` binary:
   `ant restore --expect-manifest-sha256 <hex> <backup>`. A `reference` backup needs
   its blobs to be present in the blob store; a `copy` backup writes them.
4. Point the release at the restored database (the `postgres.existingSecret` DSN),
   then `helm rollback ant <revision> --wait`.

This path has not been rehearsed end to end; rehearse it on a copy before relying on it.
See [backup and restore](backup-restore.md#non-empty-targets-and---force).

## Docker Compose

`docker compose pull && docker compose up -d` applies the same startup
migration. Take the backup step first; compose uses the same single-writer
model, so stop the old container before starting a new one.
