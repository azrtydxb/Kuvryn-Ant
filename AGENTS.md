# Kuvryn Ant — agent contract

Self-hosted artifact platform: Go service (`cmd/ant`, `internal/*`), React console (`web/`, embedded via `internal/console/dist`), PostgreSQL metadata, disk or S3 blob storage. Product scope: `kuvryn-ant-product-spec.md`; plans: `docs/planning/`.

## Build and verify

- `go build ./... && go vet ./...`
- `scripts/test-postgres.sh [packages]` — DB-backed tests against a disposable local PostgreSQL (requires `initdb`/`pg_ctl`).
- `scripts/check-generated.sh` — `api/openapi.yaml` must match `api/generated.go` and `web/src/generated.ts`. After any API change, run the Go generator and `cd web && npm run generate`; never hand-edit generated files.
- `cd web && npm ci && npm test && npm run build` — `build` includes the API contract check and `tsc`. Rebuild `internal/console/dist` when `web/` changes.

## Conventions

- Format adapters live in `internal/<format>` with integration tests, a README and their own `test-postgres.sh`, and are mounted in `cmd/ant/artifacts.go` (`/feeds/{slug}/<format>/`).
- Migrations in `internal/database/migrations` are numbered contiguously; rebase and renumber if a number is taken.
- Management API: `/api/v1`, cursor pagination, structured errors, `If-Match` revisions, `Idempotency-Key` on mutations, audit in the same transaction.
- Remote-source credentials are encrypted per source and never returned; CA references are operator-configured IDs, never request-supplied paths. Publishes never go upstream.
- Never commit secrets. `.gitleaks.toml` allowlists only `api/generated.go` (generated embedded spec).
- `api/openapi.yaml` is JSON content read with `JSON.parse`; do not reformat it as YAML.

## Delivery

- Work on a branch, open a PR to `main`. CI is currently bypassed by owner decision: verify locally (build, vet, touched tests, check-generated), then merge with `gh pr merge --merge --admin`.
- No AI attribution lines in commits or PRs.
