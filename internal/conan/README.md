# Conan local adapter

Development subset of F06, not full Phase 1 conformance. Only this package is
implemented here; parent integration must mount the handler. No migrations,
management API, main wiring or dependency changes are required by this package.

```go
repo, err := conan.New(db, service)
// Mount at /feeds/{slug}/conan/ using the parent router.
```

The feed must have format `conan`; its instance must match the service lease.
Every request uses a scoped machine Bearer token or HTTP Basic password containing
that token. Browser principals are discarded. Anonymous access is not implemented.
The Conan reference paths `{name}/{version}/{user}/{channel}` use the Conan 2 recipe
path form; user is the core namespace for reads and writes, and `_` means absent.

## Routes

- `GET/HEAD v1/ping`: capacity announcement with the `revisions` capability.
- `GET/HEAD v2/users/authenticate`, `v2/users/check_credentials`: token echo and
  credential validation for the Conan 2 client.
- `GET/HEAD v2/conans/search?q=...`: recipe search returning compact references.
- `GET/HEAD v2/conans/{ref}/latest`, `revisions`, `revisions/{rrev}/files`:
  revision and file inventory metadata.
- `GET/HEAD v2/conans/{ref}/revisions/{rrev}/packages`, `packages/{pid}/latest`,
  `packages/{pid}/revisions`: binary package metadata.
- `GET/HEAD v2/conans/{ref}/revisions/{rrev}/files/{file}`: recipe file download.
- `PUT v2/conans/{ref}/revisions/{rrev}/files/{file}`: recipe file upload.
- `PUT v2/conans/{ref}/revisions/{rrev}/packages/{pid}/revisions/{prev}/files/{file}`:
  binary package file upload.

## Revision identity and atomicity

Conan 2 delivers revision files one by one, with `conanmanifest.txt` last. This
adapter stages the pending set on disk, pins the target source through `Prepare`,
and commits the whole revision with one `PublishBundle` when the sealed manifest
arrives. The recipe revision is the MD5 of the sorted `name: md5` manifest lines;
package revisions follow the same rule. If any staged file disagrees with the
manifest, or the manifest's own digest does not match the requested revision, the
stage is discarded and nothing becomes visible.

A re-upload of a previously published revision merges the published assets and is
idempotent. Different bytes under the same requested revision can never seal.
Tarball members are hashed individually (`conan_sources.tgz` under an
`export_source/` prefix), so a modified member changes the manifest and therefore
the revision identity.

## Bounds

Two concurrent uploads, per-file limits of 1 MiB for plain metadata and 128 MiB for
tarballs, 128 MiB expanded tar budget, 4,096 manifest members, and an 8-file, 130 MiB
stage cap with a one-hour expiry. Streams use 64 KiB buffers and 30-second progress
deadlines. Reads are private/no-store. Revisions are content-addressed; listing
never silently truncates beyond 4,096 rows.

## Verification and remaining gates

Run from repository root:

```sh
sh internal/conan/test-postgres.sh
```

This follows `scripts/test-postgres.sh`: temporary Unix-socket-only PostgreSQL,
isolated schema, automatic cleanup, and `go test -race -count=1 -v` scoped to this
package. Tests cover recipe and binary-package lifecycle, manifest sealing and
byte-identical downloads, revision monotonicity, idempotent re-uploads, rejected
tampering, package search/listing, and cross-instance isolation.

The package has not been exercised against the real Conan 2.33.0 client; native
client verification, remote/replica resolution, rebuild/cache graphs, retention and
deployment evidence remain release gates. All 26 formats remain Phase 1 scope.
Protocol reference: https://docs.conan.io/2/reference/commands/upload.html