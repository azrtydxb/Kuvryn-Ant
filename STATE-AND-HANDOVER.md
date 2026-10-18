# Kuvryn Ant — State and Handover

Date: 2026-10-18 · Branch: `feat/service-foundation` · Last commit: `a1ee0cc` "Implement atomic artifact adapters, enterprise sessions, console and deployment"

Everything below exists in the **uncommitted working tree** (~50 files). Nothing on this branch except the three prior commits is committed. A future session should review, split, and commit this work deliberately — see [Handover for the next session](#handover-for-the-next-session).

## 1. What this phase delivered

Backend adapter work, the remote-source management contract, and the administration console all reached a green, verified state. The prior session left four broken adapters (`conan`, `ansiblerepo`, `condarepo`, `cran`), an untested `remoteruntime` fetch/cache layer, and a web console with three failing tests. All of that is now complete and verified.

### Backend

- **Format adapters implemented and mounted** in `cmd/ant/artifacts.go` + `cmd/ant/main.go`, each with integration tests (`internal/<pkg>/integration_test.go`), a README, and a `test-postgres.sh` fixture:
  - `internal/conan` — stage/search/upload/download/metadata, manifest-sealed atomic bundles
  - `internal/ansiblerepo` — Galaxy v3 collection index (detail, versions, version detail)
  - `internal/condarepo` — channel/repodata publication
  - `internal/cran` — package index + source/binary metadata
  - plus the earlier `gitlfs`, `pubrepo`, `composer`, `rubygems` adapters
- **OCI / package discovery**: `internal/oci/referrers.go` (OCI 1.1 referrers API), `internal/artifact/subject.go` (immutable subject locks with `ON CONFLICT` retry-identity), migrations `016_package_discovery.sql` (namespace backfill) and `017_artifact_subjects.sql`, and `internal/management/packages.go` (package list/versions/tags/assets search with namespace partition authorization and cursor scope binding).
- **Remote-source management contract**: `internal/remoteconfig` (normalized origin/cache policy, source-bound encrypted credential purposes, operator-only CA refs), `internal/management/remote_sources.go` (GET/PUT remote-config, PUT remote-credential, revision + If-Match), migration `018_remote_source_configs.sql`, and `internal/config/upstream.go` (operator CA bundles).
- **Key rotation**: `cmd/ant/rotate.go` + `rotate_test.go` (resumable root-key rotation, batch rewraps, replay-safe fingerprint derivation via HMAC of the root key).
- **Remote runtime layer**: `internal/remoteruntime` — fetch/cache integration tests covering metadata caching, negative caching, credential forwarding/rotation reload, config-revision/CA reload, and request bounds. (Materialization is still not wired to the adapters — see §3.)
- **OG tag**: `api/openapi.yaml` + `api/generated.go` (Go) + `web/src/generated.ts` (TS) are in sync; enforced by `scripts/check-generated.sh`.

### Administration console (`web/` + `internal/console`)

- New views from the run-3 increment: `web/src/Packages.tsx` (package discovery with prefix/namespace/digest filters), `web/src/RemoteSource.tsx` (remote-config + per-source encrypted credential forms), `web/src/Webhooks.tsx` (create/rotate/disable, delivery history), plus the earlier Feeds/Sources/Identity views.
- Fixed three failing tests:
  - `web/src/api.ts` — the SHA-256 replay fingerprint was awaited before `fetch` on every call, racing dispose/abort in `web/src/api.test.ts` ("aborts in-flight requests when session ends"). The digest now runs only for idempotent operations, so GETs reach the network synchronously and abort correctly.
  - `web/src/App.test.tsx` — the new `Packages` view's `listPackages` request fell through the mock to the bare `feed` object, crashing the render (`r.value.data.items.map`) and unmounting the tree before tests reached "Add rule" / "Attach a source". Added an empty-page mock branch for `/packages`.
- Real-backend browser QA recorded in `web/README.md` (§ "Run 3"): created feeds/sources, attached at priority, saved publish policy, pushed a real OCI image through `/v2/` and verified package discovery + filters, configured a remote source with encrypted credentials (secret never returned), created a webhook and opened delivery history. All flows green.

## 2. Verification status

All of the following pass on this working tree:

```sh
go build ./...        # clean
go vet ./...          # clean
scripts/check-generated.sh   # generated code in sync
cd web && npm test    # 24/24
cd web && npm run build       # contract check + tsc + vite
cd web && npx prettier --check src/
go test ./internal/console/...            # embedded assets, routing, headers
scripts/test-postgres.sh                  # full DB-backed suite (40+ packages)
```

Scoped `go test` passed for: `internal/{conan,ansiblerepo,condarepo,cran,gitlfs,pubrepo,composer,rubygems,oci,console,configuration,management,remoteconfig,remoteruntime,secrets,artifact}`.

## 3. Known gaps / not done

- **Remote materialization is wired for raw, npm, pypi, maven, go and OCI** (see `docs/implementation/pull-through.md`). The other adapters still serve local content only and can be enabled with the two documented hooks. Replica resolution remains a release gate.
- **Formats remaining** (8 of 26 are still not implemented): APK, APT, CocoaPods, HuggingFace, p2, Swift, Terraform, RPM. Implemented (18): ansible, cargo, composer, conan, conda, cran, git-lfs, go, helm, maven, npm, nuget, oci, oci-artifacts, pub, pypi, raw, rubygems.
- **Phase-1 backlog still open**: replication, retention/GC (failed metadata commits can leave unreferenced verified bytes), backups/restore, provider administration, release packaging/qualification, deployment evidence. Target: OKE + MinIO on TrueNAS SCALE; AWS and compatible providers supported without a separate fleet.
- **Web console**: live OIDC/SAML/LDAP provider end-to-end auth is not exercised (covered only by frontend tests with mocked fixtures).
- **Review rounds that previously failed mid-run were re-run in this session** (config-review-r2, protocol-review-r2). Both produced no blocking findings; no code changes were required from them.

## 4. Handover for the next session

1. **Commit discipline**: ~50 files are uncommitted on `feat/service-foundation`. Review `git status`/`git diff`, split into coherent commits (adapters, management contract + migrations, rotate, console, docs), and never commit secrets. `.local/`, `.qa-fixture.json`, and fixture artifacts are gitignored.
2. **Merge/PR**: continue on `feat/service-foundation`; open the PR against the base when the intended parent is known. If a PR already exists, check `gh pr status` before creating another.
3. **Next work options (in no order)**:
   - Wire `remoteruntime` materialization into adapters (closes the largest remaining gate).
   - Next format adapters per the delivery order still in the product spec (APK, APT, CocoaPods, HuggingFace, p2, Swift, Terraform, RPM), following the per-adapter standard: integration tests via `scripts/test-postgres.sh`, README, `test-postgres.sh`, and mounting in `cmd/ant/artifacts.go` + `main.go` (`SetPublicURL` wiring).
   - Replication, retention/GC, backups/restore, release packaging.
4. **When touching the API**: run both `npm run generate` in `web/` and the Go generator, keep `generated.go`/`generated.ts` in sync with `openapi.yaml`, and gate on `scripts/check-generated.sh`.
5. **QA fixture**: `go run ./cmd/qafixture start|stop` is the shared real-backend browser QA harness (disposable socket-only Postgres, bootstrapped token, loopback server). T3 preview or ego-browser can drive it; record evidence under "Real backend QA evidence" in `web/README.md`.
6. **Known fact to preserve**: remote-source credentials are stored encrypted per source with a purpose bound to the exact source UUID; the API returns only an opaque `credential_id`, never the secret. CA refs are operator-configured IDs, never request-supplied paths. Keep those invariants when wiring materialization.