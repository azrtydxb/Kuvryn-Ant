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
