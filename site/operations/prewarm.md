# Prewarm

Prewarming fills the remote cache of a feed before builds ask for it, so the first CI run after a lockfile change, or after a new runner pool comes up, does not wait on public upstreams. A **job** is a durable definition; a **run** is one execution of it; the per-item rows of a run are the checkpoint a restart resumes from.

Console: **Settings, Cache prewarm**. API: `/api/v1/prewarm/*`. MCP: the `*_prewarm_*` tools (see [MCP tools](../reference/mcp.md)).

## What can be warmed

A job belongs to one feed, which must have a remote source. The job's source says what to warm:

| Source `kind` | Accepted for                                                                                                                                                               | Notes                                                                                                                                                                                                      |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `packages`    | npm, cargo, go, pypi, maven, raw                                                                                                                                           | `name` + `version`. Maven names are `group:artifact[:packaging[:classifier]]`; raw names are a path                                                                                                        |
| `lockfile`    | npm: `package-lock.json` (v1 to v3), `pnpm-lock.yaml` (v5, v6, v9); go: `go.sum`; cargo: `Cargo.lock`; pypi: `requirements.txt` (exact `==` pins, `--hash`), `poetry.lock` | Parsed when the job is created; git, file, path and unpinned entries are ignored and reported, not fatal                                                                                                   |
| `images`      | oci                                                                                                                                                                        | `repository[:tag]` or `repository@sha256:...`, relative to the feed. `options.platforms` selects platforms of multi-platform images (`os/arch[/variant]`, default `linux/amd64`, `all` for every platform) |
| `feed`        | npm, cargo, go, pypi, maven                                                                                                                                                | Every package version another feed of the same format references, in any of its sources, is warmed through the target feed                                                                                 |

`GET /api/v1/prewarm/formats` lists exactly what the running server accepts. A job holds at most 20,000 items; a lockfile may be up to 8 MiB. `POST /api/v1/prewarm/preview` expands a source into items (and the ignored entries with reasons) without creating anything.

## How it works

Each item is requested through the feed's own package-protocol route (for example `/feeds/<slug>/npm/<name>/-/<file>.tgz`), in process, as the principal that owns the run. Authorization, package-owner rules (a locally owned name is never completed from upstream), the pull-through path, upstream integrity checks and negative caching are therefore exactly those of a real client. Prewarm never writes to the store any other way. Response bodies are read to the end so the adapter commits the cache entry.

- **Integrity.** Beyond what each adapter verifies, bytes are compared with what the source pins: npm SRI hashes, cargo checksums, pypi `--hash` and poetry hashes (a pinned hash also selects exactly those files), go.sum `h1:` hashes of the module zip and of `go.mod`, and OCI digests of manifests, configs and layers. A mismatch fails the item with `integrity_mismatch`.
- **Warmed vs cached.** An item is `warmed` when the feed had to fetch something from an upstream and `cached` when everything was already present.
- **PyPI files.** Without pinned hashes and without `options.file_filters`, the pure-Python wheels and source distributions of the version are warmed; a version with no such file is `skipped` (`no_matching_file`). `file_filters` warms only files whose name contains one of the given strings.
- **Pace.** `concurrency` (1 to 32, default 4) items run in parallel and `rate_per_second` (0 for unlimited, default 10) item starts per second. Transient failures get up to three attempts with backoff. 25 consecutive upstream failures stop the run (`failed`; remaining items stay pending for a retry) so a dead upstream is not hammered.
- **Principal.** A run acts as the principal that queued it (for a schedule, the job creator) with that principal's current roles. If that principal is disabled the run fails. Creating or changing a job and starting, cancelling or retrying a run needs permission to configure the feed; reading needs read access.

## Runs, progress and recovery

Run states: `queued`, `running`, `succeeded`, `partial` (some failures, some successes), `failed`, `canceled`. A run exposes totals (`warmed`, `cached`, `failed`, `skipped`, `bytes`) and per-item state with a failure code: `not_found`, `upstream_unavailable`, `integrity_mismatch`, `denied`, `timeout`, `internal`; skip reasons are `no_matching_file`, `unsupported` and `canceled`.

