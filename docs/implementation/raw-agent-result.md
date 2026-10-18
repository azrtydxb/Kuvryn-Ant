# Raw implementation handoff

Implemented only new `internal/rawrepo` files plus this requested handoff. No dependencies added; no main/config/module/OpenAPI edits, staging, commits, or GitHub operations.

Integration: `repo, err := rawrepo.New(db, artifactService)` returns `(*Repository, error)`. Mount as an HTTP handler at `/feeds/{slug}/raw/` without stripping the prefix. It authenticates Bearer service tokens with shared identity middleware, binds requests to the service lease's instance, and resolves only raw feeds. No Close lifecycle is required.

Contract:

- PUT uses shared Prepare and Publish, with ExpectedSource pinned before body consumption. Coordinate and asset key are the full normalized relative path, namespace is its parent directory, and version is the constant `first`. Path identity is limited to the core's 256-byte coordinate limit. Traversal, encoded separators, nested escaping, control characters, empty segments, and trailing slashes are rejected.
- Optional `X-Checksum-Sha256` accepts lowercase SHA-256 hex. A declared digest plus known Content-Length streams directly through the core's verified staging. Otherwise a private 0600 temporary file computes size/digest using 64 KiB buffers; unknown-length/chunked uploads are supported. Declared lengths and digests are verified. Temporary files are removed on every normal error/success path. There is no arbitrary object-size ceiling. Four concurrent upload slots bound adapter memory; disk capacity/quota remains an operator concern and abnormal process termination can leave temporary files for OS cleanup.
- New PUT returns 201, identical replay 204, conflicting bytes or media type 409. GET and HEAD use shared Lookup/Open and check opened record identity against the resolved record. Strong digest ETags, If-Match, weak If-None-Match, If-Range, single/suffix/open-ended/multipart ranges, and 416 are supported. HEAD ignores Range and returns no body. Files use attachment disposition and nosniff, with private/no-store caching.
- Conditional PUT, partial PUT and encoded request bodies return 501; DELETE and other unsupported methods return 405 with Allow. Atomic overwrite/delete policy operations are not implemented. No browse/search, remote fetching, replica synchronization, WebDAV, or client-certification claims.
- ResponseController clears the control server's fixed read/write deadlines. Body reads and response writes renew 30-second idle deadlines; copy buffers are 64 KiB. No database transaction or row iterator is retained while streaming or calling the shared artifact service.

Validation completed: `go test -race -count=1 -v ./internal/rawrepo` with an isolated temporary PostgreSQL 17 database and real disk blob store; all tests passed. `go vet ./internal/rawrepo` passed. Coverage includes conditions/ranges, digest/length mismatch, unknown-length upload, cancellation invisibility, immutable conflicting concurrent PUT, audit uniqueness, scoped and foreign-instance credentials, independent path ownership, traversal, and idle deadline renewal. The temporary PostgreSQL instance was stopped and removed.

Verified against official references on 2026-10-18:

- https://www.rfc-editor.org/rfc/rfc9110.html — conditional evaluation, GET-only Range, and safe HTTP behavior.
- https://pkg.go.dev/net/http#ServeContent — standard library conditional/range support; explicit HEAD Range correction is tested.

Parent follow-up: mount handler and document the optional checksum header and unsupported conditional writes. Existing core services provide transactional metadata, authorization, immutable conflict handling, auditing and outbox publication; this adapter has no artifact SQL write bypass.
