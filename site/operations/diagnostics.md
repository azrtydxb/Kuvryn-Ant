# Diagnostics and repair

This page covers the health endpoints, the redacted support bundle (`ant diagnostics` and `GET /api/v1/diagnostics`), and the two repair commands: `ant repair metadata` and `ant repair oci-index-children`.

## Health endpoints

| Endpoint             | Auth            | Answers                                                                                                                                                                                                                              |
| -------------------- | --------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `GET /health/live`   | none            | `200 {"status":"alive"}` while the process runs. It bypasses the admission limit, so a busy server is not restarted by Kubernetes                                                                                                    |
| `GET /health/ready`  | none            | `200 {"status":"ready"}` when PostgreSQL answers a ping within 2 seconds and the process is not stopping; otherwise `503 {"status":"not_ready"}`                                                                                     |
| `GET /api/v1/status` | optional bearer | Anonymous callers get name and version only. With an instance-admin token the response also carries the readiness of the database, blob storage and writer lease, and a `warnings` list (for example `storage_pressure_unevictable`) |

The chart uses `/health/live` for the startup and liveness probes and `/health/ready` for the readiness probe. The HTTP listener opens only after startup migrations have completed, so the startup probe also covers the migration window. Each failed readiness check increments `ant_database_readiness_failures_total`.

Other operational views for administrators: `GET /api/v1/source-health` and `GET /api/v1/sources/{id}/health` (upstream health of remote sources), `GET /api/v1/metrics/traffic`, `GET /api/v1/storage/usage`, `GET /api/v1/retention/pressure`, `GET /api/v1/replication/status`. The console health page shows the same data. See [Observability](./observability.md).

## Diagnostics bundle

The diagnostics bundle is a redacted `tar.gz` for support cases.

```sh
ant diagnostics                  # writes ant-diagnostics-<timestamp>.tar.gz (mode 0600)
ant diagnostics -o bundle.tgz    # explicit path
ant diagnostics -o - > b.tgz     # write to stdout
curl -fsS -H "Authorization: Bearer $ANT_TOKEN" -o bundle.tgz https://ant.example.com/api/v1/diagnostics
```

The CLI uses the same environment as `ant serve`. It never migrates, and it still produces a bundle when PostgreSQL or the blob store is unreachable (those show up as failed checks); if the configuration itself is rejected, the bundle records the load error. When writing to a file it prints `{"path": ..., "bytes": ...}`.

The API endpoint requires instance admin, is audited (`diagnostics.download`) and additionally includes the running service's readiness detail and its in-memory recent errors. The CLI's error log only covers its own short-lived process, so prefer the API (or `kubectl logs`) for incident evidence.

| File              | Content                                                                       |
| ----------------- | ----------------------------------------------------------------------------- |
| `build.json`      | Version, VCS revision, Go version, OS/arch                                    |
| `config.json`     | Effective configuration (allowlisted fields only)                             |
| `health.json`     | Readiness detail: database, blob storage, writer lease                        |
| `migrations.json` | Applied schema version vs the binary's version, pending count, `current`      |
| `checks.json`     | Database ping and PostgreSQL version; blob backend reachability, with timings |
| `errors.log`      | Most recent WARN/ERROR log records (JSON lines)                               |
| `runtime.json`    | Goroutines, heap, GC and CPU counts                                           |
| `goroutines.txt`  | Full goroutine dump                                                           |

### Redaction

Redaction is layered:

1. The configuration is built from an allowlist. The database password, root key and storage credentials are replaced by `[REDACTED]`; credential file paths are shown without their contents.
2. Every file is then scrubbed for literal secret values (database password, the root key in raw, hex and base64 forms), `postgres://user:password@` URLs, `Bearer` and `Basic` credentials, `ant_` API tokens, and `password`, `secret`, `token`, `api_key`, `authorization`, `credential` key/value pairs. A unit test seeds each of these and fails if any survives in the archive.

::: warning
Redaction is best-effort defence in depth. Review a bundle before sharing it outside your organization, and never paste secret material into free-text fields that end up in logs.
:::

## `ant repair metadata`

```sh
ant repair metadata --dry-run   # report only
ant repair metadata             # fix everything fixable in one transaction
```

The command checks for orphaned or inconsistent metadata rows and prints a JSON report of every check, including those that found nothing (`check`, `description`, `found`, `fixed`, a `sample` of affected IDs, and `report_only` for checks it never changes), plus `total_found` and `total_fixed`.

| Check                             | Fix                                                                                                           |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| `tags_dangling`                   | Tags whose component or version row is missing or mismatched are deleted                                      |
| `subjects_without_asset`          | Artifact subject links whose referrer asset is missing are deleted                                            |
| `subjects_component_mismatch`     | Subject links recorded against a different component than their asset's are corrected                         |
| `assets_without_blob`             | Assets whose blob metadata row is missing are deleted                                                         |
| `assets_without_version`          | Assets whose version row is missing are deleted                                                               |
| `assets_without_component`        | Assets of versions whose component row is missing are deleted                                                 |
| `versions_without_component`      | Versions whose component row is missing are deleted                                                           |
| `versions_without_assets`         | Versions with no assets, older than one hour (in-flight publishes are untouched), are deleted with their tags |
| `components_without_versions`     | Components with no versions are deleted                                                                       |
| `stale_integrity_findings`        | Open integrity findings for blobs that no longer exist are marked resolved                                    |
| `assets_with_missing_blob_object` | Report only: assets whose blob object is missing from storage according to the integrity scan                 |

Behaviour:

- It refuses to run if the schema does not match the binary (`schema does not match this binary; start the service (or upgrade) first`), and it never migrates.
- It never deletes blob objects; garbage collection owns those.
- Assets whose blob object is missing are reported but not changed: restore the object from a backup or a replica.
- A dry run reports the current state, so a cascade (for example a component that becomes empty only after its last assetless version is removed) may show up only in the apply report.
- If any step fails, nothing is committed.

Take a backup first and run it during low traffic.

## `ant repair oci-index-children`

```sh
ant repair oci-index-children --dry-run
ant repair oci-index-children
```

OCI image indexes stored before the per-platform grouping was recorded at publish time have no links to their child manifests. Pull-through indexes are linked lazily the next time their tag is resolved; this command covers hosted indexes and cached ones that are not re-resolved. It reads manifest bytes already in blob storage, never contacts an upstream, and is idempotent. The report contains `dry_run`, `indexes_found`, `children_linked` and `unreadable`. Like `repair metadata`, it requires the schema to match the binary.

## Related commands

| Command              | Purpose                                                                                                             |
| -------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `ant gc [--dry-run]` | One retention, pressure, garbage collection and integrity pass. See [Storage maintenance](./storage-maintenance.md) |
| `ant storage status` | Active and retained blob backends and the latest migration job. See [Storage migration](./storage-migration.md)     |
| `ant backup verify`  | Check a backup's manifest and, optionally, blob presence. See [Backup and restore](./backup-restore.md)             |

The full command list is in the [CLI reference](../reference/cli.md).
