# internal/retention

Retention policies, safe orphan-blob garbage collection and integrity scanning
(spec 6.4 garbage collection, 6.6 integrity checks, 6.13 maintenance). Schema:
`internal/database/migrations/020_retention.sql`.

## Entry points

| API | Purpose |
| --- | --- |
| `Service.RunOnce(ctx, dryRun)` | retention, then GC, then one integrity batch; returns a JSON-able `Report` |
| `Service.ApplyRetention` / `CollectGarbage` / `VerifyIntegrity` | the individual stages |
| `Service.SetPolicy` / `ListPolicies` / `DeletePolicy` / `OpenFindings` | policy and finding administration |
| `Service.Run(ctx, interval, dryRun, handle)` | periodic loop used by the worker |
| `ant gc [--dry-run]` | one run from the CLI (`cmd/ant/gc.go`), JSON report on stdout |
| worker | started from `serve` when `ANT_GC_INTERVAL` is non-zero, fenced by the instance writer lease |

Settings: `ANT_GC_INTERVAL` (default 6h, `0` disables the worker), `ANT_GC_GRACE`
(24h), `ANT_GC_DRY_RUN`, `ANT_GC_BATCH_SIZE`, `ANT_INTEGRITY_BATCH`,
`ANT_INTEGRITY_MAX_BYTES`.

## Garbage collection protocol

A blob is garbage when no asset references its `blobs` row (or no row exists at
all: bytes left by a failed metadata commit, discovered by listing the store).

1. **Mark.** Unreferenced rows older than the grace period, and listed objects
   older than the grace period with no row, are recorded in `gc_candidates`.
   Listing is bounded (`ObjectScanLimit` per domain per run) and resumes from a
   persisted cursor. A candidate whose blob regained a reference is unmarked.
2. **Sweep**, in a later run only (`first_seen_at` must predate the run start
   and be older than the grace period). Per candidate, one transaction:
   lock the `blobs` row `FOR UPDATE` (inserting a placeholder if none exists),
   re-check that no asset references it, delete the bytes through
   `blob.Deleter`, delete the row and candidate, commit. A concurrent publish
   either already holds the row (the sweep waits, then sees the reference and
   aborts) or blocks on the row until the sweep commits.

`dry-run` reports exactly the blobs a real run started now would mark and sweep
and writes nothing. Both disk and S3 stores implement `blob.Deleter` and
`blob.Lister` (`internal/blob/gc.go`); stores that do not are skipped for
listing and fail closed for deletion.

### Publisher contract (important)

`artifact.Service.Publish` finalizes bytes before its metadata transaction. The
row lock closes every interleaving provided the publisher calls `Stat` on the
blob **after** upserting the `blobs` row and **inside** its transaction, failing
the publish if the bytes are gone. Without that check a publish that finalized
against an already-present object in the last milliseconds before a sweep can
commit metadata for deleted bytes. The grace periods make this window
negligible but not impossible; adding the in-transaction `Stat` to
`commitAsset` closes it. The stress test in `gc_test.go` demonstrates the
invariant with a publisher that does this.

## Retention policies

A policy targets one feed or one source (`retention_policies`). Rules: `KeepLast`
(cap per component), `MaxAge`, `MaxIdle` (remote-cache eviction by last access,
`assets.last_accessed_at`, falling back to `created_at`; call `retention.Touch`
from readers). A version is selected when it violates any rule, except:

* the `MinKeep` newest versions (default 1) are exempt from age/idle rules;
* tagged versions are exempt unless `IncludeTagged`;
* content of `local`/`replica` sources is release content and is never selected
  unless `AllowReleaseDelete`; `remote` (cache) sources are not gated.

A source policy overrides feed policies. Without one, every feed attached to the
source must have an enabled policy and all must select the version; a feed with
no policy means keep everything. No policy means nothing is ever deleted.
Deletion removes tags, assets (cascading subject links) and the version under the
same per-coordinate advisory lock publication uses, writes
`retention.version.delete` audit events, and leaves blob bytes to GC.

## Integrity scan

`VerifyIntegrity` re-hashes up to `IntegrityBatch` blobs / `IntegrityMaxBytes`
per run in `(domain, digest)` order, resuming from a cursor. Mismatches
(`missing`, `size`, `digest`) are returned and recorded in
`blob_integrity_findings` (resolved automatically when a later pass is clean).
Nothing is deleted or repaired; transient backend errors are counted and retried.

## Limitations

* No management API or console for policies yet (`api/` is out of scope); use the
  Go API or SQL.
* Nothing in the tree calls `Touch` yet and remote materialization is not wired,
  so idle eviction currently falls back to `created_at`.
* Multi-process runs (worker plus CLI) are safe via row locks but may duplicate
  listing work. Abandoned S3 multipart uploads are not aborted here.
* S3 listing/deletion is covered by the in-process fixture, not a live provider.

## Instance default and storage pressure

See `docs/operations/storage-maintenance.md`. `settings.go` (stored settings with
environment overrides, `SaveSettings`) and `pressure.go` (`RunPressure`, capacity,
LRU selection, `PressureStatus`) add an instance default policy for remote-cached
content and watermark-driven eviction. `RunOnce` runs retention, then pressure, then GC.
`gc_candidates.sweep_after` shortens the sweep wait of blobs freed by eviction; the
sweep transaction is unchanged (row lock, reference re-check). The earlier "no
management API" and "Touch not wired" limitations are obsolete: policies have an API
and console, and idle time uses `asset_downloads.last_downloaded_at`.
