# Backup and restore

`ant backup` and `ant restore` capture and recover PostgreSQL metadata and the blobs it references as one consistent unit. They use the same environment as `ant serve` (`ANT_DATABASE_URL`, `ANT_ROOT_KEY_FILE`, `ANT_STORAGE_BACKEND`, `ANT_DATA_DIR` / `ANT_S3_*`), so the blob store can be disk or S3-compatible, and source and target may use different backends.

## Commands

```sh
ant backup [--concurrency N] [--blobs auto|copy|reference] [--keep N] [--max-age DURATION] \
           [--max-disk-percent P] <new-or-empty-dir | s3://bucket/prefix/backups/name>
ant backup verify [--expect-manifest-sha256 HEX] [--check-blobs] [--concurrency N] <dir | s3://...>
ant restore [--force] [--expect-manifest-sha256 HEX] [--concurrency N] <dir | s3://...>
```

| Flag                           | Default   | Meaning                                                                                                      |
| ------------------------------ | --------- | ------------------------------------------------------------------------------------------------------------ |
| `--concurrency N`              | `4`       | Parallel blob transfers                                                                                      |
| `--blobs`                      | `auto`    | `auto`, `copy` or `reference` (see [Blob modes](#blob-modes))                                                |
| `--keep N`                     | `0` (off) | Keep the newest N sibling backups next to the new one                                                        |
| `--max-age DURATION`           | `0` (off) | Also remove sibling backups older than this, for example `168h`                                              |
| `--max-disk-percent P`         | `80`      | Refuse to write into a local directory whose filesystem is, or would become, more than P% full; `0` disables |
| `--expect-manifest-sha256 HEX` | none      | Refuse a backup whose manifest digest differs (detects wholesale replacement)                                |
| `--check-blobs`                | off       | `verify` only: re-check that blobs referenced by a `reference` backup still exist                            |
| `--force`                      | off       | `restore` only: accept a schema that already has the Kuvryn tables but zero rows                             |

Flags go before the location. Each command prints a JSON summary (schema version, table and row counts, blob count and bytes, `blob_mode` and `manifest_sha256`). To ship a backup as a single file, tar the directory afterwards; `verify` and `restore` read directories and object-storage prefixes.

In Kubernetes run the commands inside the running pod, which already has the right environment:

```sh
kubectl exec deployment/ant-ant -- ant backup --keep 5 s3://backups-bucket/ant/backups/ant-$(date +%Y%m%d-%H%M)
```

## Where to write a backup

::: danger Never back up onto the data volume
Do not write a backup onto the data PVC (`/var/lib/ant`) or any volume the service needs to stay alive. A copy-mode backup of an object-storage deployment downloads every referenced blob and can fill the volume.
:::

- **Object storage (`s3://bucket/prefix/backups/<name>`)**, preferred for pre-upgrade backups. The backup is written directly with the instance's own `ANT_S3_*` endpoint, region, CA and credentials file; the bucket in the URL may differ from the blob bucket. It is streamed in multipart uploads and nothing touches local disk.
- **A separate volume or directory** for disk deployments, mounted outside the data directory.

`--max-disk-percent` checks the target filesystem before writing and again once the blob volume is known, and leaves nothing behind when it refuses.

## Blob modes

| `--blobs`        | Behaviour                                                                                                                                                       |
| ---------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `auto` (default) | `reference` when the active blob backend is S3, otherwise `copy`                                                                                                |
| `copy`           | Blob bytes are copied into the backup: self-contained, and as large as the referenced content                                                                   |
| `reference`      | Metadata (`tables/`) plus the blob list in `manifest.json`; no bytes are copied. Every blob is checked to exist with the recorded size when the backup is taken |

The metadata of a `reference` backup is one consistent snapshot, and blobs are immutable and content-addressed, so a referenced blob is either present with exactly the recorded bytes or absent. Restore therefore checks, before touching the database, that every referenced blob exists in the target store (same bucket and prefix as the source) with the recorded size, and refuses otherwise.

What `reference` cannot guarantee is that the blobs still exist later: a blob deleted by garbage collection or retention after the snapshot is gone from every reference backup that needs it.

- Use `reference` for short-lived safety backups, for example before an upgrade, and do not run `ant gc` or let retention sweep between the backup and the end of the upgrade window.
- For long-term archives use `--blobs copy` to an `s3://` location, or enable bucket versioning.
- `ant backup verify --check-blobs` re-checks that the referenced blobs still exist.

## Retention of backups

`--keep N` keeps the newest N backups next to the new one (same parent directory or object prefix); `--max-age 168h` also removes backups older than that. The backup just created is never removed, and only siblings with a valid, complete `manifest.json` are candidates. Retention runs after the new backup succeeded; a retention error is logged but does not fail the backup.

```sh
ant backup --keep 3 s3://backups-bucket/ant/backups/pre-upgrade-$(date +%Y%m%d-%H%M)
```

## What a backup contains

```text
manifest.json                  schema version + migration checksums, per-table row
                               counts/bytes/SHA-256, every blob (domain, digest, size),
                               blob_mode ("copy" or "reference")
tables/<table>.copy            PostgreSQL COPY text format
blobs/<dedup-domain>/<digest>  verbatim blob bytes (copy mode only)
```

In object storage the same names are object keys under the backup prefix. The manifest is written last and atomically; a directory without `manifest.json` is an incomplete backup and is rejected. Local output files are mode 0600 in a 0700 directory.

Excluded by design (short-lived security state, rebuilt in normal operation): `browser_sessions`, `oidc_login_states`, `enterprise_login_states`, `saml_replay_claims`, `instance_writer_leases`, `oci_uploads`. After a restore, users sign in again, in-flight OCI uploads must restart, and the first server to start acquires a fresh writer lease. Every other table, including tables added by future migrations, is included automatically. Pending webhook outbox rows are included and may be delivered again after a restore (deliveries are at-least-once).

## Secrets and keys

- **Root keys are never in a backup**, and neither is any decrypted secret. `encrypted_secrets` rows are copied exactly as stored: envelope ciphertext plus wrapped data keys.
- **Mount the same keyring at restore**: the root key that was active at backup time (`ANT_ROOT_KEY_FILE`) plus every previous version still used by any record (`ANT_PREVIOUS_ROOT_KEYS_FILE`). Back up the keyring separately, in your secret manager, never beside the data backup. See [Root key](../install/root-key.md).
- Restore decrypts every restored secret before committing (and discards the plaintext). A wrong or incomplete keyring aborts the restore and rolls the tables back; fix the mount and run it again.
- Treat a backup as sensitive anyway: it holds token hashes, identity data and all artifacts. Backups are not an encrypted container; encrypt them at rest with your storage or tooling.

## Consistency guarantees

- All tables are read in one `REPEATABLE READ`, read-only transaction, so the metadata is a single point-in-time snapshot. The running service is neither blocked nor paused.
- The blob list comes from the same snapshot. Blobs not referenced at that moment (uploads in flight, orphans) are skipped; blobs committed afterwards are not part of the backup.
- Every copied blob is hashed while it is copied and must match the snapshot's digest and size. A missing or corrupt blob fails the backup (no partial backup is left behind).
- Restore loads all tables in a single transaction, verifies each table file's digest and row count, then commits: either all metadata is restored or none.
- Metadata is committed only after every blob has been written and verified, so a restored database never references a missing blob. If a restore fails first, the blobs already written are harmless unreferenced objects.

::: warning
Do not run garbage collection or blob deletion while a backup is running. If a concurrent delete removes a blob the snapshot references, the backup fails (it never skips silently) and must be repeated.
:::

## Procedure

### Back up (online)

1. Run the backup with the service's environment:

   ```sh
   ant backup --keep 5 s3://backups-bucket/ant/backups/ant-2026-10-20
   ```

2. Record `manifest_sha256` from the output in a different system than the backup.
3. Verify the backup in its final location:

   ```sh
   ant backup verify --expect-manifest-sha256 <hex> s3://backups-bucket/ant/backups/ant-2026-10-20
   ```

   A directory verify needs no database or keys; an `s3://` verify needs the `ANT_S3_*` settings.

As an alternative for the database, rely on your PostgreSQL backup tooling (for example CloudNativePG) and take `--blobs reference` backups for the consistent metadata plus blob list.

### Restore (offline)

1. Stop every Kuvryn Ant instance that used the old database. Never point two deployments at the same database or bucket.
2. Provision an **empty** PostgreSQL 17 or 18 database and the target blob store (a new disk directory or S3 bucket/prefix). Mount the original keyring.
3. Use an `ant` binary at the backup's schema version or newer.
4. Restore:

   ```sh
   ant restore --expect-manifest-sha256 <hex> s3://backups-bucket/ant/backups/ant-2026-10-20
   ```

   A `reference` backup needs the blobs to be present in the target blob store; a `copy` backup writes them.

5. Start the service. `ANT_PUBLIC_URL` and other settings may differ from the source; users sign in again.

Internally, restore validates the manifest and its migration checksums against the binary, verifies table digests, checks that the target is empty, migrates to the backup's schema version, copies and digest-verifies blobs, loads the tables in one transaction (resetting identity sequences and checking the keyring), commits, and then migrates forward to the binary's current schema.

### Non-empty targets and `--force`

By default restore refuses any database that already contains tables in the connection's schema. `--force` relaxes exactly one case: a schema that holds the Kuvryn tables (for example because `ant serve` already migrated it) in which every table has zero rows. Restore still refuses if any row exists, and never overwrites or merges data. The blob store is not checked for emptiness: blobs are written content-addressed, an identical existing object is verified and reused, and a different object under the same digest fails the restore.

## Rollback and version constraints

- A backup restores into its own schema version and is then upgraded forward. A binary older than the backup refuses. If a migration's checksum differs from the one recorded in the backup, restore refuses.
- Downgrades are not supported. To roll back an upgrade, restore the backup taken before the upgrade into a fresh database with the pre-upgrade binary and accept the loss of writes made since. See [Upgrade](./upgrade.md).
- Restore is not an in-place rollback of a live database. Switch traffic to the new database only after the summary reports success.
- A failed restore leaves an empty schema (plus orphan blobs) that can be retried with `--force`, or you can discard the database and start again.
- PostgreSQL text COPY is portable across the supported majors 17 and 18.

::: warning Limits

- The dump is a single snapshot on one connection, so very large databases are bounded by one connection's throughput. Blob transfers run in parallel (`--concurrency`).
- The blob list is held in memory (about 150 bytes per blob).
- `manifest_sha256` pinning, not a signature, detects wholesale replacement of a backup.
- Replication and object-store versioning are not substitutes for a backup: neither gives a consistent snapshot across database and blobs.
- Restore throughput has not yet been qualified on large data sets.
  :::
