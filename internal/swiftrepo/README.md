# Swift Package Manager registry adapter

Development subset of F24 (SE-0292 registry v1). No migrations or dependency changes.

```go
repo, err := swiftrepo.New(db, service)
err = repo.SetPublicURL("https://packages.example")
// Mount at /feeds/{slug}/swift/. Feed format must be `swift`.
```

Auth: Bearer token or Basic password containing a scoped token; no anonymous access.
Every request needs `Accept: application/vnd.swift.registry.v1+json` (400 missing/invalid, 415 other version).
Errors are `application/problem+json`.

Routes (relative to the mount): `GET {scope}/{name}` (releases, Link latest-version),
`GET {scope}/{name}/{version}` (metadata, signing info), `GET .../{version}/Package.swift[?swift-version=]`
(alternate manifests via Link), `GET .../{version}.zip` (Digest, Content-Disposition),
`GET identifiers?url=`, `PUT {scope}/{name}/{version}` (multipart `source-archive`, `metadata`,
optional `source-archive-signature`/`metadata-signature` with `X-Swift-Package-Signature-Format`,
optional `X-Checksum-Sha256`). Publish is one atomic bundle (archive, release record, manifests) and
immutable: identical retry is 201, different bytes 409.

Limitations: publish is synchronous (201, never 202); DELETE returns 405 (no delete in the core
service yet); identifier lookup scans up to 4096 release records (no index); signatures are stored
and returned but not cryptographically verified; no `.json` suffix negotiation beyond package list.
Test: `sh internal/swiftrepo/test-postgres.sh`.

## Remote sources (pull-through)

The remote source origin is the registry root (the URL a client passes to `swift package-registry set`). The package owner is
decided per `scope.name` before any upstream request: a locally published package is never completed from upstream.

- The release list (`GET {scope}/{name}`) and release resource (`GET {scope}/{name}/{version}`) are read through the metadata
  cache, validated (version, id, source-archive resource with a SHA-256 checksum) and re-rendered with feed URLs. While the
  upstream is reachable its list replaces the locally cached view; during an outage the cached releases are listed (stale
  responses carry `X-Kuvryn-Cache: stale`).
- The source archive is fetched on the first `GET .../{version}.zip`, verified against the checksum of the upstream release
  resource before the first byte is served, and cached together with a release record that keeps the upstream signing data.
  `X-Swift-Package-Signature`/`-Format` and `Digest` are re-served from it, so SwiftPM verifies publisher signatures against the
  publisher's key itself; Ant does not sign or verify signatures.
- `Package.swift` is cached on first read.

Limits: only the default `Package.swift` is proxied (alternate `Package@swift-x.y.swift` manifests are announced in `Link`
headers, which the runtime does not expose, so `?swift-version=` receives the default manifest); `identifiers?url=` is answered
from local releases only; release lists are limited to the metadata limit and 4096 releases.
