# F11 classic Helm adapter implementation

Implemented 2026-10-18 in `internal/helmrepo` only, plus this explicitly requested handoff. No staging, commits, main/API/module edits or GitHub writes.

## Parent integration

- Construct `helmrepo.New(db, artifactService) *helmrepo.Repository`; mount requests beginning `/feeds/{slug}/helm/` without stripping the prefix. Feed/source format is `helm`.
- Uses existing `artifact.Service.Prepare`, `PublishBundle`, `Lookup`, `Open`, identity service-token authentication/roles, and PostgreSQL. No direct artifact metadata writes or storage bypass.
- Uses existing `go.yaml.in/yaml/v3` and `golang.org/x/mod/semver`; no additional dependency is needed. Parent may promote those existing module requirements from indirect to direct.
- Basic username plus service-token password and Bearer service tokens are accepted. Administrative ingestion requires feed-resource `Admin` plus the shared artifact `Publish` authorization. Reads use shared artifact authorization.

## Routes and behavior

- `POST /feeds/{slug}/helm/charts/{name}/{version}`: multipart fields `chart` and optional `provenance`; filenames must be `{name}-{version}.tgz` and `{name}-{version}.tgz.prov` respectively. Chart, original Chart.yaml and optional provenance form one immutable atomic bundle. Exact retries are supported by PublishBundle; different/partial bundles conflict.
- `GET|HEAD /feeds/{slug}/helm/index.yaml`: deterministic YAML, descending semantic versions, SHA-256 digest, original chart metadata and source/digest-bound relative URLs. Package owners and their versions are selected in one PostgreSQL statement. Rows are closed before any service call, so pool connections are not held across streaming/service work.
- `GET|HEAD /feeds/{slug}/helm/charts/{name}/{version}/{source}/{chart-sha256}/{name}-{version}.tgz[.prov]`: pinned source and chart digest are checked. Unpinned `charts/{name}/{version}/{filename}` also resolves through shared ownership. Provenance occupies the chart URI with `.prov` appended, compatible with Helm.
- Download SHA-256 ETags/checksum headers, conditional GET, HEAD, single ranges, suffix ranges, If-Range and 416 responses.
- Uploads spool to private temporary files and remove them on completion/failure. No extraction or execution occurs. Archive checks reject unsafe paths, duplicate members, links/special files, sparse metadata, invalid gzip CRC/truncation, hidden trailing nonzero archives, oversized metadata, aliases/deep YAML, and coordinate/version mismatch.
- Provenance cleartext identity and declared archive SHA-256 are checked; original provenance bytes are stored untouched. Signature verification is not implemented or asserted (`signatureVerified: false`), and no signing/trust keys are invented.
- Upload/download streaming overrides control-plane deadlines using ResponseController and 30-second progress/idle deadlines with 64 KiB buffers. Four concurrent ingestion slots bound buffering. Chart/expanded defaults use the representable int64 range rather than a small object cap; operator-configurable Limits also bound metadata, provenance, members and index entries. Metadata defaults: 1 MiB, provenance 2 MiB, 10,000 members/index entries.

## Evidence

Passed `go vet ./internal/helmrepo` and targeted `go test -race -count=1 -v ./internal/helmrepo` against a freshly initialized temporary PostgreSQL 17 server. Each integration test also creates/drops its own schema. Temporary server stopped and removed afterward. No whole-suite run.

Tests cover archive/YAML/coordinate safety, provenance mismatch, atomic three-asset publication, exact retries/partial conflict, unauthorized and read-only tokens, invalid archive invisibility, exact chart/provenance download bytes, digest conditional GET/HEAD/ranges, package-owner index selection, forbidden missing-version fallback, and stale source-bound downloads.

Real installed Helm client successfully executed package, authenticated repo add/update, pull with exact digest/bytes, dependency update using the authenticated repository, and local template rendering of both chart and dependency. No cluster install was performed.

Client actually tested: `v4.1.1+g5caf004`, binary `/opt/homebrew/bin/helm`, SHA-256 `9f7e2bfe4f0b8d9a746f509645307553fd0ff37cd764b9f91ee8dbe73a7489e4`. This is **not** certification of the planning baseline Helm v4.3.1. `TestRealHelmClientPostgres` logs the installed version and skips when Helm is absent. PostgreSQL tests use ANT_TEST_DATABASE_URL and honor ANT_REQUIRE_INTEGRATION.

## Official protocol references checked

- https://helm.sh/docs/topics/chart_repository/ — classic index fields, packaged chart files and URLs.
- https://helm.sh/docs/topics/provenance/ — two YAML documents in clearsigned provenance, chart archive checksum, and same chart URI with `.prov` suffix.
- https://helm.sh/docs/topics/charts/ — Chart.yaml identity/version and archive layout.

## Remaining scope outside this adapter slice

OCI Helm remains with the existing OCI adapter. Remote pull-through, replicated materialization, management staged-ingestion sessions/UI, configured anonymous reads, namespace binding policies beyond shared Lookup, cluster installation, MinIO execution and cryptographic provenance trust verification are not certified by these tests. This adapter consumes locally materialized shared service records. No full F11 release-conformance claim is made.
