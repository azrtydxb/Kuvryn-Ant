# Local Dart Pub adapter

Integration: `New(pool, artifactService)`, `SetPublicURL` with the trusted external
origin, mounted at `/feeds/{slug}/pub/` by `cmd/ant/artifacts.go`, and `Close` after HTTP
shutdown. Feed/source format is `pub`. See `site/formats/pub.md`.

Implemented subset:

- Bearer tokens (`dart pub token add <hosted-url>`), verified by the shared
  client-authentication layer; browser principals cannot authenticate protocol
  requests. No fabricated OAuth endpoints.
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
  10,000 archive members, 256 KiB pubspec, transfers bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32), 64 sessions,
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

## Further routes and remote sources

`DELETE api/packages/{name}/versions/{version}` deletes a version through the core (gated by the feed's delete policy); `PUT` and `DELETE` on `/retract` retract and restore it (the package document marks it `retracted`, and `latest` skips it). With a remote source, the upstream package document is read through the metadata cache, validated and re-rendered with every `archive_url` pointing at this feed; archives are fetched on first download from the `archive_url` the upstream names, verified against `archive_sha256` before the first byte is served and cached, with the pubspec beside them so cached versions stay listable during an outage.

Not implemented: durable or resumable staging (sessions are per replica and in memory), download ranges, `discontinued` lifecycle state. The real Dart client (3.13.5 in the conformance harness) is exercised by `conformance/run.sh local pub`.

Protocol reference: https://github.com/dart-lang/pub/blob/master/doc/repository-spec-v2.md
