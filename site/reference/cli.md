# CLI reference

The `ant` binary is both the server and a set of host-side maintenance commands. Every command reads the same `ANT_*` environment as the server (see [Configuration](./configuration.md)); most need the database (`ANT_DATABASE_URL`), and some need the storage settings or the root keyring. Commands are authorized by possession of that configuration, not by an API token: anyone who can run them already controls the deployment.

```text
ant [serve | bootstrap [name] | bootstrap-token | rotate-secrets | gc [--dry-run] | storage | diagnostics [-o FILE]
     | repair metadata|oci-index-children [--dry-run] | backup | restore | admin break-glass|local-user
     | export | import | migrate | compare | federation status|re-identify|purge|recovery]
```

Output is JSON on standard output; logs and errors are JSON on standard error. A usage error exits with status 1.

On Kubernetes run commands inside the Ant pod, which has the configuration mounted, for example `kubectl -n ant exec -it deploy/ant -- ant gc --dry-run` (names depend on your Helm release).

Flags use Go's flag syntax: `--flag value`, `--flag=value` and `-flag` are equivalent.

## Commands that change schema

`serve`, `bootstrap`, `bootstrap-token`, `rotate-secrets`, `gc`, `storage`, `admin`, `import`, `migrate`, `compare` and `federation status|re-identify|purge` apply pending database migrations before they run. `backup`, `export`, `diagnostics` and `repair` never migrate (`repair` refuses to run unless the schema matches the binary). `restore` migrates only after its own emptiness check.

## serve

```sh
ant            # same as: ant serve
ant serve
```

Runs the HTTP server on `ANT_LISTEN`. Serving artifacts on a non-loopback address requires `ANT_PUBLIC_URL`. A server started on an empty database serves only the management plane and starts serving artifacts by itself, without a restart, once `ant bootstrap` has run.

## bootstrap

```sh
ant bootstrap [name]   # name defaults to "local"
ant bootstrap-token
```

`bootstrap` initializes an empty database: the instance, the service principal `bootstrap-admin` with a full-grant `bootstrap-administrator` role, and a 24-hour service token printed once:

```json
{
  "instance_id": "<UUID>",
  "principal_id": "<UUID>",
  "token_id": "<UUID>",
  "token": "ant_<43 characters>",
  "expires_at": "<RFC 3339>"
}
```

