# Storage maintenance

Kuvryn Ant never deletes blob bytes inline. Content leaves the system in two steps: first its metadata is removed (by a retention policy, the instance default policy, storage-pressure eviction, a cache-feed limit or an explicit delete), then the two-phase mark/sweep garbage collector reclaims the bytes that nothing references any more. This page describes every part of that pipeline, the integrity scan that runs alongside it, storage quotas and the settings that control them.

In the console all of this lives under **Settings, Retention and cleanup**. The management API endpoints are under `/api/v1/retention/*`, `/api/v1/storage/usage` and `/api/v1/limits`; every one of them requires instance admin.

## The maintenance run

One maintenance run does, in order:

1. **Retention**: applies feed and source retention policies, the instance default policy and cache-feed limits, and deletes the selected versions (metadata only).
2. **Storage pressure**: if pressure eviction is enabled and usage is at or above the high watermark, evicts least recently used versions.
3. **Garbage collection**: marks unreferenced blobs and sweeps blobs marked in an earlier run.
4. **Integrity**: re-hashes one bounded batch of blobs.

The run happens:

- on a schedule every `ANT_GC_INTERVAL` (default `6h`, `0` disables the scheduled loop), only in the process that holds the instance writer lease, and every deleting step re-checks that lease;
- on demand from the console (**Run now** / **Dry run**) or with `POST /api/v1/retention/runs` (`{"dry_run": true|false}`, returns 202; poll `GET /api/v1/retention/runs/{id}` for the report; one manual run at a time, otherwise 409; the last 200 runs are kept);
- from the host with `ant gc [--dry-run]`.

Independently of the maintenance loop, a cheap pressure check runs every `ANT_PRESSURE_INTERVAL` (default `5m`) so that a filling disk is relieved between maintenance runs. That loop also performs cache-feed idle/size eviction and sweeps blobs whose sweep was expedited by an eviction, whether or not pressure eviction is enabled.

### `ant gc`

```sh
ant gc --dry-run    # report exactly what a run started now would delete; writes nothing
ant gc              # one retention + pressure + GC + integrity pass
```

`ant gc` uses the same environment as `ant serve` (`ANT_DATABASE_URL`, `ANT_ROOT_KEY_FILE`, storage settings) and prints one JSON report on stdout. It is a local operation authorized by possession of the deployment's database and storage configuration, like `ant rotate-secrets`. It is safe to run while the service is serving: deletion safety comes from PostgreSQL row locks and the grace periods, not from stopping the server. The configured `ANT_INSTANCE_ID`, if set, must match the bootstrapped instance.

::: warning
Do not run `ant gc` (or let a scheduled run sweep) while `ant backup` is running, or between a `--blobs reference` backup and the end of an upgrade window. See [Backup and restore](./backup-restore.md).
:::

## Garbage collection

A blob is garbage when no asset references its `blobs` row, or when bytes exist in the store with no row at all (left behind by a failed metadata commit and found by listing the store).

1. **Mark.** Unreferenced rows older than the grace period (`ANT_GC_GRACE`, default `24h`), and listed objects older than the grace period with no row, are recorded as GC candidates. Store listing is bounded per run and resumes from a persisted cursor. A candidate whose blob regained a reference is unmarked.
2. **Sweep**, in a later run only: the candidate must have been marked before the run started and be older than the grace period. Each candidate is swept in its own transaction that locks the `blobs` row `FOR UPDATE`, re-checks that no asset references it, deletes the bytes, then deletes the row and the candidate.

A concurrent publish either already holds the row lock (the sweep waits, sees the new reference and aborts) or blocks until the sweep commits. The publisher also verifies, inside its own transaction and after locking the `blobs` row, that the bytes still exist with the expected size, so a publish can never commit metadata for bytes a sweep just removed.

`--dry-run` (and `"dry_run": true` on the API) reports exactly the blobs a real run started now would mark and sweep, and writes nothing.

Garbage collection also aborts S3 multipart uploads that have been idle for more than 24 hours and reports them as `stale_uploads_aborted`.

### Why unreferenced bytes are not immediately reclaimable

`GET /api/v1/storage/usage` reports, among others:

| Field                | Meaning                                            |
| -------------------- | -------------------------------------------------- |
| `unreferenced_bytes` | Bytes of blobs no asset references                 |
| `gc_candidates`      | Blobs already marked                               |
| `reclaimable_bytes`  | Marked, or old enough to be marked by the next run |
| `in_grace_bytes`     | Unreferenced but still inside the grace period     |
| `cache_feeds`        | Bytes held by each feed with purpose `cache`       |
| `pressure`           | The current pressure status                        |

A blob is marked only once it is older than `ANT_GC_GRACE` and swept one grace period after it was marked, so fresh orphans legitimately show as unreferenced with zero candidates for up to two grace periods.

