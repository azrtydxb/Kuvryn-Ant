# Git LFS local protocol adapter (F09)

Integration: construct `New(db, service)`, call `SetPublicURL` with the trusted
external **origin**, then mount `/feeds/{slug}/git-lfs/`. Feed/source format is
`git-lfs`. This package does not change server wiring, migrations, or dependencies.
The lease instance is fixed at construction. PostgreSQL is the metadata database.

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
- Single explicit, open-ended, and suffix ranges; invalid/multipart ranges return
  416. Strong ETag If-Range is supported. HEAD returns whole-object headers.

Authentication and action lifecycle:

Basic passwords and Bearer credentials are shared machine service tokens;
usernames have no authorization meaning. Browser session principals are cleared.
Readers need read scope and upload/verify clients need publish plus read scope.
Anonymous access is disabled. Both a valid service credential and a cryptographic
random `X-LFS-Action` grant are required for each transfer. Grants bind token ID,
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
object size, four active transfers per handler, 30-second batch context, 30-second
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

Remaining release gates: integration into the main server/API/UI; remote
pull-through and freshness; authoritative replica seed/incremental/offline
closure; namespace binding semantics supplied by core; shared cleanup, search,
explain/health/metrics integration; MinIO and compatible AWS storage evidence;
large-production-object throughput and fault testing. OKE + MinIO remains the
primary deployment target; no provider environment fleet is created. All 26
formats remain Phase 1 requirements. This local evidence is not full F09 or
shared-contract conformance. Git hosting, LFS locks, custom transfer adapters,
resumable uploads, multipart ranges and Git-ref-specific ACLs are not implemented.

Protocol references: [v3.8.0 batch API](https://github.com/git-lfs/git-lfs/blob/v3.8.0/docs/api/batch.md)
and [basic transfers](https://github.com/git-lfs/git-lfs/blob/v3.8.0/docs/api/basic-transfers.md).
