# Contributing to Kuvryn Ant

Thank you for your interest in Kuvryn Ant. This guide covers the development setup,
the project conventions, and how changes get reviewed and merged. By participating
you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md).

- Questions and ideas: [GitHub Discussions or issues](SUPPORT.md)
- Security problems: never open a public issue, see [SECURITY.md](SECURITY.md)
- Documentation: <https://azrtydxb.github.io/Kuvryn-Ant/> (sources in [`site/`](site/))

## Development setup

Requirements:

- Go 1.27.2 or newer in the 1.27 series (see `go.mod`)
- Node.js 22 and npm (web console and documentation site)
- Docker with Compose, and OpenSSL
- PostgreSQL 17 or 18 binaries (`initdb`, `pg_ctl`, `createdb`) for the DB-backed tests
- Optional: `crane` for the OCI smoke test, Helm 4 for chart checks

Run the service locally:

```sh
scripts/init-dev.sh                      # one time: generates .local/ secrets (never committed)
docker compose up -d --wait postgres     # PostgreSQL on 127.0.0.1:15432
scripts/run-dev.sh                       # go run ./cmd/ant with the local settings
scripts/run-dev.sh bootstrap local       # prints a 24h administrator token once
```

The console is at <http://127.0.0.1:8080/console/>. `scripts/run-dev.sh bootstrap-token`
issues a replacement token.

## Build and verify

Run these before opening a pull request:

```sh
go build ./... && go vet ./...
test -z "$(gofmt -l api cmd internal)"
go test -count=1 ./...                    # DB tests skip without a database
scripts/test-postgres.sh [packages]      # DB-backed tests against a disposable PostgreSQL
scripts/check-generated.sh               # API contract matches generated code
cd web && npm ci && npm test && npm run build
cd site && npm ci && npm run docs:build  # when you change documentation
```

Useful variants:

- One Go test against real PostgreSQL:
  `scripts/test-postgres.sh ./internal/oci/... -run TestName`. Without
  `ANT_TEST_DATABASE_URL`, DB tests skip silently; set `ANT_REQUIRE_INTEGRATION=1`
  to make a missing database fail instead.
- One web test: `cd web && npx vitest run src/App.test.tsx`.
- Real-client OCI smoke test: `go run ./scripts/ocismoke`.
- Real-client conformance for all formats: `conformance/run.sh local [formats...]`
  (see [conformance/README.md](conformance/README.md)).

## Project conventions

These rules come from [AGENTS.md](AGENTS.md), which is the authoritative contract
for both human and automated contributors.

- **One artifact core, many adapters.** `internal/artifact` owns publication, read
  resolution, policies and deletes. A format adapter in `internal/<format>` only
  translates its protocol onto that core and must not bypass it for writes.
  Adapters ship with integration tests and a README, and are mounted in
  `cmd/ant/artifacts.go` under `/feeds/{slug}/<format>/`.
- **Migrations.** Files in `internal/database/migrations` are numbered
  contiguously and are forward-only. If your number is taken when you rebase,
  renumber your migration; never edit a migration that has been merged.
- **Generated code.** `api/openapi.yaml` is the management API contract. It is JSON
  content read with `JSON.parse`; do not reformat it as YAML. After changing it run
  `go generate ./api` and `cd web && npm run generate`, then
  `scripts/check-generated.sh`. Never hand-edit `api/generated.go` or
  `web/src/generated.ts`.
- **Console build.** The production console is committed in
  `internal/console/dist` and embedded in the binary. Rebuild it
  (`cd web && npm run build`) whenever `web/` changes, and commit the result.
- **Management API.** `/api/v1` with cursor pagination, structured errors,
  `If-Match` revisions, `Idempotency-Key` on mutations, and audit records written in
  the same transaction as the change.
- **Security rules.** Remote-source credentials are encrypted per source and never
  returned. CA references are operator-configured IDs, never request-supplied paths.
  Publishes never go upstream. Never commit secrets; `.gitleaks.toml` allowlists
  only the generated embedded spec.
- **Documentation.** Use placeholders in docs and examples: `ant.example.com`,
  `10.0.0.x`, `user@example.com`. Do not commit real hostnames, internal IP
  addresses or account identifiers.

## Commit style

Commits follow [Conventional Commits](https://www.conventionalcommits.org/) with an
optional scope naming the package or area, in the imperative mood, lower case:

```text
feat(prewarm): durable prewarm jobs, runs and schedules for remote caches
fix(health): keep an idle source degraded after an isolated trailing error
docs(go): allow storage.googleapis.com redirects for proxy.golang.org remotes
build: rebuild console dist for federation tabs
chore: renumber storage pressure migration to 045
```

Common types: `feat`, `fix`, `docs`, `test`, `build`, `ci`, `chore`, `refactor`,
`perf`. Keep the subject under about 72 characters and explain the why in the body.

### Developer Certificate of Origin

Every commit must be signed off to certify the
[Developer Certificate of Origin](https://developercertificate.org/):

```sh
git commit -s -m "fix(npm): keep dist-tags on republish"
```

This adds a `Signed-off-by: Your Name <user@example.com>` trailer using your Git
identity. Pull requests with unsigned commits cannot be merged; fix them with
`git rebase --signoff main`.

## Pull request process

1. Open an issue first for larger changes or new formats so the design can be
   agreed (use the format-support request template for new ecosystems).
2. Create a branch from `main` (`feat/...`, `fix/...`, `docs/...`).
3. Make focused commits, add or update tests, and update documentation in `site/`
   and the adapter README when behaviour changes.
4. Run the verification commands above and fill in the pull request template.
5. A maintainer reviews the change. CI (`gate` in `.github/workflows/ci.yml`) must
   pass. Address review comments with new commits; maintainers merge with a merge
   commit.
6. User-visible changes get an entry under `Unreleased` in
   [CHANGELOG.md](CHANGELOG.md).

## Licensing

Kuvryn Ant is licensed under the [Apache License 2.0](LICENSE). By contributing you
agree that your contributions are licensed under the same terms.