::: tip Nothing is deleted by default
Without a retention policy, an instance default policy or pressure eviction, no version is ever removed, so remote-cached content grows without bound. Set a default policy or enable pressure eviction on any instance that proxies public upstreams.
:::

## Retention policies

A retention policy targets one feed or one source. Create it in the console or with `POST /api/v1/retention/policies` (409 if that feed or source already has one); change it with `PUT /api/v1/retention/policies/{id}` and delete it with `DELETE /api/v1/retention/policies/{id}` (both with `If-Match`).

| Field                   | Meaning                                                              |
| ----------------------- | -------------------------------------------------------------------- |
| `feed_id` / `source_id` | The target (exactly one)                                             |
| `keep_last`             | Keep at most this many versions per package (component)              |
| `max_age_seconds`       | Delete versions older than this                                      |
| `max_idle_seconds`      | Delete versions not downloaded for this long (remote-cache eviction) |
| `min_keep`              | The newest N versions are exempt from age and idle rules (default 1) |
| `include_tagged`        | Also select tagged versions (default: tagged versions are exempt)    |
| `allow_release_delete`  | Allow the policy to delete content of `local` and `replica` sources  |
| `enabled`               | Turn the policy on or off                                            |

A version is selected when it violates any rule, except:

- the `min_keep` newest versions are exempt from age and idle rules;
- tagged versions are exempt unless `include_tagged`;
- content of `local` and `replica` sources is release content and is never selected unless `allow_release_delete` is set; `remote` (cache) sources are not gated.

A source policy overrides feed policies. Without a source policy, every feed attached to the source must have an enabled policy and all of them must select the version; a feed with no policy means "keep everything". Deletion removes tags, assets and the version under the same per-coordinate lock publication uses, writes a `retention.version.delete` audit event, and leaves the bytes to garbage collection.

"Not accessed" (idle) uses the latest of the asset creation time, its last access time and the last download recorded by the traffic flusher. Downloads are flushed in batches, so idle time lags by one flush interval.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
# Keep the 20 newest versions per package of a remote npm source,
# and evict anything not downloaded for 30 days.
curl -fsS -X POST $ANT/api/v1/retention/policies -H "$H" -H "$J" \
  -H 'Idempotency-Key: npm-remote-retention-1' \
  -d '{"source_id":"<SOURCE_ID>","keep_last":20,"max_idle_seconds":2592000,"min_keep":1,"enabled":true}'
