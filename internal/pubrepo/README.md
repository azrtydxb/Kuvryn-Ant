# Local Dart Pub adapter

Integration: `New(pool, artifactService)`, configure `SetPublicURL` with the
trusted external origin, mount `/feeds/{slug}/pub/`, and call `Close` after HTTP
shutdown. Feed/source format is `pub`. Only this package was added; the parent
must register the adapter. No dependencies or migrations are added.

Implemented subset:

- Bearer service tokens (`dart pub token add <hosted-url>`); browser principals
  cannot authenticate protocol requests. No fabricated OAuth endpoints.
- Native hosted v2 GET session, multipart POST upload, GET finalize flow.
  The random upload URL is a short-lived upload capability; the native client
  does not supply Authorization on that POST. Finalization requires the exact
  initiating token, revalidated by the shared authentication middleware.
- Archive and JSON pubspec commit together through `PublishBundle`. Same-byte
  retries succeed; changed bytes at an existing version conflict. Metadata and
  downloads use core authorization and package-owner resolution.
- Package/version JSON, semantic-version latest selection, SHA-256 archive
  hashes, GET/HEAD archive delivery. Public URLs never derive from request Host,
  forwarded headers, or TLS state.
- Gzip/tar and YAML validation: valid package/version, root pubspec, no links,
  traversal, duplicate members, YAML aliases, excessive nesting, or corrupted
  gzip. No extraction or execution. Limits: 32 MiB compressed, 128 MiB expanded,
  10,000 archive members, 256 KiB pubspec, 4 active transfers, 64 sessions,
  4,096 versions and 8 MiB accumulated pubspecs per metadata response.
  Upload/download progress deadlines are 30 seconds with 64 KiB buffers;
  finalization has a 30-second context budget.

Sessions expire after ten minutes and are in-memory with private temporary files;
expired files are collected on new-session requests and all files on `Close`.
An abrupt process exit can leave OS temporary files. Restart invalidates all
uncommitted sessions. There is no claim of durable/resumable staging.

The native initiation request does not identify a package. Preflight calls
`Prepare` with coordinate `pub-upload`, namespace empty, asset `archive` before
accepting bytes. Finalization re-routes the validated package and both asset keys
and requires the same pinned source. Thus this subset supports a default target;
package-specific routing to a different source fails closed. Pub package
coordinates currently use the empty namespace, consistent with flat ownership.

## Evidence

Run from repository root: `sh internal/pubrepo/test-postgres.sh`.
This follows the shared temporary PostgreSQL script: isolated schema per test,
Unix-socket-only temporary server, one connection per test pool, `go test -race`.
It runs native Dart when installed, with temporary token/config/cache directories.
No credentials are saved in this package or printed by the fixture.

2026-10-18 local macOS arm64 evidence: all six tests passed with real PostgreSQL
and disk storage; `go vet ./internal/pubrepo` passed. Installed Dart 3.13.1
performed authenticated native publish and fresh-cache consumption with a SHA-256
lockfile. The installed executable digest is recorded in `client-evidence.json`.
Tests also exercise pre-finalize invisibility, different-token denial, malformed
archives, cancellation, failed storage with no visible assets, concurrent
conflicting publication, immutable retries, higher-owner no-fallback, scoped auth,
and untrusted Host rejection.

## Remaining F18 / Phase 1 gates

This is not full F18 conformance. Dart 3.13.5 and Flutter 3.47.0 pinned baseline
runs, multi-package dependency-chain fixtures, MinIO/OKE and AWS-compatible
backend evidence, remote cold/warm/freshness behavior, replication/offline
closure, retraction/discontinuation lifecycle, deletion/retention/search,
namespace bindings, durable staging/restart recovery, download ranges, adapter
metrics and management integration remain. Anonymous read is not implemented.
Retraction/discontinuation state is not fabricated. All 26 formats remain in
Phase 1 scope; this local subset does not reduce that scope or require a provider
fleet.

Protocol reference: https://github.com/dart-lang/pub/blob/master/doc/repository-spec-v2.md
