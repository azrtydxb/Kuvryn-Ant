# Upgrade

This page covers how schema migrations behave, what can and cannot be rolled back, the supported upgrade paths, and the Helm and Docker Compose procedures.

::: info Release status
Kuvryn Ant is at version 0.1.0 and has no published release image or release notes yet. The procedure below is the supported one for upgrading between builds of the same deployment; it will apply unchanged once releases are published.
:::

## Schema migrations

- Migrations are embedded in the binary as contiguous, numbered SQL files. Each applied migration is recorded in `schema_migrations` with its name and SHA-256 checksum.
- The service applies all pending migrations at startup, **before** it opens the HTTP listener, in a single PostgreSQL transaction guarded by an advisory lock. If any migration fails, the whole transaction rolls back and the process exits; the previous schema is intact. Concurrent starters serialize on the lock.
- A migration history that is non-contiguous, has a checksum mismatch, or contains a version newer than the binary knows is refused. A binary never runs against a schema newer than itself.
- Migrations are forward-only. There are no down migrations.
- Large migrations run inside the startup window. The chart allows five minutes for startup (startup probe: 60 failures at 5 s intervals). Rehearse on a restored copy and raise that threshold deliberately if a migration needs longer.
- Inspect the schema state without changing anything with `ant diagnostics`: `migrations.json` in the bundle shows the applied version, the binary's version and the number of pending migrations. See [Diagnostics and repair](./diagnostics.md).

## Rollback constraints

- Rolling back the image does not undo migrations, and a previous binary refuses to start on a newer schema. The supported rollback is: stop the service, restore the pre-upgrade backup (metadata and the blobs it references) with `ant restore`, and start the previous image. Anything written after the upgrade is lost.
- A release whose migration drops or rewrites data will say so explicitly in its release notes and require the backup step below.
- The root key and previous-key file must be the ones that protected the backup. See [Backup and restore](./backup-restore.md#secrets-and-keys).

## Supported upgrade paths

Upgrades are supported from the current and the preceding minor release to the next release. Upgrading across more than one minor requires stepping through each intermediate minor; skipping minors is not tested and not supported. Patch releases within a minor are always safe to apply in order. Breaking management API changes require a new major API version and a migration guide; package-protocol compatibility follows the client matrix, not the management API version. While only one minor exists, every upgrade within it is in scope.

## Pre-upgrade checklist

1. Read the release notes for irreversible migrations and new required settings.
2. Take a consistent backup to object storage, and verify it:

   ```sh
   ant backup --keep 3 s3://backups-bucket/ant/backups/pre-upgrade-$(date +%Y%m%d-%H%M)
   ant backup verify --expect-manifest-sha256 <hex> s3://backups-bucket/ant/backups/pre-upgrade-<date>
   ```

   In Kubernetes: `kubectl exec deployment/ant-ant -- ant backup ...`. Never write the pre-upgrade backup onto the data PVC (`/var/lib/ant`). With S3 blobs `--blobs auto` resolves to `reference` (metadata only), but write it to object storage anyway; `ant backup` refuses a local target that would exceed 80% filesystem usage. On disk deployments use a separate volume.

   Do not run `ant gc` or let retention sweep between the backup and the end of the upgrade: a reference backup depends on the blobs staying in the bucket. Store `manifest_sha256` outside the backup. Watch `ant_data_dir_bytes_free` (see [Observability](./observability.md)).

3. Run `ant diagnostics` and confirm the database and storage checks pass and the applied schema version equals the running binary's version.
4. Optionally run `ant repair metadata --dry-run` and resolve its findings first, so a pre-existing inconsistency is not blamed on the upgrade.
5. Confirm the keyring (`ANT_ROOT_KEY_FILE`, `ANT_PREVIOUS_ROOT_KEYS_FILE`) is backed up separately.

## Helm upgrade

The chart runs a single replica with the `Recreate` strategy (one writer per instance), so an upgrade has downtime equal to the drain time plus start and migration time.

```sh
helm diff upgrade ant deploy/chart -f values-production.yaml   # optional plugin
helm lint deploy/chart
helm upgrade ant deploy/chart -f values-production.yaml --wait --timeout 10m
kubectl rollout status deployment/ant-ant
kubectl logs deployment/ant-ant | grep -i "schema\|listening"
```

Verify after the rollout:

- `GET /health/ready` returns 200, and `GET /api/v1/status` with an admin token shows the database, blob storage and a held writer lease.
- `ant diagnostics` reports `current: true` in `migrations.json`.
- A pull and a publish through one package protocol succeed.

Do not run `ant bootstrap` again for an existing instance.

| Startup failure                                      | Meaning                              | Action                                                                           |
| ---------------------------------------------------- | ------------------------------------ | -------------------------------------------------------------------------------- |
| `schema migration N failed; transaction rolled back` | Nothing was applied                  | Fix the cause, or roll the image back (safe, the schema is unchanged), and retry |
| `unsupported schema version`                         | The database is newer than the image | Deploy a newer image, or restore the backup                                      |

### Rolling back after a successful migration

`helm rollback` alone is not sufficient, because the older image refuses the newer schema.

1. Scale to zero: `kubectl scale deployment/ant-ant --replicas=0`.
2. Restore the pre-upgrade backup with the **pre-upgrade** `ant` binary into an empty database (a new database, or the same one after dropping its schema). `ant restore --force` accepts a schema that has the Kuvryn tables but no rows; it refuses any database that still contains data.
3. `helm rollback ant <revision> --wait`.

::: warning Gap
Running `ant restore --force` directly against the upgraded, populated database does not work: restore never overwrites rows, so the database (or its schema) must be emptied or replaced first. This rollback path has not been rehearsed end to end; rehearse it on a copy before relying on it.
:::

## Docker Compose

```sh
docker compose pull
docker compose up -d
```

This applies the same startup migration. Take the backup first. Compose uses the same single-writer model: stop the old container before a new one starts against the same database.
