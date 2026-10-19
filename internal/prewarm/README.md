# prewarm

Warms a feed's remote cache from a package list, a lockfile, an image list or everything another feed references, now or on a
schedule. Operator documentation: `docs/operations/prewarm.md`.

- `lockfile.go` parses package-lock.json, pnpm-lock.yaml, go.sum, Cargo.lock, requirements.txt and poetry.lock.
- `formats.go` maps an item to the feed's protocol requests (npm, cargo, go, pypi, maven, raw, oci) and checks pinned hashes.
- `inproc.go` / `warm.go` dispatch those requests in process to the feed handlers as the run's principal (`identity.IssueTicket`), so
  adapters apply their normal authorization and `artifact.PullThrough` does the caching; `artifact.WithFillObserver` tells warmed from
  already-cached items.
- `store.go` is the PostgreSQL persistence (migration 043): jobs, runs and per-item checkpoints. `runner.go` is the scheduler and
  executor (bounded concurrency, rate pacing, cancel, resume, retry, abort on a dead upstream). `metrics.go` exposes counters.
- Management API: `internal/management/ops_prewarm.go`; MCP: `internal/mcp/tools_ops.go`; console: `web/src/Prewarm.tsx`.

Test: `scripts/test-postgres.sh ./internal/prewarm/`.
