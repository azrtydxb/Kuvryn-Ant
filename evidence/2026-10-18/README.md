# Qualification session 2026-10-18/19: AC5, AC6, AC8, AC9

Targets: **OKE** `https://ant.azrty.com` (context `azrty-oke`, OCI Object Storage via S3 compatibility, CloudNativePG
3 instances, PostgreSQL 17.11, Kubernetes v1.36.4, 4-OCPU A1.Flex arm64 nodes) and **kw** `https://ant.kw.watteel.lab`
(context `kw`, MinIO on TrueNAS over plain HTTP on the LAN, CloudNativePG 2 instances, PostgreSQL 17.6, k3s v1.34.4,
8-CPU arm64 nodes). Ant pod limits on both: 4 CPU / 8 Gi, requests 1 CPU / 2 Gi; `/tmp` emptyDir 256 Mi. The OKE public
edge is an OCI flexible load balancer fixed at 10 Mbit/s (annotation `shape-flex-min=max=10`); this shapes every
cross-site transfer below (about 1.2 MB/s). MinIO and TrueNAS versions were not inventoried (no access).

The shared deployments were redeployed by other work every 20 to 80 minutes during this session (Helm revisions 6 to
14), so each run records the image it actually hit. AC5, AC6 and `bench/kw.json` ran on `0.1.0-50d26c0`
(`sha256:fe0427fc13893cd8...`, Helm rev 6/7, 23:12 to 01:36 +04). Later runs are on `0.1.0-375611f` (main HEAD at the
time, `sha256:e0469b5205e0...`) or on the digest named in the run's `environment.ant_image`.

Owner constraint applied during the session: the harness and tooling are Go and POSIX shell with `jq`; there is no
Python in the repository. The AC5 setup, seed and verify steps, and the AC6 scenario were first run with equivalent
Python scripts, then ported to Go (`tools/ac5`, `tools/ac6`) and the Go versions were re-run and produce the
committed AC6, verify, incremental, fault and outage results. The two seed timelines in `raw/` were produced by the
Python seed script (same API calls; the Go `seed` command is a line-for-line port and was not re-run, to avoid
repeating a 30 minute seed against the shared 10 Mbit/s link).

## Result summary

| AC                         | Result                                            | Notes                                                                                                                                                                                                                                                      |
| -------------------------- | ------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 5 two-instance replication | **Pass with one blocking defect (bug 1)**         | Seed, interruption/resume without retransfer, digest verification, incremental lag (p95 16 s for 100 metadata updates), corrupt-blob rejection, outage, deletion dry run all demonstrated. Blobs that cannot be sent within 10 s livelock the job (bug 1). |
| 6 backend equivalence      | **Pass**                                          | 46 identical steps, 0 failures on local disk, MinIO and OCI Object Storage.                                                                                                                                                                                |
| 8 benchmarks               | **Partial**                                       | Warm throughput, 1 and 4 GiB streaming, fan-out, metadata latency met on kw; OKE write path and 4 GiB are limited by the backend (bug 11); upstream-fetch counting is an estimate; no cargo run; W2 1M-asset and W3 10/100 GiB profiles not run.           |
| 9 backup/restore, upgrade  | **Exercised; restore needs a workaround (bug 5)** | Backup, verify and restore of 11.7 GB into a scratch database/prefix worked only after disabling triggers in the restore session; Helm upgrade 24 to 26 migrated and kept data; rollback path verified.                                                    |

## AC5: two-instance replication (kw replicates an OKE feed)