- One active run per job (409 otherwise). Up to 20 finished runs per job are kept.
- **Cancel** stops a queued run at once and a running run within about a second; unfinished items become `skipped` (`canceled`).
- **Retry** queues a new run (`retry_of`) with the failed, cancelled and never-started items of a finished run.
- **Restart.** Item results are written as they happen. A run that was active when the process stopped (or the writer lease moved) is resumed by the next lease holder from the pending items; finished items are not redone.
- Everything runs only under the instance writer lease.

## Schedules

`interval_seconds` (60 seconds to 31 days) schedules recurring runs; the first is due one interval after creation. `enabled: false` pauses the schedule (a manual run is still possible). A tick that finds the previous run still active, or an empty feed source, is skipped and the next one is a full interval later. Job and run lists show `next_run_at` and the last run.

## Examples

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# Warm an npm feed from a lockfile every 6 hours, starting now
jq -n --arg feed "<FEED_ID>" --rawfile lock package-lock.json '{
  feed_id: $feed, name: "web-app lockfile", run_now: true, interval_seconds: 21600,
  source: {kind: "lockfile", lockfile: {type: "package-lock.json", content: $lock}}
}' | curl -fsS -X POST $ANT/api/v1/prewarm/jobs -H "$H" -H "$J" \
  -H 'Idempotency-Key: prewarm-webapp-1' -d @-

# Warm two images for amd64 and arm64 through an OCI pull-through feed
curl -fsS -X POST $ANT/api/v1/prewarm/jobs -H "$H" -H "$J" -H 'Idempotency-Key: prewarm-images-1' -d '{
  "feed_id": "<OCI_FEED_ID>", "name": "base images", "run_now": true,
  "source": {"kind": "images", "images": ["library/alpine:3.20", "library/node:22"]},
  "options": {"platforms": ["linux/amd64", "linux/arm64"]}
}'

# Follow a run
curl -fsS $ANT/api/v1/prewarm/runs/<RUN_ID> -H "$H"
curl -fsS "$ANT/api/v1/prewarm/runs/<RUN_ID>/items" -H "$H"
```

## API summary

| Operation                   | Route                                                                                                         |
| --------------------------- | ------------------------------------------------------------------------------------------------------------- |
| Formats and accepted inputs | `GET /api/v1/prewarm/formats`                                                                                 |
| Preview a source            | `POST /api/v1/prewarm/preview`                                                                                |
| Jobs                        | `GET`/`POST /api/v1/prewarm/jobs`, `GET`/`PATCH`/`DELETE /api/v1/prewarm/jobs/{id}` (revision via `If-Match`) |
| Run now                     | `POST /api/v1/prewarm/jobs/{id}/runs`                                                                         |
| Runs                        | `GET /api/v1/prewarm/runs`, `GET /api/v1/prewarm/runs/{id}`, `GET /api/v1/prewarm/runs/{id}/items`            |
| Cancel / retry              | `POST /api/v1/prewarm/runs/{id}/cancel`, `POST /api/v1/prewarm/runs/{id}/retry`                               |

Mutations take an `Idempotency-Key` and are audited (`prewarm.job.create|update|delete`, `prewarm.run.start|cancel|retry`) in the same transaction. Run completion emits the `prewarm.run.completed` webhook event and scheduled starts `prewarm.run.started`. Metrics: `ant_prewarm_items_total`, `ant_prewarm_bytes_total`, `ant_prewarm_runs_total`, `ant_prewarm_active_runs` (see [Observability](./observability.md#prewarm)).

::: warning Limits

- Only the formats listed above can be prewarmed. Helm, NuGet, RubyGems and the other formats need an adapter-specific resolver and are not accepted yet.
- Only registry pins are compared; Git and URL dependencies are ignored, not fetched.
- Go module zips are verified by hashing them from a temporary file, so very large modules need temporary directory space.
- Metadata (indexes, packuments, project pages) used while warming comes from the normal metadata cache, so a warm that follows an upstream change may see the cached version until its TTL expires.
  :::
