# Backup and restore

`ant backup` and `ant restore` capture and recover PostgreSQL metadata and the
blobs it references as one consistent unit (spec section 6.13). They use the same
environment as `ant serve` (`ANT_DATABASE_URL`, `ANT_ROOT_KEY_FILE`,
`ANT_STORAGE_BACKEND`, `ANT_DATA_DIR` / `ANT_S3_*`), so the blob store can be disk
or S3-compatible on either side, and source and target may use different backends.

## Commands

```sh
ant backup [--concurrency N] <new-or-empty-dir>
ant backup verify [--expect-manifest-sha256 HEX] [--concurrency N] <dir>
ant restore [--force] [--expect-manifest-sha256 HEX] [--concurrency N] <dir>
```

Each prints a JSON summary (schema version, table and row counts, blob count and
bytes, and `manifest_sha256`). Flags go before the directory. To ship a backup
as one file, tar the directory afterwards; `verify` and `restore` read directories.

## What a backup contains

```
manifest.json                  schema version + migration checksums, per-table row
                               counts/bytes/SHA-256, every blob (domain, digest, size)
tables/<table>.copy            PostgreSQL COPY text format
blobs/<dedup-domain>/<digest>  verbatim blob bytes
```

The manifest is written last, atomically; a directory without `manifest.json` is an
incomplete backup and is rejected. Output files are mode 0600 in a 0700 directory.

Excluded by design (short-lived security state, rebuilt in normal operation):
`browser_sessions`, `oidc_login_states`, `enterprise_login_states`,
`saml_replay_claims`, `instance_writer_leases`, `oci_uploads`. Consequences after a
restore: users sign in again, in-flight OCI uploads must restart, and the first
server to start acquires a fresh writer lease. Every other table, including ones
added by future migrations, is included automatically. Pending webhook outbox rows
are included and may be delivered again after restore (deliveries are at-least-once).

## Secrets and keys

- **Root keys are never in a backup.** Neither is any decrypted secret.
  `encrypted_secrets` rows are copied exactly as stored: envelope ciphertext plus
  wrapped data keys.
- **The operator must mount the same keyring at restore**: the root key that was
  active at backup time (`ANT_ROOT_KEY_FILE`) plus every previous version still
  used by any record (`ANT_PREVIOUS_ROOT_KEYS_FILE`). Back up the keyring
  separately, through your secret manager, never beside the data backup.
- Restore decrypts every restored secret before committing (and discards the
  plaintext). A wrong or incomplete keyring aborts the restore and rolls the
  tables back (`ErrKeyring`); fix the mount and run it again.
- Treat the backup as sensitive anyway: it holds token hashes, identity data and
  all artifacts.

## Consistency guarantees

- All tables are read in one `REPEATABLE READ`, read-only transaction, so metadata
  is a single point-in-time snapshot with no torn foreign-key relationships.
  The running service is not blocked and is not paused.
- The list of blobs comes from that same snapshot. Blobs not referenced at that
  moment (uploads in flight, orphans) are skipped; blobs committed afterwards are
  not part of the backup. Blobs are immutable and content-addressed, so copying
  them after the snapshot yields the right bytes.
- Every blob is hashed while it is copied and must match the digest and size in
  the snapshot. A missing or corrupt blob fails the backup (no partial backup is
  left behind) rather than producing a silently inconsistent one.
- Restore loads all tables in a single transaction, verifies each table file's
  digest and row count, then commits. Either all metadata is restored or none.
- Metadata is committed only after every blob has been written and verified, so a
  restored database never references a missing blob. If restore fails first,
  already written blobs are harmless unreferenced objects.

Requirement: do not run garbage collection or blob deletion while a backup is
running. The snapshot may reference a blob that a concurrent delete removes; the
backup then fails (it never skips silently) and must be repeated.

## Procedure

Back up (online):

1. `ant backup /backups/ant-2026-10-18` with the service's environment.
2. Record `manifest_sha256` from the output in a different system than the backup.
3. `ant backup verify --expect-manifest-sha256 <hex> /backups/ant-2026-10-18`
   after copying it to its final storage. This needs no database or keys.

Restore (offline):

1. Stop every Kuvryn Ant instance that used the old database. Do not point two
   deployments at the same database or bucket.
2. Provision an **empty** PostgreSQL 17/18 database and the empty target blob
   store (new disk directory or S3 bucket/prefix). Mount the original keyring.
3. Use an `ant` binary at the backup's schema version or newer.
4. `ant restore --expect-manifest-sha256 <hex> /backups/ant-2026-10-18`.
5. Start the service. `ANT_PUBLIC_URL` and other settings may differ from the
   source; users sign in again.

Restore steps: validate the manifest and its migration checksums against the
binary, verify table digests, check the target is empty, migrate to the backup's
schema version, copy and digest-verify blobs, load tables in one transaction
(reset identity sequences, check keyring), commit, then migrate forward to the
binary's current schema.

### Non-empty targets and `--force`

By default restore refuses any database that already contains tables in the
connection's schema. `--force` relaxes exactly one case: a schema that holds the
Kuvryn tables (for example because `ant serve` already migrated it) in which
every table has zero rows. Restore still refuses if any row exists, and never
overwrites or merges data. The blob store is not checked: blobs are written
content-addressed, an identical existing object is verified and reused, and a
different object under the same digest fails the restore.

## Rollback and version constraints

- Backups restore into a schema that is the backup's version, then upgrade
  forward. A binary older than the backup (schema newer than it knows) refuses.
  Migrations are append-only; if a migration's checksum differs from the one in the
  backup, restore refuses.
- Downgrade is not supported. To roll back an upgrade, restore the backup taken
  before the upgrade into a fresh database using the pre-upgrade binary, then
  accept the loss of writes since that backup. Take a backup immediately before
  every upgrade.
- Restore is not an in-place rollback of a live database. Switch traffic to the new
  database only after the summary reports success.
- A failed restore leaves an empty schema (plus orphan blobs) that can be retried
  with `--force`, or discard the database and start again.
- PostgreSQL text COPY is portable across the supported majors 17 and 18.

## Limits

- Single-snapshot dump on one connection; very large databases are bounded by one
  connection's throughput. Blob transfers run in parallel (`--concurrency`).
- The blob list for a backup is held in memory (about 150 bytes per blob).
- Backups are directories, not an encrypted container; encrypt at rest with your
  storage or tooling. `manifest_sha256` pinning, not a signature, detects wholesale
  replacement of a backup.
- Replication or object-store versioning is not a substitute: those do not give a
  cross-system consistent snapshot of database and blobs.