It fails if an instance already exists. `name` is 1 to 128 characters. `bootstrap-token` issues a new 24-hour token for `bootstrap-admin` with the same output. See [Service accounts and tokens](../identity/tokens.md#bootstrap-token).

## rotate-secrets

```sh
ant rotate-secrets
```

Re-encrypts stored secrets (remote-source credentials, provider secrets and the like) under the current root key, in one transaction of at most 100 records, and prints `{"rotated":N,"batch_limit":100}`. Run it repeatedly until `rotated` is 0 after a root key rotation; records still encrypted under an old key version are the work queue, so an interrupted run simply resumes. A 0 result is not proof of completion while a running server holds row locks, so stop other workers and confirm before retiring an old key. Requires the root keyring (`ANT_ROOT_KEY_FILE`, plus `ANT_PREVIOUS_ROOT_KEYS_FILE` holding the old keys). See [Root key](../install/root-key.md).

## gc

```sh
ant gc [--dry-run]
```

Runs one retention, garbage-collection and integrity pass and prints the JSON report. With `--dry-run` nothing is modified and the report shows exactly what a real run would do. Safe while the server is running (deletion relies on row locks and mark/sweep grace periods). See [Storage maintenance](../operations/storage-maintenance.md).

## storage

```sh
ant storage status
ant storage migrate --to disk|s3 [--dry-run] [--throttle-mb-per-sec N]
ant storage cleanup [--dry-run]
```

| Subcommand | Effect                                                                                                                                     |
| ---------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `status`   | Active backend and the current or last storage job                                                                                         |
| `migrate`  | Copies blobs to the other configured backend and switches the active backend. The target must be configured and differ from the active one |
| `cleanup`  | Deletes blobs from the previous backend after a migration                                                                                  |

| Flag                      | Meaning                                                                        |
| ------------------------- | ------------------------------------------------------------------------------ |
| `--to disk\|s3`           | Target backend (`migrate` only, required)                                      |
| `--dry-run`               | Report without copying or deleting (`migrate`, `cleanup`)                      |
| `--throttle-mb-per-sec N` | Copy rate limit in MiB/s, 0 (default) for none, up to 1048576 (`migrate` only) |

Safe while serving. Ctrl-C pauses the job; running the same command again resumes from the persisted cursor. Only one storage job runs at a time (a stale job is taken over after 90 s idle), and a new migration is refused until the previous backend was cleaned up. See [Storage migration](../operations/storage-migration.md).

## diagnostics

```sh
ant diagnostics [-o FILE|-]
```

Writes a redacted support bundle (`.tar.gz`). Default file name `ant-diagnostics-<UTC timestamp>.tar.gz` in the current directory (mode 0600); `-o -` writes the archive to standard output. It works even when the database or storage is unreachable (they become failed checks) and never migrates. The error log in the bundle covers only this short-lived process; use the management API for the running server's recent errors. See [Diagnostics and repair](../operations/diagnostics.md).

## repair

```sh
ant repair metadata [--dry-run]
ant repair oci-index-children [--dry-run]
```

| Subcommand           | Effect                                                                                                                                                                                     |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `metadata`           | Finds orphaned or inconsistent metadata rows and fixes them in one transaction. Deletes only rows already unreachable through foreign keys; blobs are left to `gc`. Run during low traffic |
| `oci-index-children` | Links per-platform manifests of OCI image indexes stored before that grouping was recorded at publish time. Idempotent, reads only stored manifest bytes, never contacts an upstream       |

Both print a JSON report; `--dry-run` changes nothing. Both refuse to run unless the database schema matches this binary.

## backup and restore

```text
ant backup [--concurrency N] [--blobs auto|copy|reference] [--keep N] [--max-age DURATION]
           [--max-disk-percent P] <new-or-empty-dir | s3://bucket/prefix/backups/name>
ant backup verify [--expect-manifest-sha256 HEX] [--check-blobs] <dir | s3://...>
ant restore [--force] [--expect-manifest-sha256 HEX] [--concurrency N] <dir | s3://...>
```

| Flag                            | Commands                   | Meaning                                                                                                                                                                                                                     |
| ------------------------------- | -------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--concurrency N`               | all                        | Parallel workers, 1 to 64, default 4                                                                                                                                                                                        |
| `--blobs auto\|copy\|reference` | `backup`                   | `copy` copies blob bytes into the backup; `reference` lists blobs in the manifest and checks each exists with the right size, copying nothing; `auto` (default) uses `reference` when the active backend is S3, else `copy` |
| `--keep N`                      | `backup`                   | After a successful backup, keep only the newest N sibling backups. Needs a location inside a directory of backups                                                                                                           |
| `--max-age DURATION`            | `backup`                   | After a successful backup, delete sibling backups older than this (Go duration, for example `720h`)                                                                                                                         |
| `--max-disk-percent P`          | `backup`                   | Refuse to write into a local directory whose filesystem is or would become fuller than P percent (0 to 100, default 80)                                                                                                     |
| `--expect-manifest-sha256 HEX`  | `backup verify`, `restore` | Require this manifest digest                                                                                                                                                                                                |
| `--check-blobs`                 | `backup verify`            | Also check the referenced blobs (needs the storage settings)                                                                                                                                                                |
| `--force`                       | `restore`                  | Allow restoring into a schema that already has the Kuvryn tables, as long as every one is empty. Never overwrites data                                                                                                      |

An `s3://` location uses the configured S3 settings (`ANT_S3_REGION`, `ANT_S3_BUCKET`, `ANT_S3_CREDENTIALS_FILE`). `backup verify` of a local directory without `--check-blobs` needs no configuration at all. `restore` checks every restored secret against the mounted keyring before committing, so mount the keyring (`ANT_ROOT_KEY_FILE`, `ANT_PREVIOUS_ROOT_KEYS_FILE`) that protected the source deployment. See [Backup and restore](../operations/backup-restore.md).

## admin

```sh
ant admin break-glass [--password-stdin]
ant admin local-user USERNAME [--admin] [--password-stdin]
```

Both need the database and the root keyring, refuse to run before `ant bootstrap`, and work with the server stopped or SSO broken.

- `break-glass` creates or resets the emergency administrator: replaces the password, clears the lockout, removes TOTP and revokes its sessions. Without `--password-stdin` a password is generated and printed once; with it, the first line of standard input is used (minimum 14 characters). Output: `{"username":"break-glass","created":...,"mfa":"disabled","sessions":"revoked","lockout":"cleared"[,"password":...]}`. See [Break-glass access](../operations/break-glass.md).
- `local-user` creates a username/password account (with `--admin`, also the full-grant `local-administrator` role) or, if it exists, resets its password. `--admin` is rejected for an existing account. See [Local users](../identity/local-users.md).

## export, import, migrate, compare

```text
ant export [--feed SLUG]... [--namespace NS]... [--no-content] <new-or-empty-dir>
ant export verify [--expect-manifest-sha256 HEX] <dir>
ant import [--apply] [--expect-manifest-sha256 HEX] <dir>
ant migrate nexus --url URL [--user NAME] [--repo NAME]... [--copy-content] [--no-count] [--apply]
ant compare nexus --url URL --coordinates FILE
```

| Flag                           | Commands                  | Meaning                                                                                  |
| ------------------------------ | ------------------------- | ---------------------------------------------------------------------------------------- |
| `--feed SLUG`                  | `export`                  | Export only this feed; repeatable. Default: all feeds                                    |
| `--namespace NS`               | `export`                  | Export only this namespace; repeatable                                                   |
| `--no-content`                 | `export`                  | Metadata only, no blob bytes                                                             |
| `--expect-manifest-sha256 HEX` | `export verify`, `import` | Require this bundle manifest digest                                                      |
| `--apply`                      | `import`, `migrate`       | Make changes. Without it the command is a dry run that only reports                      |
| `--url URL`                    | `migrate`, `compare`      | Nexus base URL (required)                                                                |
| `--user NAME`                  | `migrate`, `compare`      | Nexus username                                                                           |
| `--repo NAME`                  | `migrate`                 | Migrate only this Nexus repository; repeatable                                           |
| `--copy-content`               | `migrate`                 | Copy artifact bytes, not only metadata                                                   |
| `--no-count`                   | `migrate`                 | Skip counting assets in the source repositories                                          |
| `--coordinates FILE`           | `compare`                 | Lines of `<repository> <path> [<feed>]`; blank lines and `#` comments ignored (required) |

Nexus credentials are read at run time from `ANT_NEXUS_PASSWORD` or `ANT_NEXUS_TOKEN` and are never stored. `export verify` needs no configuration. `import` and `migrate` exit with status 2 when integrity failures were found. See [Import, export, Nexus migration](../operations/import-export.md).

## federation

```text
ant federation status
ant federation re-identify --confirm
ant federation purge --confirm
ant federation recovery sign-rotation --template FILE --key FILE --new-key-out FILE
ant federation recovery sign-authority-cert --template FILE --key FILE [--rotation FILE]
ant federation recovery sign-challenge --challenge FILE --key FILE
```

| Subcommand    | Effect                                                                                                                                                                                |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `status`      | Local federation view from the database (indented JSON)                                                                                                                               |
| `re-identify` | Gives a restored copy a new instance ID before it joins a federation and resets its federation state; re-seals secrets, so it needs the root keyring. Update `ANT_INSTANCE_ID` if set |
| `purge`       | Deletes federation rows after leaving a federation, so a downgrade is possible                                                                                                        |
| `recovery`    | Offline recovery-key signer (no database, no network); the recovery key never reaches a server                                                                                        |

`re-identify` and `purge` require `--confirm` and refuse while a serving process holds the writer lease. If `ANT_INSTANCE_ID` is set it must match the database.

Recovery flags: `--template` (rotation or recovery template JSON from the API), `--key` (recovery key file, `ant-recovery-v1:...`; keys are never accepted on the command line), `--new-key-out` (file to create for the new key, `sign-rotation` only), `--rotation` (output of `sign-rotation` to merge, `sign-authority-cert` only), `--challenge` (drill challenge JSON). `-` for `--template`, `--rotation` or `--challenge` reads standard input (not both `--template` and `--rotation`). The signed result goes to standard output.

| Procedure                 | Steps                                                                                                                                                                                                                                        |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Recovery-key rotation     | `GET /api/v1/federation/recovery-key/rotation` on the authority, then `sign-rotation` with the current key, then `sign-authority-cert` with the new key and `--rotation`, then `POST` the result to `/api/v1/federation/recovery-key/rotate` |
| Forced authority recovery | `POST /api/v1/federation/authority/recovery-template` on the member taking over, then `sign-authority-cert` with the recovery key, then `POST` the result plus `"confirm": true` to `/api/v1/federation/authority/recover`                   |
| Recovery-key drill        | `POST /api/v1/federation/recovery-key/challenge`, then `sign-challenge`, then `POST` the proof to `/api/v1/federation/recovery-key/verify`                                                                                                   |

See [Federation](../replication/federation.md) and [Federation runbooks](../replication/federation-runbooks.md).
