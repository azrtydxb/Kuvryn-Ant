# Cache prewarming

Prewarming (spec 6.5, issue #43) fills the remote cache of a feed before builds ask for it. A **job** is a durable definition; a
**run** is one execution; the per-item rows of a run are the checkpoint a restart resumes from. Console: Settings, Cache prewarm.
API: `/api/v1/prewarm/*`. MCP: the `*_prewarm_*` tools. Code: `internal/prewarm`.

## What can be warmed

A job belongs to one feed, which must have a remote source. The source says what to warm:

| Input | Accepted for | Notes |
| --- | --- | --- |
| Package list | npm, cargo, go, pypi, maven, raw | `name` + `version` (maven: `group:artifact[:packaging[:classifier]]`, raw: a path) |
| Lockfile | npm: `package-lock.json` (v1-v3), `pnpm-lock.yaml` (v5, v6, v9); go: `go.sum`; cargo: `Cargo.lock`; pypi: `requirements.txt` (exact `==` pins, `--hash`), `poetry.lock` | Parsed when the job is created; git, file, path and unpinned entries are ignored and reported, not fatal |
| Image list | oci | `repository[:tag]` or `repository@sha256:...`, relative to the feed; `options.platforms` (default `linux/amd64`, `all` for every platform) |
| Another feed | npm, cargo, go, pypi, maven | Every package version the other feed (same format) references, in any of its sources, is warmed through the target feed |

Other formats cannot be prewarmed yet (`GET /api/v1/prewarm/formats` lists exactly what this server accepts). At most 20,000 items
per job; a lockfile may be 8 MiB. `POST /api/v1/prewarm/preview` expands a source without creating anything.

## How it works

Each item is requested through the feed's own protocol route (for example `/feeds/{slug}/npm/{name}/-/{file}.tgz`), in process, as the
principal that owns the run. So authorization, package-owner rules (a locally owned name is never completed from upstream), the
pull-through seam (`artifact.Materialize`/`PullThrough`), upstream integrity checks and negative caching are exactly those of a real
client; prewarm never writes to the store by any other path. Bodies are read to the end so the adapter commits the cache entry.

- **Integrity.** Beyond what each adapter verifies, bytes are compared with what the source pins: npm SRI hashes, cargo checksums, pypi
  `--hash`/poetry hashes (a pinned hash also selects exactly those files), go.sum `h1:` hashes of the module zip and of go.mod, OCI
  digests of manifests, configs and layers. A mismatch fails the item (`integrity_mismatch`).
- **Warmed vs cached.** An item is `warmed` when the feed had to fetch something from an upstream and `cached` when everything was already
  present.
- **PyPI files.** Without pinned hashes and without `options.file_filters`, pure-Python wheels and source distributions of the version
  are warmed; a version with no such file is `skipped` (`no_matching_file`).
- **Pace.** `concurrency` (1-32, default 4) items in parallel and `rate_per_second` (0 unlimited, default 10) item starts per second.
  Transient failures get up to three attempts with backoff; 25 consecutive upstream failures stop the run (`failed`, remaining
  items stay pending for a retry) so a dead upstream is not hammered.
- **Principal.** A run acts as the principal that queued it (for a schedule, the job creator) with that principal's current roles. If
  that principal is disabled the run fails. Creating or changing a job and starting, canceling or retrying a run needs permission to
  configure the feed; reading needs read access.

## Runs, progress and recovery

States: `queued`, `running`, `succeeded`, `partial` (some failures, some successes), `failed`, `canceled`. A run exposes totals
(`warmed`, `cached`, `failed`, `skipped`, `bytes`) and per-item state with a failure code (`not_found`, `upstream_unavailable`,
`integrity_mismatch`, `denied`, `timeout`, `internal`; skip reasons `no_matching_file`, `unsupported`, `canceled`).

- One active run per job (409 otherwise). Up to 20 finished runs per job are kept.
- **Cancel** stops a queued run at once and a running run within about a second; unfinished items become `skipped` (`canceled`).
- **Retry** queues a new run (`retry_of`) with the failed, canceled and never-started items of a finished run.
- **Restart.** Item results are written as they happen. A run that was active when the process stopped (or the writer lease moved) is
  resumed by the next lease holder from the pending items; finished items are not redone.
- Everything runs only under the instance writer lease.

## Schedules

`interval_seconds` (60 s to 31 days) schedules recurring runs; the first is due one interval after creation. `enabled=false` pauses
the schedule (a manual run is still possible). A tick that finds the previous run still active, or an empty feed source, is skipped and
the next one is a full interval later. Job and run lists show `next_run_at` and the last run.

## API summary

| Operation | Route |
| --- | --- |
| Formats and accepted inputs | `GET /prewarm/formats` |
| Preview a source | `POST /prewarm/preview` |
| Jobs | `GET/POST /prewarm/jobs`, `GET/PATCH/DELETE /prewarm/jobs/{id}` (revision via `If-Match`) |
| Run now | `POST /prewarm/jobs/{id}/runs` |
| Runs | `GET /prewarm/runs`, `GET /prewarm/runs/{id}`, `GET /prewarm/runs/{id}/items` |
| Cancel / retry | `POST /prewarm/runs/{id}/cancel`, `POST /prewarm/runs/{id}/retry` |

Mutations take an `Idempotency-Key` and are audited (`prewarm.job.create|update|delete`, `prewarm.run.start|cancel|retry`) in the same
transaction; run completion emits the `prewarm.run.completed` event (webhooks) and scheduled starts `prewarm.run.started`.
Metrics: see [observability](observability.md#prewarm-metrics).

## Limits

- The formats above only; helm, nuget, rubygems and the other formats need an adapter-specific resolver and are not accepted yet.
- Only registry pins are compared; Git/URL dependencies are ignored, not fetched.
- Go zips are verified by hashing them from a temporary file; very large modules need the temporary directory space.
- Metadata (indexes, packuments, project pages) used while warming comes from the normal metadata cache, so a warm that follows an
  upstream change may see the cached version until its TTL expires.
