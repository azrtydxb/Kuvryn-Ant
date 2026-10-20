# Conan adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/conan/` for feeds of format `conan`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/conan.md`.

```go
repo, err := conan.New(db, service)
// Mount at /feeds/{slug}/conan/.
```

The feed must have format `conan`; its instance must match the service lease.
Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter.
The Conan reference paths `{name}/{version}/{user}/{channel}` use the Conan 2 recipe
path form; user is the core namespace for reads and writes, and `_` means absent.

## Routes

- `GET/HEAD v1/ping`: capacity announcement with the `revisions` capability.
- `GET/HEAD v2/users/authenticate`, `v2/users/check_credentials`: login and
  credential validation for the Conan 2 client. `authenticate` returns the token
  Conan keeps as its bearer: a service token is echoed, a personal access token is
  returned as itself, a directory password login is exchanged for a 12 hour
  personal access token named `conan remote login` (a new login replaces it).
  The client-auth layer replaces a PAT or directory credential with a single-use
  ticket before the adapter runs, so the adapter must not echo what it received.
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

Concurrent uploads are bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32); per-file limits of 1 MiB for plain metadata and 128 MiB for
tarballs, 128 MiB expanded tar budget, 4,096 manifest members, and an 8-file, 130 MiB
stage cap with a one-hour expiry. Streams use 64 KiB buffers and 30-second progress
deadlines. Reads are private/no-store. Revisions are content-addressed; listing
never silently truncates beyond 4,096 rows.

## Verification

Run from repository root:

```sh
sh internal/conan/test-postgres.sh
```

This follows `scripts/test-postgres.sh`: temporary Unix-socket-only PostgreSQL,
isolated schema, automatic cleanup, and `go test -race -count=1 -v` scoped to this
package. Tests cover recipe and binary-package lifecycle, manifest sealing and
byte-identical downloads, revision monotonicity, idempotent re-uploads, rejected
tampering, package search/listing, and cross-instance isolation.

The real Conan 2 client (login, upload, install, delete, pull-through of a remote) is exercised by `conformance/run.sh local conan`.

## Remote sources (pull-through)

The origin is the server root (for example `https://center2.conan.io`); the Conan v2 API has no absolute URLs, so `v2/conans/...` maps one to one onto the upstream path. Revision listings (`latest`, `revisions`, package id search, file inventories) go through the metadata cache after shape validation. Revision files are immutable and cached on first download. Conan publishes md5, not sha256: `conanmanifest.txt` is verified against the revision in the URL and every other file against the manifest (a file by its md5, a tarball by the md5 of each member) before anything is served or cached. `v2/conans/search` stays local.

Protocol reference: https://docs.conan.io/2/reference/commands/upload.html