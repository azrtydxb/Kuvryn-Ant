# Benchmark harness (acceptance criterion 8)

Go only (standard library plus the repository module); no third-party load tool is required.

| Path             | Purpose                                                                                                                                                                                                                                                                                                       |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `bench/antbench` | HTTP load generator, one JSON result per run: `get` (closed loop or `-rate`, URL list, hot/popular mix, SHA-256 of every body), `put` (deterministic bodies, streamed above 32 MiB, `-dup` duplicate-coordinate writers), `stream` (large object to a discarding sink with digest check and `-cancel-after`). |
| `bench/runner`   | Orchestrator. Cross-builds `antbench` for the node architecture, runs it from a pod inside the target cluster, samples the Ant pod's `VmRSS`/`VmHWM` from `/proc/1/status` once per second, creates `evb-*` feeds and removes them afterwards.                                                                |
| `bench/antlib`   | Management-API and kubectl helpers shared with `evidence/*/tools`.                                                                                                                                                                                                                                            |

```sh
go run ./bench/runner -target kw  -out evidence/2026-10-18/bench/kw.json
go run ./bench/runner -target oke -scenarios warm,fanout -fanout 16 -duration 20 -out /tmp/oke.json
```

Needs `kubectl` contexts `azrty-oke` and `kw`, Go, and the automation tokens in `~/.kuvryn-ant/`. Load is capped by flags
(`-fanout`, `-duration`, `-large-gib`); defaults are modest enough for a shared cluster.

## Scenarios

| Name      | Shape (workload)                                                                                                                                     | What is asserted or recorded                                                                   |
| --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `warm`    | locally published 64 KiB and 8 MiB objects, c=1 and c=16; 100 req/s package-list metadata (W1, W2 warm)                                              | p50/p95/p99, MiB/s, req/s, errors, RSS during the run                                          |
| `cold`    | remote-source pull-through of 8 unique objects (1 MiB and 4 MiB), then 64 warm reads at c=8                                                          | cold vs warm latency, digests equal the published content, warm run performs no upstream fetch |
| `fanout`  | N concurrent pulls of one uncached key (W2 identical-key cold fan-out)                                                                               | at most one upstream fetch per key (see measurement note), all bodies byte-identical, RSS      |
| `large`   | 1 and 4 GiB objects: streamed publish, 1 and 4 concurrent reads, cancel at 100 MiB then full re-read (W3)                                            | RSS and peak RSS versus object size, digest at every concurrency, cancel does not poison       |
| `outage`  | origin unreachable after warming (W6)                                                                                                                | cached objects keep serving, uncached fail cleanly and fast, recovery without cache poisoning  |
| `publish` | 200 x 64 KiB at c=4/16; 32 writers to one coordinate (W4)                                                                                            | throughput, exactly one accepted body, final read equals the accepted body                     |
| `npm`     | real npm client in a pod installing express, webpack and eslint through a pull-through npm feed: cold, warm (empty client cache), origin unreachable | wall time, resolved package count                                                              |

## Measurement notes and limits

- The client runs in the same cluster as the instance and talks to the ClusterIP service over plain HTTP, so the
  shaped edge load balancer and the WAN are not measured. Ingress overhead is therefore excluded and reported as such.
- Remote sources require `https` with a publicly trusted certificate (CA references are operator-configured and not
  set on these deployments), so the only origin that works for the cold/fan-out/outage scenarios is the OKE instance
  itself (a raw feed with a read-only scoped token). It is served through an edge load balancer fixed at 10 Mbit/s,
  which is why those objects are 1 to 8 MiB and why the large-object scenario uses publish and read rather than a
  1-5 GiB pull-through.
- Upstream-fetch counting: the Ant `pulls` statistics and `metrics/traffic` cache counters turned out not to be
  usable for this (asynchronous and not exact for raw pull-through), and request logs only label non-management routes
  `unmatched` (current builds name them `<format>.<class>`, for example `raw.download`). The runner therefore reads the origin pod's `ant_http_requests_total{route="unmatched"|"raw.metadata"|"raw.download",method="GET"}`
  before and after a window, subtracts a measured background rate (other tenants poll the origin) and, for target
  oke, the clients' own GETs. The result is an estimate; the completion time of the slowest client compared with the
  single-transfer floor (object size / 1.25 MB/s) is the independent cross-check.
- Memory: the sampler reads the Ant process (`/proc/1`) inside the pod, so Go runtime and buffers are included.
  Base RSS is reported separately from growth.
- Direct-backend throughput (the 80 percent warm-throughput target) is not measured by the runner; see the evidence
  README for what was and was not measured.

## Publish path (`bench/publishpath`)

`go test` benchmark of `artifact.Service.Publish` (64 KiB objects, distinct coordinates) against a throwaway PostgreSQL and a disk store, with optional object-store latency injected into Stage, Finalize and Stat the way the S3 backend behaves. It needs no cluster and isolates serialization in the publish transaction, which the HTTP-level `publish` scenario above cannot show.

```sh
ANT_BENCH_STORE_LATENCY=20ms scripts/test-postgres.sh -race=false ./bench/publishpath -run '^$' -bench Publish -benchtime 400x
ANT_BENCH_WAITS=1 ...   # also sample what the active database sessions wait on
```

Finding behind the 2026-10-18 kw plateau (about 7 req/s regardless of concurrency): every publish held the instance writer-lease row and the feed row `FOR UPDATE` until commit, and the in-transaction object-store `Stat` ran under both, so throughput was capped at 1 / (commit-path latency). With 20 ms object-store latency, 16 publishers: 42 req/s before, 114 req/s after (both locks are now `FOR SHARE`: takeover, renewal and feed edits still exclude writers, writers no longer exclude each other); 4 publishers: 23 -> 40 req/s. A single publisher is unchanged (about 10 req/s, the sum of the injected latencies). The S3 backend also now publishes an object that fits its first buffer (at most one part, 64 MiB by default) with one conditional `PutObject` instead of staging multipart + server-side copy + delete (9 requests down to 1 plus the `Stat`), and no longer allocates a 64 MiB part buffer per upload up front (it starts at 1 MiB and grows).
