# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

The shared agent contract (build/verify commands, conventions, delivery rules) lives in AGENTS.md and applies here:

@AGENTS.md

## Running things

- Local stack: `scripts/init-dev.sh` (one-time: generates `.local/` secrets), `docker compose up -d --wait postgres`, then `scripts/run-dev.sh` (wraps `go run ./cmd/ant` with `ANT_DATABASE_URL` + `ANT_ROOT_KEY_FILE`). Console at `http://127.0.0.1:8080/console/`.
- First admin credential: `scripts/run-dev.sh bootstrap local` (prints a 24h token once); `scripts/run-dev.sh bootstrap-token` reissues one.
- Other `ant` subcommands (the usage line in `cmd/ant/main.go` is authoritative): `bootstrap-token`, `rotate-secrets`, `gc [--dry-run]`, `backup`/`restore`, `storage`, `diagnostics`, `repair metadata [--dry-run]`.
- Single Go test against real PostgreSQL: `scripts/test-postgres.sh ./internal/oci/... -run TestName` (args pass straight to `go test -race -count=1`). Without `ANT_TEST_DATABASE_URL`, DB tests skip silently; `ANT_REQUIRE_INTEGRATION=1` makes them fail instead.
- Single web test: `cd web && npx vitest run src/App.test.tsx`.
- Regenerate API code: `go generate ./api` (oapi-codegen via `api/codegen.yaml`) and `cd web && npm run generate`; `scripts/check-generated.sh` verifies without rewriting.
- Real-client smoke test for OCI: `go run ./scripts/ocismoke` (needs crane + PostgreSQL binaries). Real-backend browser QA fixture: `go run ./cmd/qafixture start|stop`.

## Architecture

**One artifact core, many protocol adapters.** `internal/artifact` owns publication (staged, digest-verified, atomic commit of components/assets/tags with audit + outbox in one transaction), read resolution, policies, deletes and the remote pull-through seam (`remote.go`: `PullThrough`/`Materialize` for bytes, `RemoteMetadata` for mutable index documents). Each `internal/<format>` package only translates its ecosystem protocol onto that core; it must not bypass it for writes. `cmd/ant/artifacts.go` constructs every adapter and mounts it.

**Feeds, sources, policies.** A feed is the stable client endpoint for one format. Sources are `local`, `remote` or `replica`. A feed's read policy orders its sources (local owner of a package wins; a remote 404 falls through, a remote error stops the search so a private outage can't resolve from a public source). Its publish policy resolves every write to exactly one local source. `internal/routing` + `artifact/explain.go` implement this, exposed as `GET /feeds/{id}/resolve`.

**Remote fetching** is layered: `internal/remoteconfig` (normalized origin/cache policy, credentials encrypted per source UUID via `internal/secrets`), `internal/remoteruntime` (coalesced misses, metadata + negative cache, stale-if-error, health), `internal/upstream` (HTTP client, bearer-token handshake, origin-checked redirects). Credentials never leave their source's origin/realm.

**Storage.** `internal/blob` (disk + S3 implementations, digest/size verification, resumable/multipart) behind `internal/storage` (registry, active-backend routing, disk↔S3 migration). Metadata is PostgreSQL only; blobs are content-addressed and never deleted inline — `internal/retention` does two-phase mark/sweep GC with row-lock re-checks, plus retention policies and integrity scans.

**Instance ownership.** A process holds a database writer lease (`internal/ownership`) for its instance; background workers (`cmd/ant/workers.go`: webhooks, GC, replication, audit purge/sink) run only under that lease.

**Management plane.** `internal/management` serves `/api/v1` (contract in `api/openapi.yaml`; the generated `api/generated.go` types are used server-side). `internal/identity` provides principals, persisted roles, scoped service tokens and auth middleware; browser login is in `internal/oidclogin`, `internal/samllogin`, `internal/directory`. `internal/mcp` exposes MCP over `/mcp` by calling the same management handlers in-process, so authz and audit stay identical. `internal/events` delivers webhooks from the outbox.

**Replication** (`internal/replication`): a gapless, trigger-maintained change sequence per instance; the destination pulls manifests/changes/blobs from a peer, verifies digests, checkpoints per page in one transaction, and serves the result through a read-only replica source attached to the destination feed.

**Console.** `web/` (React + TypeScript 7 + Vite) uses the vendored Azrty design system in `web/src/ds/` and talks only to the generated client in `web/src/generated.ts` via `web/src/api.ts`. The production build is committed to `internal/console/dist` and embedded in the binary (prettier-ignored; rebuild it whenever `web/` changes).

## Planning sources

`kuvryn-ant-product-spec.md` is the product scope. Per-format contracts are `docs/planning/contracts/F01–F26.md`; milestone exit criteria are in `docs/planning/delivery-plan.md`; Phase 2 is `docs/planning/phase-2-federation.md`. Adapter-specific behavior and known limits are in each `internal/<format>/README.md` and `docs/implementation/` (notably `pull-through.md`, `oci-mirror.md`). Deployment: `deploy/chart` (Helm; examples for OKE and kw in `deploy/examples/`) and `docs/operations/`.