Network path verified: a kw pod reaches `https://ant.azrty.com` over the public internet with a valid certificate.
OKE side: feeds `ev5s-raw` (raw, 350 assets, 81,850,975 unique bytes: 250 small 1 to 64 KiB files and 100 files of
0.25 to 1.2 MiB) and `ev5s-oci` (busybox and distroless-static multi-arch images, 88 assets, 19,522,272 bytes), a
service principal, a role granting `read`+`replicate` on those two feeds, and a token scoped to them. kw side: peer
(URL + that token), one `continuous` job per feed (10 s interval, `include_cached`, deletion policy `ignore`),
destination feeds `ev5s-raw-replica` and `ev5s-oci-replica`. Commands: `tools/ac5` (`setup`, `seed`, `ctl`,
`verify`, `incr`); raw outputs in `raw/`, `ac5-*.json`.

| Item                                                  | Result                                                                                                                                                                                                                                                                                                                                                                                                            | Evidence                                                                  |
| ----------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| Full seed                                             | 350 raw assets committed in two pages (200 + 150); OCI job 88 assets                                                                                                                                                                                                                                                                                                                                              | `raw/ac5-seed-timeline.jsonl`, `raw/ac5-seed-oci-timeline.jsonl`          |
| Interrupt mid-seed                                    | kw `ant-ant` scaled to 0 for 45 s after page 1 (200 of 350 assets, 77,038,900 bytes) committed; scaled back, ready in 17.6 s                                                                                                                                                                                                                                                                                      | timeline events `interrupt`, `down`, `scaled-up`, `ready-again`           |
| Resume without retransfer                             | Seed finished 9 s after the pod was ready. Bytes over the wire for the whole seed: 81,857,327 against 81,850,975 unique bytes = **6,352 B (0.008 %)** repeated (the one in-flight small blob); 4 blobs counted as deduplicated                                                                                                                                                                                    | final `status` row; `ant_replication_*` metrics in `ac5-incremental.json` |
| Resume of an interrupted incremental sync             | 30 files (16.35 MB) published, kw killed 2 s later for 30 s: 25 blobs fetched (14,342,480 B), 5 deduplicated, none repeated, all 30 verified, caught up 1.1 s after ready                                                                                                                                                                                                                                         | `ac5-incremental.json` `incremental_interrupted`                          |
| Digest verification                                   | all 350 replicated raw artifacts re-read from kw and identical (SHA-256, size) to the publisher manifest; both images have identical manifest digests on OKE and kw and `crane validate --remote` passes on kw                                                                                                                                                                                                    | `ac5-verify.json`                                                         |
| Incremental lag, 20 raw publishes                     | visible on kw after p50 5.5 s, p95 8.9 s, max 9.4 s (poll interval 10 s)                                                                                                                                                                                                                                                                                                                                          | `ac5-incremental.json`                                                    |
| Incremental lag, 100 metadata-only updates (OCI tags) | p50 8.5 s, p95 **16.1 s**, max 17.0 s; target is p95 <= 30 s                                                                                                                                                                                                                                                                                                                                                      | `ac5-incremental.json`                                                    |
| Corrupt-blob rejection                                | one byte flipped (same size) in the OKE object backing a new asset while the job was paused, then job resumed: kw refused it (`replicated blob failed digest verification: sha256 mismatch ...`), nothing stored (reading it on kw returned 404), checkpoint stayed behind the head, `consecutive_failures` rose to 5; after the object was repaired the job recovered in 28.5 s and kw served the original bytes | `ac5-faults.json`                                                         |
| Source outage                                         | peer URL pointed at an unresolvable host for 40 s: `consecutive_failures` 5 with `last_error`, kw kept serving 3 of 3 seeded reads; after restoring the URL the object published during the outage was visible after **33.2 s** (target <= 60 s, includes the capped backoff)                                                                                                                                     | `ac5-outage.json`                                                         |
| Deletion dry run                                      | an asset deleted on the authority with policy `ignore`: `GET /jobs/{id}/deletions` listed it as `would_delete` (the component as `protected`), applied nothing, and kw kept serving the object (HTTP 200)                                                                                                                                                                                                         | `ac5-incremental.json` `deletion_dry_run`                                 |
| Lag/failure reporting accuracy                        | Failure reporting is accurate (counts, last error, digest mismatch text). Lag reporting is not: see bug 8                                                                                                                                                                                                                                                                                                         | bug 8                                                                     |

