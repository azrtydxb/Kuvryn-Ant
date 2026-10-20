# Sizing

This page collects the resource defaults of the chart, how the built-in concurrency limits scale, and guidance for the data volume and PostgreSQL. Kuvryn Ant runs as a single writer, so capacity is scaled vertically.

::: warning Planning targets, not measurements
The workload profiles below are engineering acceptance targets from [`docs/planning/release-workloads.md`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/docs/planning/release-workloads.md). They are not measured results, benchmarks or certified capacity. Measure your own workload before committing to a size.
:::

## Chart defaults

| Resource        | Request | Limit   |
| --------------- | ------- | ------- |
| CPU             | `1`     | `4`     |
| Memory          | `2Gi`   | `8Gi`   |
| Data PVC        | `120Gi` |         |
| `/tmp` emptyDir |         | `256Mi` |

The requests are an initial scheduling allocation. The limits match the application budget of the small-site profile (W1: 4 vCPU, 8 GiB).

## Concurrency limits

Three limits scale with the number of CPUs the Go runtime sees (`GOMAXPROCS`, derived from the container's CPU limit, rounded up). A value of `0` in Helm keeps the computed default.

| Limit                      | Env / Helm value                   | Default formula | Range of default | Configurable range |
| -------------------------- | ---------------------------------- | --------------- | ---------------- | ------------------ |
| Concurrent requests        | `ANT_MAX_REQUESTS` / `maxRequests` | 32 x CPUs       | 128 to 1024      | 1 to 4096          |
| PostgreSQL pool            | `ANT_DB_MAX_CONNS` / `dbMaxConns`  | 8 x CPUs        | 16 to 64         | 4 to 500           |
| Uploads per package format | `ANT_UPLOAD_SLOTS` / `uploadSlots` | 2 x CPUs        | 4 to 32          | 1 to 256           |

Resulting defaults:

| CPU limit         | Requests | DB connections | Upload slots per format |
| ----------------- | -------- | -------------- | ----------------------- |
| 1 or 2            | 128      | 16             | 4                       |
| 4 (chart default) | 128      | 32             | 8                       |
| 8                 | 256      | 64             | 16                      |
| 16                | 512      | 64             | 32                      |
| 32 or more        | 1024     | 64             | 32                      |

Behaviour at the limit:

- Requests over `maxRequests` are answered immediately with `503` and `Retry-After: 1` (management API errors use code `unavailable`). `/health/live` is never limited.
- When all upload slots of a format are busy, most adapters reject the next upload with `503` instead of queueing it, so clients retry. Raise `uploadSlots` if CI publish bursts hit this.
- Without a CPU limit, `GOMAXPROCS` is the node's CPU count, which can produce a much larger pool than PostgreSQL allows. Set a CPU limit or set `dbMaxConns` explicitly.

Raise the three together, keep `dbMaxConns` below PostgreSQL's `max_connections` (see [PostgreSQL](postgresql.md#connection-pool-sizing)), and watch request saturation in the metrics before and after.

## Memory

Transfers are streamed with bounded buffers; the planning target is that growing one object from 1 GiB to 100 GiB adds at most 64 MiB of resident memory at fixed concurrency. Some protocol adapters hold bounded request bodies in memory (for example npm publish JSON up to 64 MiB, at most two concurrently). Keep the memory limit at or above the 8 GiB default for busy instances and adjust from observed peaks.

## Data volume

| Backend | What the PVC holds                                                           | Starting point                                                                    |
| ------- | ---------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `disk`  | All blobs, upload staging, local working data                                | Expected blob volume x 1.3, at least the 120 GiB default                          |
| `s3`    | Upload staging, OCI upload and S3 multipart resume state, local working data | The largest concurrent uploads you expect, with headroom; tens of GiB are typical |

Guidance:

- Staging must fit the largest artifacts uploaded concurrently, including pull-through fetches that are being verified.
- Blobs are deduplicated by digest within a deduplication domain, so storage grows with unique content, not with the number of feeds.
- Unreferenced blobs are removed only after `maintenance.gcGrace` (24 h by default); plan headroom for a day of churn.
- With `disk`, enable [storage-pressure eviction](../operations/storage-maintenance.md) for cache-heavy instances so that remote-cached content is evicted before the volume fills.
- Never write backups onto the data volume. `ant backup` refuses local targets above 80 % filesystem usage; use object storage.
- Alert on `ant_data_dir_bytes_free`.
- Block storage only (ext4/XFS); see [Storage](storage.md).

## PostgreSQL

| Profile          | PostgreSQL reference resources |
| ---------------- | ------------------------------ |
| W1 small site    | 2 vCPU, 4 GiB                  |
| W2 build fan-out | 4 vCPU, 8 GiB                  |

Metadata volume is small compared to blobs but grows with the number of assets, versions, audit events (kept 400 days by default) and the replication change history (14 days or 10 million rows by default). Use fast block storage, size `max_connections` for the pool plus CLI commands, and take regular backups. See [PostgreSQL](postgresql.md).

## Reference workload profiles

These are the planning profiles and pass/fail targets. They describe what release qualification is meant to measure.

| Profile                 | Dataset and traffic                                                                             | Reference resources                                           |
| ----------------------- | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| W1 small site           | 100,000 assets; 10,000 packages; 100 GiB working set; 16 clients; 100 metadata requests/s       | App 4 vCPU / 8 GiB; PostgreSQL 2 vCPU / 4 GiB; NVMe; 1 Gbit/s |
| W2 build fan-out        | 64 clients; 1,000,000 assets; 1 TiB working set; cold and warm runs, 90 % popular / 10 % unique | App 8 vCPU / 16 GiB; PostgreSQL 4 vCPU / 8 GiB; 10 Gbit/s     |
| W3 large objects        | 1, 10 and 100 GiB objects; 1, 4 and 16 concurrent transfers; cancellation and range resume      | Same as W2                                                    |
| W4 metadata and publish | 100 metadata requests/s plus 10 publishes/s for 10 minutes; conflicting writers                 | Same as W1                                                    |
| W5 multi-site           | Two instances; 10,000 assets / 100 GiB seed; incremental batches; 50 ms RTT, 100 Mbit/s         | Isolated databases and buckets                                |
| W6 outages              | Database unavailable, storage full, S3 throttling, upstream failure, pod termination            | Fault injection on test data                                  |

Targets:

| Area                         | Target                                                                                                                                          |
| ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| W1 warm metadata             | p95 at most 100 ms, p99 at most 300 ms at 100 requests/s (excluding client network latency)                                                     |
| Warm large-object throughput | At least 80 % of direct backend throughput on the same path                                                                                     |
| W2 cold fan-out              | One upstream fetch per key while healthy; warm runs make no upstream blob fetches for cached immutable objects; metadata error rate below 0.1 % |
| W3 streaming memory          | At most 64 MiB extra peak RSS when growing one object from 1 to 100 GiB; charged transfer buffers at most 1 GiB globally                        |
| W5 replication               | Resume without re-transferring verified blobs; incremental metadata lag p95 at most 30 s for 100 updates                                        |
| Restore                      | 100 GiB / 100,000 assets restored in at most 60 minutes at 100 MiB/s sustained restore throughput                                               |

::: warning Gap
No measured results for W1 to W6 are published. Treat the profile resources as a starting point, not as validated capacity.
:::
