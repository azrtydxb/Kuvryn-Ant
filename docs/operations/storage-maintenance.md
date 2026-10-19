# Storage maintenance: retention defaults and storage-pressure eviction

Blob bytes are never deleted inline. Metadata is removed first (retention policies or
pressure eviction), then the two-phase mark/sweep garbage collector reclaims the bytes
(`internal/retention/README.md`). The console page is Settings, Retention and cleanup.

## Why `unreferenced_bytes` is not `gc_candidates`

`GET /api/v1/storage/usage` reports `unreferenced_bytes` (blobs with no asset) and
`gc_candidates` (blobs already marked). A blob is marked only once it is older than
`ANT_GC_GRACE` (24h), and swept in a later run one grace period after it was marked, so
fresh orphans legitimately show as unreferenced with zero candidates for up to two
graces. The response now splits the number: `reclaimable_bytes` (marked, or old enough
to be marked next run) and `in_grace_bytes`. Nothing is deleted at all unless a
retention policy removes versions, so cached remote content grows until you set a
default policy or enable pressure eviction.

## Settings

Editable in the console and through `GET/PUT /api/v1/retention/settings` (If-Match
revision, audited). A value set by the environment wins, is listed in `locked` and is
read-only there.

| Setting                          | Environment                                           | Chart value                                       | Default                        |
| -------------------------------- | ----------------------------------------------------- | ------------------------------------------------- | ------------------------------ |
| Maintenance interval             | `ANT_GC_INTERVAL`                                     | `maintenance.gcInterval`                          | `6h` (`0` disables)            |
| Orphan grace                     | `ANT_GC_GRACE`                                        | `maintenance.gcGrace`                             | `24h`                          |
| Default policy: not accessed for | `ANT_RETENTION_DEFAULT_MAX_IDLE`                      | `maintenance.defaultPolicy.maxIdle`               | unset                          |
| Default policy: older than       | `ANT_RETENTION_DEFAULT_MAX_AGE`                       | `maintenance.defaultPolicy.maxAge`                | unset                          |
| Default policy: keep last        | `ANT_RETENTION_DEFAULT_KEEP_LAST`                     | (extraEnv)                                        | unset                          |
| Default policy: enabled          | `ANT_RETENTION_DEFAULT_ENABLED`                       | (extraEnv)                                        | implied by a rule above        |
| Pressure enabled                 | `ANT_PRESSURE_ENABLED`                                | `maintenance.pressure.enabled`                    | `false`                        |
| High / low watermark             | `ANT_PRESSURE_HIGH_PERCENT` / `_LOW_PERCENT`          | `maintenance.pressure.highPercent` / `lowPercent` | 85 / 75                        |
| Capacity quota                   | `ANT_STORAGE_CAPACITY_BYTES` (`500GiB`, `2TB`, bytes) | `maintenance.pressure.capacity`                   | unset                          |
| Check interval                   | `ANT_PRESSURE_INTERVAL`                               | `maintenance.pressure.interval`                   | `5m` (`0` disables the worker) |
| Sweep wait after eviction        | `ANT_PRESSURE_SWEEP_GRACE`                            | `maintenance.pressure.sweepGrace`                 | `15m`                          |

## Default policy

Applies to cached remote content (remote sources) in every feed without its own
retention policy; a feed or source policy takes precedence. Local and replica content is
never deleted by it. Tagged versions and the newest `min_keep` versions are kept.
"Not accessed" uses the later of the asset creation time, `last_accessed_at` and the last
download recorded by the traffic flusher (flushed in batches, so it lags by one flush).

## Storage pressure

Capacity: disk uses the filesystem of the blob root; a configured quota overrides it
(usage is then committed blob bytes). Object storage has no native capacity: without a
quota the trigger is disabled and the console says so (state `no_capacity`).

Semantics: at usage at or above the high watermark, eviction removes versions until the
projected usage (usage minus bytes garbage collection will free anyway) is at or below
the low watermark. Between the watermarks nothing happens, so it does not flap.
Order is least recently used first across all evictable sources. Evictable: remote cache
content only, minus the newest `min_keep` versions per package and tagged versions
(taken from the source's policy, else the default policy, else 1 and protected). Local
and replica content is evictable only when its source (or every feed attached to it) has
an enabled policy with `allow_release_delete`. A version that frees no bytes because its
blobs are shared with protected content is skipped. Each deletion is audited and emitted
as an outbox event `retention.pressure.evict`.

Reclaiming: blobs freed by eviction are marked with a short `sweep_after`
(`ANT_PRESSURE_SWEEP_GRACE`) instead of waiting out the full grace. The sweep is the
normal one: it locks the `blobs` row, re-checks references and only then deletes. The
check runs every `ANT_PRESSURE_INTERVAL` under the writer lease, in every maintenance
run, and on demand (Run now / Dry run, `POST /api/v1/retention/runs`).

Health: above the high watermark with nothing (more) evictable is state `unevictable`:
shown in the console, added to `warnings` of `GET /api/v1/status` (admin) and exported as
`ant_storage_pressure_unevictable`. Metrics: `ant_storage_usage_ratio`,
`ant_storage_used_bytes`, `ant_storage_capacity_bytes`, `ant_storage_reclaimable_bytes`,
`ant_storage_pressure_state{state}`, `ant_storage_pressure_evicted_{versions,bytes}_total`,
`ant_storage_pressure_last_run_timestamp_seconds`.

## Suggested values

OKE (S3 on OCI, about 46 GB stored): `pressure.enabled=true`, `capacity=150GiB` (the
budget you want to pay for, not a bucket limit), 85/75, `defaultPolicy.maxIdle=720h`
(30 days). Start with a dry run from the console.

kw (MinIO on TrueNAS, about 8.5 GB unreferenced): `capacity` set to the dataset or quota
size you want to protect (for example `500GiB`), 85/75, `defaultPolicy.maxIdle=720h`.
MinIO has no capacity API either, so the quota is required.

## Cache feeds

Feeds with purpose `cache` (buildx registry cache, sccache; `docs/operations/build-caches.md`)
follow their own per-feed policy instead of retention policies and the instance default:
`max_idle_seconds` (default 7 days) and optionally `max_bytes`. Their content is evictable
without `allow_release_delete`, tagged versions and the newest version are not exempt, and
they are never replicated.

Under storage pressure the order is: cache feeds first (least recently used first), then
remote-cached content, then release content that explicitly allows deletion. A cache feed
never makes the state `unevictable` worse: if the cache feeds free enough, nothing else is
touched. Blobs freed from a cache feed (eviction, an OCI tag overwrite, the cleanup API) get
the short sweep wait (`ANT_PRESSURE_SWEEP_GRACE`) and are deleted by the next pressure-loop
pass, which runs every `ANT_PRESSURE_INTERVAL` whether or not pressure eviction is enabled.
`GET /api/v1/storage/usage` lists `cache_feeds` with the bytes each holds and the console
shows them under Retention and cleanup, Overview. Metrics:
`ant_storage_evicted_{versions,bytes}_total{purpose,reason}`, `ant_feed_used_bytes`,
`ant_feed_max_bytes`.