Pre-run attempts that did not complete are kept because they document bug 1: `raw/ac5-bug1-*`,
`raw/ac5-prerun-uncontended-seed-status.txt` (the same 350-asset seed completing in about 2 minutes when it did not
have to share the link with the OCI job), `raw/ac5-setup.txt` (first fixture with 16 MiB blobs).

## AC6: backend equivalence

`tools/ac6` runs the identical scenario on each backend; results in `ac6/*.json` (46 steps each, all pass). The 66 MiB
object crosses the 64 MiB S3 part boundary; the corrupted-blob step flips one byte of the stored object directly in
the backend (`tools/s3poke` for S3, the file for disk) and expects the integrity scan to find and later clear it.

| Step group                                                                                            | local disk | kw MinIO | OKE OCI Object Storage |
| ----------------------------------------------------------------------------------------------------- | ---------- | -------- | ---------------------- |
| publish empty, 1 B, 4 KiB, 1 MiB, 8 MiB, 66 MiB                                                       | pass       | pass     | pass                   |
| read back + SHA-256, same six objects                                                                 | pass       | pass     | pass                   |
| ranges: first bytes, middle, across the 64 MiB part boundary, open-ended, suffix, unsatisfiable (416) | pass       | pass     | pass                   |
| immutable coordinate: different content rejected (409), original intact, identical re-put idempotent  | pass       | pass     | pass                   |
| OCI: push 8 MiB layer, digest, pull, `crane validate --remote`                                        | pass       | pass     | pass                   |
| management: unlist, relist, delete, neighbour unaffected, policy `denied` blocks delete (403)         | pass       | pass     | pass                   |
| GC dry run completes and sweeps nothing                                                               | pass       | pass     | pass                   |
| integrity scan full pass clean                                                                        | pass       | pass     | pass                   |
| bit-rot injected: scan records a finding (`digest`), repair, finding clears                           | pass       | pass     | pass                   |

