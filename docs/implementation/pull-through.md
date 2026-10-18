# Pull-through reads (remote sources)

Pull-through is implemented once, in `internal/artifact` (`remote.go`), on top of
`internal/remoteruntime`. Adapters add a small hook to their existing read path.
Enabled today: raw, npm, pypi, maven, go, oci (blobs and manifests by digest, tags,
tag lists). `cmd/ant/artifacts.go` builds the `remoteruntime.Manager` from the
secret keyring and `ANT_UPSTREAM_CA_MAP_FILE` and sets `artifact.Service.Remote`;
without a keyring pull-through is off and feeds serve local content only.

## Semantics

- **Package-owner first.** Before any upstream request the seam looks up the owner
  of the coordinate in the feed. A local (or unreadable) owner means not found, with
  no upstream request: a private package can never be completed from a public
  source. A remote owner restricts the search to that source. With no owner, the
  feed's remote sources are tried in priority order.
- **Not-found falls through, errors do not.** A 404/410 moves to the next source.
  Any upstream error stops the search (503 to the client) so an outage of a private
  source cannot resolve the name from a lower-priority public one.
- **Immutable artifacts** are cached into the artifact store under the serving
  remote source (dedup domain `source:<id>`, created on first use), as ordinary
  components/versions/assets with `provenance.origin = remote`, audit action
  `artifact.materialize` and outbox topic `artifact.materialize`. The next read is a
  local hit through the unchanged `Lookup`/`Open` path (ranges, conditionals, offline).
- **Misses stream.** The client receives bytes while they are staged; the cache entry
  is committed only on a clean EOF with the declared length. Truncation or any error
  aborts the staged bytes and the client connection. If the adapter supplies
  `Origin.Digest` (OCI, PyPI files) the object is fully staged and verified first and
  then served from the store, so the client never sees bytes that miss the digest.
  Range and conditional headers are ignored on a miss (a full 200 is always valid).
- **Coalescing.** Concurrent misses for one asset share one upstream request; followers
  wait for the commit and are served from the store.
- **Mutable documents** (indexes, packuments, `maven-metadata.xml`, version lists, OCI
  tags) are not persisted. They go through the runtime's bounded metadata cache (TTL,
  negative cache, stale-if-error per source) and the adapter rewrites them. Staleness is
  visible: `X-Kuvryn-Cache: stale` plus `Warning: 110`; an upstream-outage fallback to
  the locally cached view is marked `X-Kuvryn-Cache: offline`.
- **Invariants.** Credentials are attached only for the source origin; absolute URLs
  (for example a PyPI file CDN) must be the source origin or a configured redirect
  origin, and never receive the credential. Credentials are never returned or logged.
  CA references are operator IDs resolved from the operator map. Only GET is ever sent
  upstream, and `Publish`/`Prepare` accept only local sources.

## Enabling another adapter

1. **Artifact reads.** On `artifact.ErrNotFound` from `Lookup`/`Open`, build an
   `artifact.Origin` (`Path` relative to the source origin via `artifact.EscapePath`, or
   `URL` for an absolute reference resolved with `artifact.ResolveReference`; set `Digest`
   when the metadata gives a sha256) and call
   `service.PullThrough(ctx, principal, publication, origin)`:
   - `(stream, false, nil)`: `stream.ServeStream(w, method)`
   - `(nil, true, nil)`: repeat the normal local read
   - `(nil, false, err)`: map as usual (`ErrNotFound` 404, `ErrUnavailable` 503)

   Set `Publication.MediaType` for deterministic types, otherwise the upstream
   `Content-Type` is used. If the metadata served earlier carries a source token, pass it
   as `Origin.SourceID` so bytes come from the same source as the metadata.
2. **Metadata.** Call `service.RemoteMetadata(ctx, principal, publication, origin)` before
   the local composition. `ErrNotFound`: continue locally. `ErrUnavailable`: continue locally
   as the degraded offline view, and report 503 instead of 404 if that also finds nothing.
   On success rewrite every absolute upstream URL to the feed URL and mark stale responses
   with `artifact.StaleHeaders()`. Never concatenate local and remote indexes: one owner
   serves a package.
3. Add an integration test with `internal/artifact/remotetest` (TLS upstream, CA map,
   credential, feed/source helpers); see the `remote_test.go` files of the enabled adapters.

## Limitations

- Remote registries that require a token handshake (Docker Hub style `WWW-Authenticate`
  realms) are unsupported; only the source's static `authorization`/`api_key` header is sent.
- npm tarballs are fetched from `<name>/-/<base>-<version>.tgz`, not from the URL in the
  upstream document; PyPI serves only wheels and `.tar.gz` sdists with a stated sha256, and
  the all-projects `/simple/` index is local only; Maven timestamped snapshots remain unsupported.
- Metadata and negative caches are in-process (not shared across instances, lost on restart);
  cached artifacts are durable. Metadata bodies are limited to 4 MiB.
- Upstream latency/hit-rate metrics and per-source retry budgets beyond the fetcher's
  circuit breaker are not exposed yet.
- A cache fill is not rolled back if the source's configuration changes mid-stream; the
  next read re-resolves ownership.
