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
  references, with `ant restore`) and start the previous image. Anything
  written after the upgrade is lost.
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
   ant backup /backups/ant-pre-upgrade-$(date +%Y%m%d)
   ant backup verify /backups/ant-pre-upgrade-<date>
   ```

   For Kubernetes run these with `kubectl exec deployment/ant-ant -- ant backup ...`
   against a volume that outlives the pod, and also snapshot PostgreSQL and the
   data PVC if your platform supports it. Store the manifest SHA-256 outside the
   backup.
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

To roll back after a successful migration: `helm rollback` is not sufficient. Scale
to zero (`kubectl scale deployment/ant-ant --replicas=0`), restore the backup
with `ant restore --force` into the same PostgreSQL and blob store, then
`helm rollback ant <revision> --wait`.

## Docker Compose

`docker compose pull && docker compose up -d` applies the same startup
migration. Take the backup step first; compose uses the same single-writer
model, so stop the old container before starting a new one.