Behaviour recorded identically on all three (info steps): a read of a corrupted blob returns HTTP 200 with the
corrupted bytes (detection is by the scan or by the client's digest check, not on read); an unlisted object stays
readable by direct path. A real GC sweep was not run on the shared instances because the grace period (24 h) protects
fresh blobs and shortening it on shared instances would risk in-flight uploads of other work.

## AC8: benchmarks

Harness: `bench/` (`antbench`, `runner`, `antlib`; see `bench/README.md` for what each scenario asserts and the
measurement caveats). The client runs in a pod in the target cluster against the ClusterIP service; ingress and the
shaped load balancer are not in the path. RSS is `VmRSS`/`VmHWM` of the Ant process sampled each second.

### Warm reads and metadata (W1, W2 warm)

Raw feed, objects published locally, 30 s per row, ClusterIP, no errors in any row.

| Target (image)   | Case                                           | req/s | MiB/s | p50 ms | p95 ms | p99 ms |
| ---------------- | ---------------------------------------------- | ----- | ----- | ------ | ------ | ------ |
| kw (50d26c0)     | package-list metadata at 100 req/s, 16 workers | 100.0 | -     | 5.1    | 6.9    | 12.0   |
| kw               | 64 KiB, c=1                                    | 64.6  | 4.0   | 14.5   | 22.3   | 29.4   |
| kw               | 64 KiB, c=16                                   | 316   | 19.8  | 30.9   | 125    | 154    |
| kw               | 8 MiB, c=1                                     | 16.6  | 133   | 59.8   | 69.2   | 77.1   |
| kw               | 8 MiB, c=16                                    | 34.4  | 275   | 456    | 775    | 962    |
| OKE (8c17785...) | package-list metadata at 100 req/s             | 100.0 | -     | 4.5    | 5.3    | 6.4    |
| OKE              | 64 KiB, c=1                                    | 25.2  | 1.6   | 37.8   | 50.4   | 76.3   |
| OKE              | 64 KiB, c=16                                   | 258   | 16.1  | 59.8   | 83.5   | 107    |
| OKE              | 8 MiB, c=1                                     | 9.3   | 74.7  | 99.8   | 139    | 261    |
| OKE              | 8 MiB, c=16                                    | 51.5  | 412   | 298    | 419    | 586    |

- W1 metadata target (p95 <= 100 ms, p99 <= 300 ms at 100 req/s): **met on both** (database + authorization included,
  client network excluded).
- Warm throughput against the raw backend (`bench/kw-direct.json`, `bench/oke-direct.json`; the same pod reads a
  64 MiB object through Ant and a same-size object directly from the bucket with a pre-signed URL, 20 s each):
  kw Ant/direct = **0.83 (c=1), 0.99 (c=4), 1.01 (c=16)** with the backend saturated near 272 MiB/s; OKE =
  **1.04 (c=1), 0.74 (c=4), 0.79 (c=16)**, so the 80 % target is met at c=1 and missed by 1 to 6 points at c=4/16 on
  OKE.
- Tail latency (observation, bug 9): on kw, 64 cached requests at c=8 (`bench/kw.json` `cold`, warm rows) had p50
  25 to 60 ms but p95 2.6 s and 11.7 s (1 MiB and 4 MiB); OKE max 4.7 s.

### Cold pulls and fan-out (W2)

Remote raw source whose origin is a raw feed on OKE (public TLS; sources require https with a public certificate).
`origin fetches` is an estimate (see caveats in `bench/README.md`); the completion time is the independent check.

| Case                                                | kw                                                                                      | OKE                                                    |
| --------------------------------------------------- | --------------------------------------------------------------------------------------- | ------------------------------------------------------ |
| cold 1 MiB x8 sequential, p50 / p95                 | 1.31 s / 1.59 s                                                                         | 0.27 s / 0.58 s                                        |
| cold 4 MiB x8 sequential, p50 / p95                 | 4.56 s / 4.78 s                                                                         | 0.34 s / 0.39 s                                        |
| warm repeat x64 (c=8): estimated upstream fetches   | 0 and 1                                                                                 | 1 and 3 (background noise)                             |
| fan-out: 32 clients, 1 uncached 4 MiB key, 3 rounds | all 32 identical digests, slowest client 5.3 to 5.7 s, estimated origin fetches 0, 0, 0 | all identical, slowest 0.8 to 1.0 s                    |
| fan-out: 32 clients, 1 uncached 8 MiB key, 3 rounds | all identical, slowest 10.2 to 10.7 s, estimated 0, 0, 0                                | all identical, slowest 1.2 to 1.3 s, estimated 1, 1, 0 |

On kw one object over the 10 Mbit/s origin link takes 3.4 s (4 MiB) or 6.7 s (8 MiB); the slowest of 32 concurrent
clients finished in 5.3 and 10.2 s, which is what a single shared fetch costs, not 32 (about 107 s and 215 s over this
link). That, plus the origin request counter delta (no excess over the measured background of 2 to 3 GETs/s), supports
**at most one upstream fetch per key**. For target OKE the origin and the proxy are the same pod, so only the timing is
indicative. No row had errors; every body matched the published digest.

### Large objects and memory (W3)

Publish and read of 1 and 4 GiB objects through Ant (a 1 to 5 GiB pull-through was not possible: the only trusted
https origin is the 10 Mbit/s OKE edge). Publish declares `X-Checksum-Sha256` (see bug 7 for what happens without it).

| Target        | Case                                    | Result                                                                                                                                                          |
| ------------- | --------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| kw (375611f)  | 1 GiB publish                           | 107 s (9.5 MiB/s), RSS peak sampled 306 MiB                                                                                                                     |
| kw            | 4 GiB publish                           | 416 s (9.9 MiB/s), RSS peak sampled 306 MiB (same as 1 GiB: **no growth with size**)                                                                            |
| kw            | read 1 GiB / 4 GiB, 1 stream            | 264 / 269 MiB/s, digest identical                                                                                                                               |
| kw            | read 1 GiB / 4 GiB, 4 streams           | 275 / 276 MiB/s aggregate, all digests identical; RSS peak 306 / 233 MiB                                                                                        |
| kw            | cancel after 100 MiB, then full re-read | re-read 200, digest identical (1 and 4 GiB)                                                                                                                     |
| OKE (375611f) | 1 GiB publish                           | 834 s (**1.2 MiB/s**), RSS peak sampled 859 MiB, HWM 1.34 GiB                                                                                                   |
| OKE           | read 1 GiB, 1 / 4 streams               | 277 MiB/s / 445 MiB/s aggregate, digest identical                                                                                                               |
| OKE           | 4 GiB publish                           | not completed: a redeploy of the shared instance interrupted it twice; an earlier 4 GiB attempt without a declared digest took 3,386 s and ended in 503 (bug 7) |

W3 target (1 to 100 GiB adds <= 64 MiB peak RSS): met between 1 and 4 GiB on kw (peak 306 MiB at both sizes); on OKE
the slow ingest path holds 0.8 to 1.3 GiB of buffers while publishing a 1 GiB object (within the 1 GiB charged-buffer
bound, but far above kw); idle RSS is 50 to 80 MiB on kw. 10 and 100 GiB were not run.

### Proxy outage, publish, npm (W4, W6)

- Outage: with the origin unreachable, uncached objects fail fast with 503 (11 to 35 ms p95), nothing is poisoned
  (the same objects load after recovery, digest identical). Cached objects: 40 of 40 served (HTTP 200) on kw and
  OKE with the origin made unreachable by revoking the proxy's credential (config unchanged) and, in most runs, by
  re-pointing the source at an unresolvable host. Two kw runs on 375611f (`bench/kw-rerun2.json`,
  `bench/kw-outage-recheck.json`) returned 503 for the cached objects when the outage started less than a second after
  the first response; the harness now waits 3 s and the result is 40 of 40 (`bench/kw-outage.json`). Treat as bug 10.
- Concurrent publish (64 KiB objects, 200 per row): kw 6.9 req/s at c=4 and 7.5 at c=16 (p95 1.4 s and 2.9 s);
  OKE 21.4 and 23.1 req/s (p95 0.25 s and 0.86 s). W4 asks for 10 publishes/s: not met on kw, where throughput does
  not scale with concurrency (bug 12). Duplicate-coordinate writers (32 PUTs, 16 concurrent, same path, different
  bodies): exactly one 201 and 31 409 on both targets, and the final read equals the accepted body.
- npm (real client, `express`+`mocha`, 138 packages, fresh client cache): kw cold 12 s, warm 5 s; OKE cold 12 s,
  warm 4 s. Tarball URLs in packuments carry the public URL, so the client pod needs the CA on kw. After re-pointing
  the source at an unreachable origin a repeated install failed with 503 for the `express` packument on both
  instances (npm retries for about 140 s): previously cached metadata was not served (open question, bug 10).
  cargo was not run.

## AC9: backup, restore, upgrade

`scripts/ac9_backup_restore.sh` and `scripts/ac9_upgrade.sh`; logs and JSON in `ac9/`.

**Backup/restore (kw, online, 11.74 GB: 3,140 blobs, 63 tables, 22,105 rows, 20 secrets).** `ant backup` 109 s
(about 103 MiB/s; first run 122 s for 11.66 GB), `ant backup verify --expect-manifest-sha256` 36 s, a wrong expected
digest is refused. Restore into a scratch database (`ant_scratch`) and scratch blob prefix (`scratch/`) with the
production keyring: **fails without a workaround** (bug 5, `ac9/restore-attempt1-failed.stderr`); with
`session_replication_role = replica` set on the scratch database for the restore session it completed in 436 s
(about 26 MiB/s; first successful run 469 s), keyring check passed for all 20 secrets, schema migrated forward, and
the restored server (started on the same image, workaround removed) reported 36 feeds and 3,140 blobs / 11,738,717,812
bytes, identical to production just before the backup, and served all 350 replicated raw artifacts digest-identical to
the publisher manifest (`ac9/restored-raw-hashes.json`). Scratch database and prefix were removed. Extrapolation, as a
measurement and not a promise: at 26 MiB/s a 100 GiB restore takes about 66 minutes, above the 60 minute target that
presumes 100 MiB/s on the restore path.

**Upgrade (scratch namespace `ant-upg` on kw, scratch DB `ant_upg`, prefix `upg/`).** Chart `deploy/chart`, image
`0.1.0-0cbc6e3` (schema 24) to `0.1.0-50d26c0` (schema 26). Pre-upgrade backup and verify OK; `helm upgrade --wait`
returned after 16 s; the in-cluster 0.5 s poller saw about 9 s without a ready endpoint (single replica, Recreate);
`ant diagnostics` reports `applied_version 26, pending 0`; the 30 seeded raw objects and the OCI image re-read
identical; publishing and the new delete-policy endpoint work. Rollback: the previous image refuses to start on the
upgraded schema (`unsupported schema version or non-contiguous history`); restoring the pre-upgrade backup into a fresh
database with the previous image and serving it returned all 30 objects and none of the post-upgrade write (needs the
same restore workaround; `ac9/ac9-upgrade-runA-rollback-restore-failed.txt` shows it failing without it on a two-feed
instance).

## Defects and observations (product code was not changed)

1. **Replication blob export is cut at 10 s.** `internal/server/server.go:85` sets `WriteTimeout: 10s`; the blob
   handler in `internal/replication/export_http.go` (`blob`, line 143) never extends the write deadline, unlike every
   format adapter (for example `internal/rawrepo/stream.go`). A blob that cannot be sent in 10 s is truncated
   (`transfer interrupted`); destinations on S3 restart from zero, and a page commits only when all its blobs are
   present, so a job livelocks (`raw/ac5-bug1-oke-export-durations.txt`: 128 requests clamped at exactly 10.0 s;
   `raw/ac5-bug1-large-blob-status.txt`: 6 failures, 0 assets applied, 100 MB transferred). With four blob workers
   sharing a 10 Mbit/s link this hit 16 MiB blobs and, when two jobs ran together, 2 to 4 MiB blobs. Workaround used:
   sequential jobs and blobs <= 1.2 MiB.
2. **Deleting a replication job leaves its `replica:<name>` source and dedup domain,** so a new job with the same name
   fails with 409 (`internal/replication/service.go` `CreateJobTx`). Also, disabling a running job does not stop the run
   in progress for about two minutes.
3. **`anonymous_read: true` has no effect on raw feeds:** unauthenticated `GET /feeds/{slug}/raw/...` returns 401
   (`raw/bug3-anonymous-read-raw.txt`). Not checked for other formats.
4. **No DELETE for sources.** Deleting a feed or job leaves source rows behind; this session left `ev5*`, `ac6*`,
   `evb-*`, `replica:*` sources on both instances.
5. **`ant restore` fails on any backup whose `feed_sources` rows fire the replication trigger.** `COPY` into
   `feed_sources` fires `replication_attachments_change` (migration 024) which inserts into `replication_changes` while
   the backed-up `replication_changes` rows are already loaded: `duplicate key value violates unique constraint
"replication_changes_pkey"`. Reproduced with the production backup (schema 31) and with a two-feed instance
   (schema 24). Workaround: `ALTER DATABASE <target> SET session_replication_role = replica` before the restore,
   `RESET` after.
6. **Fresh database: data-plane routes answer `404 page not found` until the pod is restarted after `ant bootstrap`**
   (image 0cbc6e3; the management API works at once; `ac9/ac9-upgrade-attempt3-fresh-db-404.txt`).
7. **Large raw PUT without `X-Checksum-Sha256` is spooled to `/tmp`** (`internal/rawrepo/raw.go`, `put`), which the chart
   limits to a 256 Mi emptyDir. A 1 GiB upload succeeded; 4 GiB ended in 503 on kw (443 s) and OKE (3,386 s,
   `bench/kw-rerun2.json`, `bench/oke-rerun.json`). With the digest declared, 1 and 4 GiB stream straight to storage.
8. **Replication status lag is not trustworthy:** `lag_seconds` and `changes_behind` stayed 0 while the source was
   unreachable and while 100 tag updates were up to 17 s late (`max_lag_seconds_reported_by_status` 0). Main now has a
   "lag from last success" change (`9ba2a9b`); not re-measured.
9. **Warm-read tail latency** on kw (p95 2.6 to 11.7 s at c=8 with p50 25 to 60 ms) and a 4 to 5 s outlier on OKE.
10. **Outage handling is timing- and configuration-sensitive:** cached raw objects returned 503 in two kw runs when the
    outage began within a second of the first response; cached npm packuments returned 503 after the source was
    re-pointed (unclear whether this is cache keyed by origin or stale-if-error not applying to metadata).
11. **Write path to OCI Object Storage is slow:** about 1.2 MiB/s sustained for 1 and 4 GiB while reads from the same
    bucket exceed 270 MiB/s. kw/MinIO ingests at about 9.5 MiB/s from the same code.
12. **Publish throughput on kw plateaus near 7 req/s** regardless of concurrency (OKE about 22 req/s), consistent with
    serialization of commits (the replication sequence takes a per-instance head-row lock); below the W4 10/s target.
13. **Observability gaps:** every non-management route is labelled `unmatched` in `ant_http_requests_total` and in
    request logs; there is no upstream-fetch or cache-hit metric; the feed `pulls` statistic and
    `metrics/traffic` cache counters do not move for raw pull-through.
14. **Resident memory after replication seeds is high:** kw RSS high-water mark 1.0 to 1.1 GiB after the seeds (idle 50
    to 80 MiB), consistent with the 1 GiB transfer-buffer budget but worth watching on the small footprint.
15. **Shared-environment hazard (not a product defect):** a rollout at 03:38 +04 put an older binary on OKE against a
    newer schema and the pod crash-looped for about 5 minutes (`unsupported schema version`).

## Reproducing

```sh
go build -o /tmp/ac5 ./evidence/2026-10-18/tools/ac5 && go build -o /tmp/ac6 ./evidence/2026-10-18/tools/ac6
go build -o /tmp/s3poke ./evidence/2026-10-18/tools/s3poke
go run ./bench/runner -target kw -out /tmp/kw.json                       # AC8
sh evidence/2026-10-18/scripts/ac9_backup_restore.sh out /tmp/s3poke manifest.json   # AC9 part 1 (WORKAROUND=1)
sh evidence/2026-10-18/scripts/ac9_upgrade.sh out deploy/chart 0.1.0-0cbc6e3 0.1.0-50d26c0 ""
```

## Test data and residue

Removed: all `evb-*`, `ac6*`, `ev5*` feeds, the replication jobs and peers, the replicate tokens, the scratch
databases (`ant_scratch`, `ant_upg`, `ant_upg_rb`), the scratch namespace `ant-upg`, the scratch blob prefixes
(`scratch/`, `upg/`, `upg-rb/`), the bench pods, secrets and the backup volume. Remaining and not removable through the API
(bugs 2 and 4): the `ev5*`/`ac6*`/`evb-*` source rows, the roles `ev5-replicate`, `ev5s-replicate`,
`evb-origin-reader*` and principals `ev5*-replicator`, `evb-origin-reader*`. Blobs of deleted feeds are reclaimed by
GC after its 24 h grace period.
