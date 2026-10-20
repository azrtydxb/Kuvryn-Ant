# Conda adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/conda/` for feeds of format `conda`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/conda.md`.

```go
repo, err := condarepo.New(db, service)
// Check err, then configure the externally reachable origin before serving:
err = repo.SetPublicURL("https://packages.example")
// Mount at /feeds/{slug}/conda/.
```

`SetPublicURL` accepts HTTPS or literal loopback HTTP, with no credentials, path,
query or fragment. Host, forwarding headers and request TLS never select URLs.
The feed must have format `conda`; its instance must match the service lease.
Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter.
The channel subdirectory (`linux-64`, `noarch`, etc.) is the core namespace for
reads and writes.

## Routes

- `PUT ingest/{subdir}/{filename}`: adapter-specific admin ingestion of a complete
  `.tar.bz2` package, not a native conda publish command. Optional
  `X-Checksum-Sha256` must be the exact lowercase hexadecimal SHA-256 of the body.
- `GET/HEAD {subdir}/repodata.json` and `current_repodata.json`: generated snapshot index with `sha256` and
  `size` per package plus `repodata_version: 1`.
- `GET/HEAD {subdir}/{filename}`: immutable archive with ETag.

## Ingestion and atomicity

The archive is decompressed for inspection only; no scripts, `post-link`,
`pre-link`, or environment mutation executes. `info/index.json` must match the
filename (`name-version-build.tar.bz2`) and the requested subdir exactly, with
bounded dependency statements. The `archive` and parsed `metadata` assets publish
atomically through `PublishBundle`; metadata refers to the exact archive digest.
Identical retries return 201 without new core events; changed content at the same
name/version/build conflicts. Repodata always reads the selected source's own
metadata and never unions sources by filename.

## Bounds

Concurrent uploads are bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32); 128 MiB compressed / 512 MiB expanded (with a bounded
200:1 expansion cap), 10,000 tar members, 256 KiB metadata and 8 MiB `info/`
budget. Streams use 64 KiB buffers and 30-second progress deadlines. Reads are
private/no-store; enumeration is capped at 4,096 packages.

## Verification

Run from repository root:

```sh
sh internal/condarepo/test-postgres.sh
```

This follows `scripts/test-postgres.sh`: temporary Unix-socket-only PostgreSQL,
isolated schema, automatic cleanup, and `go test -race -count=1 -v` scoped to this
package. Tests cover ingestion, repodata generation, byte-identical downloads,
empty-channel responses, filename/subdir mismatch rejection and isolation.

The real conda and mamba clients (publish through ingest, create, pull-through of the `conda-test` channel) are exercised by `conformance/run.sh local conda`. Local ingestion accepts `.tar.bz2` only; a remote channel may serve `.conda` packages.
Protocol reference: https://conda-forge.org/docs/

## Remote channels and large repodata

A feed with a remote source proxies an upstream channel (see
`remote.go`). `<subdir>/repodata.json` and `current_repodata.json` are read
through the metadata cache while they fit the source's metadata bound
(`cache.max_metadata_bytes`, default the instance bound of 24 MiB). A larger
document (conda-forge `linux-64` is hundreds of MiB) takes the streaming path in
`large.go`: `repodata.json.zst` is preferred upstream and decoded on the fly with a
bounded decoder, entries are validated one at a time and written to the client
(gzip when accepted), so memory does not grow with the document. The stream also
spools a digest index (16-byte filename hash to sha256, sorted runs on disk,
looked up by binary search) in the process temp directory; package downloads use
it to verify against the repodata digest. It expires after 15 minutes and is
rebuilt on demand. A failure after the response started aborts the connection.
`.zst`, `.bz2` and sharded repodata are not served to clients (404, so they fall
back to `repodata.json`).