```

## Instance default policy

The instance default policy applies to **remote-cached content** in every feed that has no policy of its own. A feed or source policy takes precedence. Local and replica content is never deleted by the default policy, and tagged versions and the newest `min_keep` versions are kept unless configured otherwise (`include_tagged`).

It is edited in the console or with `GET`/`PUT /api/v1/retention/settings` (with `If-Match`, audited), together with the pressure settings. A value set by the environment wins: it is listed in the `locked` array of the response (dotted names such as `pressure.capacity_bytes`) and changing it through the API returns 409.

```sh
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/retention/settings -H "$H" \
  | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/retention/settings -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: retention-settings-1' -d '{
  "default_policy": {"enabled": true, "max_idle_seconds": 2592000, "min_keep": 1, "include_tagged": false},
  "pressure": {"enabled": true, "high_percent": 85, "low_percent": 75, "capacity_bytes": 536870912000}
}'
```

## Storage pressure

Pressure eviction removes least recently used content when storage fills up, before the volume or the budget runs out.

**Capacity.** For the disk backend, capacity is the size of the filesystem holding the blob root. A configured capacity quota (`ANT_STORAGE_CAPACITY_BYTES` or `pressure.capacity_bytes`) overrides it; usage is then the committed blob bytes. Object storage has no native capacity, so without a quota the trigger is disabled and the status is `no_capacity`. Set the quota to the budget you want to protect, not to a bucket limit.

**Watermarks.** When usage is at or above the high watermark (default 85%), eviction removes versions until the projected usage (usage minus what garbage collection will free anyway) is at or below the low watermark (default 75%). Between the watermarks nothing happens, so eviction does not flap. The low watermark must be below the high one.

**What is evictable, in order:**

1. Feeds with purpose `cache` (least recently used first). If they free enough, nothing else is touched.
2. Remote-cached content, least recently used first across all evictable sources, minus the newest `min_keep` versions per package and tagged versions (taken from the source's policy, else the default policy, else 1 and protected).
3. Local and replica content, only when its source (or every feed attached to it) has an enabled policy with `allow_release_delete`.

A version whose blobs are shared with protected content frees no bytes and is skipped. Every deletion is audited and emitted as the outbox event `retention.pressure.evict`.

**Reclaiming.** Blobs freed by eviction are marked with a short sweep wait (`ANT_PRESSURE_SWEEP_GRACE`, default `15m`) instead of the full grace period. The sweep itself is the normal one (row lock, reference re-check).

**States** (`GET /api/v1/retention/pressure`, the console, the `ant_storage_pressure_state` metric):

| State         | Meaning                                                           |
| ------------- | ----------------------------------------------------------------- |
| `disabled`    | Pressure eviction is not enabled                                  |
| `no_capacity` | Enabled, but capacity is unknown (object storage without a quota) |
| `invalid`     | The watermarks are inconsistent                                   |
| `ok`          | Below the high watermark                                          |
| `pressure`    | At or above the high watermark and being relieved                 |
| `unevictable` | At or above the high watermark with nothing (more) evictable      |

`unevictable` is a health warning: it is shown in the console, added to the `warnings` of `GET /api/v1/status` (admin) and exported as `ant_storage_pressure_unevictable`. See [Observability](./observability.md#storage-and-retention) for all storage metrics.

### Suggested starting values

| Deployment                                    | Suggested settings                                                                                                                             |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Disk, dedicated volume                        | `pressure.enabled=true`, 85 / 75, no capacity (the filesystem size is used), `defaultPolicy.maxIdle=720h`                                      |
| S3-compatible object storage                  | `pressure.enabled=true`, `capacity` set to the budget you are willing to pay for (for example `150GiB`), 85 / 75, `defaultPolicy.maxIdle=720h` |
| MinIO or another store without a capacity API | as for object storage: the quota is required                                                                                                   |

Start with a dry run from the console before enabling deletion.

## Cache feeds

Feeds with purpose `cache` (buildx registry cache, sccache; see [Build caches](../build-caches/index.md) and [Feed purposes](../concepts/purposes.md)) do not use retention policies or the instance default. They follow their own per-feed `cache_policy`: `max_idle_seconds` (default 7 days) and optionally `max_bytes`. Their content is evictable without `allow_release_delete`, tagged versions and the newest version are not exempt, and they are never replicated.

Blobs freed from a cache feed (eviction, an OCI tag overwrite, `DELETE /api/v1/feeds/{id}/cache`) get the short sweep wait and are deleted by the next pressure-loop pass.

## Quotas and rate limits

`GET`/`PUT /api/v1/limits` (instance admin, `If-Match`, `Idempotency-Key`) holds two lists that are replaced atomically and apply immediately on the instance:

**Storage quotas** (`quotas`) bound the logical content (sum of asset sizes) and/or the package count of a feed, or of one namespace of a feed. They are enforced when a publication commits; at least one bound is required.

| Field          | Meaning                                                      |
| -------------- | ------------------------------------------------------------ |
| `scope`        | `feed` or `namespace`                                        |
| `feed_id`      | The feed                                                     |
| `namespace`    | For `namespace` scope: the first path segment below the feed |
| `max_bytes`    | Maximum logical bytes                                        |
| `max_packages` | Maximum package count                                        |

A publication that would exceed a quota is rejected with **507 Insufficient Storage** and the body `{"code":"quota_exceeded", ...}`.

**Rate limits** (`rate_limits`) are token buckets with `requests_per_minute` and `burst`, by `scope`:

| Scope       | `subject`                                                                                                                                                                             |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `principal` | A principal ID                                                                                                                                                                        |
| `feed`      | A feed ID                                                                                                                                                                             |
| `namespace` | The feed ID, plus `namespace` (first path segment below the feed)                                                                                                                     |
| `site`      | A site label, plus `cidrs` (client networks by TCP peer address, or the forwarded address from a [trusted proxy](../install/ingress-tls.md)); no `cidrs` makes an instance-wide limit |

A throttled request gets **429** with `Retry-After` and `X-RateLimit-Scope`. Buckets are held in memory per process.

`GET /api/v1/limits/usage` returns current quota usage and the throttle counters. The metrics `ant_quota_usage`, `ant_quota_limit`, `ant_quota_rejections_total` and `ant_ratelimit_throttled_total` expose the same data.

```sh
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/limits -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/limits -H "$H" -H "$J" -H "If-Match: $REV" -H 'Idempotency-Key: limits-1' -d '{
  "quotas": [{"scope":"feed","feed_id":"<FEED_ID>","max_bytes":107374182400}],
  "rate_limits": [{"scope":"site","subject":"ci","cidrs":["10.0.0.0/24"],"requests_per_minute":6000,"burst":500}]
}'
```

## Integrity scan

Each maintenance run re-hashes up to `ANT_INTEGRITY_BATCH` blobs (default 200) or `ANT_INTEGRITY_MAX_BYTES` (default 1 GiB), whichever comes first, in a stable order, resuming from a cursor on the next run. Mismatches are recorded as findings with a kind of `missing`, `size` or `digest`, and resolved automatically when a later pass finds the blob correct.

Nothing is deleted or repaired by the scan. Transient backend errors are counted and retried. Read open findings with `GET /api/v1/retention/integrity-findings` (or in the console). To recover, restore the blob from a backup or a replica; `ant repair metadata` reports assets whose blob object is missing but does not change them (see [Diagnostics and repair](./diagnostics.md)).

## Settings reference

Environment variables take precedence over, and lock, the corresponding stored settings. See also the full [configuration reference](../reference/configuration.md).

| Variable                          | Default                             | Range / format                                                              | Chart value                         |
| --------------------------------- | ----------------------------------- | --------------------------------------------------------------------------- | ----------------------------------- |
| `ANT_GC_INTERVAL`                 | `6h`                                | `0` (off) or `1m`..`720h`                                                   | `maintenance.gcInterval`            |
| `ANT_GC_GRACE`                    | `24h`                               | `1m`..`2160h`                                                               | `maintenance.gcGrace`               |
| `ANT_GC_DRY_RUN`                  | `false`                             | boolean; scheduled runs report only                                         | `extraEnv`                          |
| `ANT_GC_BATCH_SIZE`               | `500`                               | 1..100000                                                                   | `extraEnv`                          |
| `ANT_INTEGRITY_BATCH`             | `200`                               | 1..100000 blobs per run                                                     | `extraEnv`                          |
| `ANT_INTEGRITY_MAX_BYTES`         | `1073741824`                        | at least 1048576                                                            | `extraEnv`                          |
| `ANT_PRESSURE_ENABLED`            | unset (stored setting, default off) | boolean                                                                     | `maintenance.pressure.enabled`      |
| `ANT_PRESSURE_HIGH_PERCENT`       | unset (stored, default 85)          | 1..99                                                                       | `maintenance.pressure.highPercent`  |
| `ANT_PRESSURE_LOW_PERCENT`        | unset (stored, default 75)          | 1..99, below high                                                           | `maintenance.pressure.lowPercent`   |
| `ANT_STORAGE_CAPACITY_BYTES`      | unset                               | bytes or `KiB`/`MiB`/`GiB`/`TiB`/`KB`/`MB`/`GB`/`TB` suffix, at least 1 MiB | `maintenance.pressure.capacity`     |
| `ANT_PRESSURE_INTERVAL`           | `5m`                                | `0` (off) or `1m`..`24h`                                                    | `maintenance.pressure.interval`     |
| `ANT_PRESSURE_SWEEP_GRACE`        | `15m`                               | `1m`..`24h`                                                                 | `maintenance.pressure.sweepGrace`   |
| `ANT_RETENTION_DEFAULT_ENABLED`   | unset (implied by a rule below)     | boolean                                                                     | `extraEnv`                          |
| `ANT_RETENTION_DEFAULT_KEEP_LAST` | unset                               | 1..1000000                                                                  | `extraEnv`                          |
| `ANT_RETENTION_DEFAULT_MAX_AGE`   | unset                               | duration `1h`..`876000h`                                                    | `maintenance.defaultPolicy.maxAge`  |
| `ANT_RETENTION_DEFAULT_MAX_IDLE`  | unset                               | duration `1h`..`876000h`                                                    | `maintenance.defaultPolicy.maxIdle` |

The Helm chart's maintenance block (see [Helm values](../install/helm.md#values-reference)):

```yaml
maintenance:
  gcInterval: 6h # "0" disables the scheduled maintenance loop
  gcGrace: 24h # minimum age of an unreferenced blob before GC marks it
  defaultPolicy:
    maxIdle: "" # e.g. 720h; empty = editable in the console
    maxAge: ""
  pressure:
    enabled: false
    highPercent: 85
    lowPercent: 75
    capacity: "" # e.g. 500GiB; required for object storage
    interval: 5m # "0" disables the periodic check
    sweepGrace: 15m
extraEnv:
  ANT_RETENTION_DEFAULT_KEEP_LAST: "50"
```

::: tip Chart behaviour
The chart sets `ANT_PRESSURE_ENABLED`, the watermarks and `ANT_STORAGE_CAPACITY_BYTES` only when `maintenance.pressure.enabled` is `true`. With `enabled: false` those settings are not locked and stay editable in the console. The default-policy variables are set only when their values are non-empty.
:::

::: warning Limits

- Running the scheduled worker and `ant gc` at the same time is safe (row locks), but may duplicate store-listing work.
- S3 listing and deletion for garbage collection are tested against an in-process S3 fixture, not certified against every provider.
- Rate-limit buckets are per process and are not shared between instances.
  :::
