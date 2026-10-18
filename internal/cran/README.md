# R / CRAN local adapter

Development subset of F20, not full Phase 1 conformance. Only this package is
implemented here; parent integration must mount the handler and configure the
trusted origin. No migrations, management API, main wiring or dependency changes
are required by this package.

```go
repo, err := cran.New(db, service)
// Check err, then configure the externally reachable origin before serving:
err = repo.SetPublicURL("https://packages.example")
// Mount at /feeds/{slug}/cran/ using the parent router.
```

`SetPublicURL` accepts HTTPS or literal loopback HTTP, with no credentials, path,
query or fragment. Host, forwarding headers and request TLS never select URLs.
The feed must have format `cran`; its instance must match the service lease.
Every request uses a scoped machine Bearer token or HTTP Basic password containing
that token. Browser principals are discarded. Anonymous access is not implemented.
The fixed source index channel `src/contrib` is the core namespace for reads and
writes.

## Routes

- `PUT src/contrib/{package}_{version}.tar.gz`: adapter-specific admin ingestion of
  a source package archive, not a native R publish/`tools::write_PACKAGES` command.
  Optional `X-Checksum-Sha256` must be the exact lowercase hexadecimal SHA-256 of
  the body.
- `GET/HEAD src/contrib/PACKAGES` and `PACKAGES.gz`: generated index with
  `Package`, `Version`, `Depends`, `Imports`, `LinkingTo`, `Suggests`, `Enhances`,
  `License`, `OS_type`, `Archs`, `MD5sum` and `NeedsCompilation`.
- `GET/HEAD src/contrib/{package}_{version}.tar.gz`: immutable archive with ETag.

## Ingestion and atomicity

`DESCRIPTION` is parsed as a single bounded DCF record (folded fields preserved)
without executing R code, running installation hooks or touching the package
library. Package and version must match the URL exactly, with mandatory `Title`,
`Description` and `License`. The `archive` and generated index metadata publish
atomically through `PublishBundle`. Identical retries return 201 without new core
events; different bytes at the same package/version conflict. Indexes are
regenerated snapshots of the selected source's own metadata; the newest published
version of each package wins, and enumeration never unions sources by filename.

## Bounds

Four concurrent uploads, 32 MiB package cap, 128 MiB expanded budget, 10,000 tar
members, 256 KiB DESCRIPTION and 8 MiB of index metadata; index reads fail at
4,096 candidates. Streams use 64 KiB buffers and 30-second progress deadlines.
Reads are private/no-store.

## Verification and remaining gates

The integration tests in this package follow `scripts/test-postgres.sh` using an
isolated temporary schema; run them with:

```sh
sh internal/cran/test-postgres.sh
```

Tests cover ingestion, index generation, byte-identical downloads, conflict
behavior under concurrency, and package-owner isolation.

The package has not been exercised against the real R `install.packages` / `pak`
client; native client verification with the pinned R baseline, binary packages
(`bin/...` channels), `PACKAGES` layout parity, remote/replica resolution,
retention and deployment evidence remain release gates. All 26 formats remain
Phase 1 scope.
Protocol reference: https://cran.r-project.org/doc/manuals/r-release/R-admin.html