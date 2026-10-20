# Git LFS adapter

Integration: construct `New(db, service)`, call `SetPublicURL` with the trusted
external **origin** and mount `/feeds/{slug}/git-lfs/` (done by `cmd/ant/artifacts.go`;
see `site/formats/git-lfs.md`). Feed/source format is `git-lfs`. The lease instance is
fixed at construction. PostgreSQL is the metadata database.

Implemented subset:

- `POST objects/batch`: upload/download, SHA-256 IDs, basic transfer negotiation,
  per-object failures, existing-object upload omission, optional ref ignored.
- Batch-granted `PUT objects/{oid}/upload`, `POST objects/{oid}/verify`, and
  `GET|HEAD objects/{oid}/download`. Verify requires the exact declared oid/size.
- Core Prepare before accepting bytes, ExpectedSource on Publish, immutable
  hash/size verification in private staging before atomic visibility. Verify is
  acknowledgement of an already verified commit, not a delayed visibility gate.
- Read authorization uses Lookup/Open, never global blob existence. Coordinate
  is the oid, namespace `git-lfs`, version `sha256`, asset key `object`. Each oid
  therefore has one package/object owner. Download actions pin owner and size;
  fresh lookups cannot fall through to lower sources.
- Single explicit, open-ended, and suffix ranges; invalid/multipart ranges return 416. Strong ETag If-Range is supported. HEAD returns whole-object headers.

Authentication and action lifecycle:

Basic passwords and Bearer credentials are verified by the shared client-authentication
layer (`internal/identity.ClientAuth`); usernames have no authorization meaning. Browser
session principals are cleared. Readers need read scope and upload/verify clients need
publish plus read scope. Both a valid credential and a cryptographic random `X-LFS-Action`
grant are required for each transfer. Grants bind token ID,
principal, feed, oid, size, operation, source and five-minute expiry. Every request
reauthenticates the service token (including revocation), and core operations
recheck authorization. Credentials are returned only in action headers targeting
the configured trusted origin; URLs contain no credentials, and the adapter emits
no redirects. Never derive SetPublicURL from client headers. TLS is required,
except literal loopback HTTP for fixtures. Configure it before serving traffic.

Grants are process-local, capped at 4096, expire lazily and are not persisted.
Restart or routing a transfer to another handler requires a fresh batch response.
No signing key, home-grown signature scheme, or secret storage is introduced.
If cross-process grants become necessary, integrate a parent-owned keyring/token
interface or persistent grant store before claiming that capability. Actions are
reusable for immutable retries/ranges until expiry; transfers are cancelled at
expiry too. Only the URL plus both returned headers constitutes an action.

Limits: 1 MiB batch JSON, 100 objects per batch, 4 KiB verify JSON, 1 TiB declared
object size, active transfers bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`), 30-second batch context, 30-second
read/write progress deadlines and 64 KiB transfer buffers. Object bytes are never
buffered as a whole. Unsupported transfers/hash algorithms fail explicitly.

Evidence: `go test -race -count=1 ./internal/gitlfs` with a real temporary
PostgreSQL fixture (same initdb/Unix-socket pattern as `scripts/test-postgres.sh`)
and disk storage. A one-connection pool checks nested core call safety. Tests
cover hash/size failures with no visible asset, concurrent immutable retries,
range resume, expiry, grant operation/token/oid binding, scope denial, revocation,
restart, cancellation, source routing changes, owner non-fallback, hostile Host,
and browser principal rejection. Native Git LFS 3.8.0 push/fetch/checkout passed
with isolated HOME/config and exact byte comparison. The binary digest and tested
workflow are recorded in `testdata/client-lock.json`. Tests report the actual
installed binary version/digest rather than silently claiming a version pin.
PostgreSQL integration tests skip unless ANT_TEST_DATABASE_URL is set; setting
ANT_REQUIRE_INTEGRATION=1 makes its absence fatal. The native test skips if
`git-lfs` is absent. No global Git settings or environment changes are made.

The adapter is wired into the server, management API and console like the other formats
and is exercised end to end by `conformance/run.sh local git-lfs`. Not implemented: Git
hosting, LFS locks, custom transfer adapters, resumable uploads, multipart ranges and
Git-ref-specific ACLs.

Protocol references: [v3.8.0 batch API](https://github.com/git-lfs/git-lfs/blob/v3.8.0/docs/api/batch.md)
and [basic transfers](https://github.com/git-lfs/git-lfs/blob/v3.8.0/docs/api/basic-transfers.md).

## Remote sources (pull-through to an upstream LFS server)

A remote source's origin is the upstream LFS endpoint, the URL a client would use as `lfs.url` (for example
`https://lfs.example.com/org/repo.git/info/lfs`; the batch API is `<origin>/objects/batch`). The oid is the SHA-256 of the content, so
every byte is verified against the id the client asked for.

- **Batch (download).** An object that is not stored is looked up with the upstream batch API (`POST objects/batch`, operation
  `download`, basic transfer, sha256). If the upstream has it with the declared size, the client receives Ant's own download action;
  an upstream "not found" is a per-object 404, a different size a per-object 422, an upstream failure a 503. This is the only
  non-GET request ever sent upstream: a bounded, same-origin JSON query (`upstream.Fetcher.Post`, `artifact.Service.RemoteQuery`);
  uploads, verify and every other operation never go upstream.
- **Download.** On first use Ant asks the upstream batch API again for a fresh action (they expire), fetches the object from the
  action href and caches it under the remote source. A plain GET streams to the client while it is staged and the digest is checked
  at the end (a mismatch aborts the transfer and caches nothing); a Range request on a miss first fills the cache completely and is
  then answered from the store, so resumed transfers hit it. HEAD on a miss answers from the grant without fetching.
- **Ownership.** Objects are owned per oid within a feed: an oid held by a local source is never answered from upstream.

Limits: only the `basic` transfer; the upstream action's `header` map is not forwarded (the source credential is sent only to the source
origin), so upstreams that need per-action headers, or an Authorization on another host, cannot be proxied; an action href on a host
other than the source origin must be https and a configured redirect origin; the batch response is limited to the metadata limit; LFS
locks, custom transfer adapters and refs are not proxied.
