# R / CRAN adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/cran/` for feeds of format `cran`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/cran.md`.

```go
repo, err := cran.New(db, service)
// Check err, then configure the externally reachable origin before serving:
err = repo.SetPublicURL("https://packages.example")
// Mount at /feeds/{slug}/cran/.
```

`SetPublicURL` accepts HTTPS or literal loopback HTTP, with no credentials, path,
query or fragment. Host, forwarding headers and request TLS never select URLs.
The feed must have format `cran`; its instance must match the service lease.
Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter.
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

Concurrent uploads are bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32); 32 MiB package cap, 128 MiB expanded budget, 10,000 tar
members, 256 KiB DESCRIPTION and 8 MiB of index metadata; index reads fail at
4,096 candidates. Streams use 64 KiB buffers and 30-second progress deadlines.
Reads are private/no-store.

## Verification

The integration tests in this package follow `scripts/test-postgres.sh` using an
isolated temporary schema; run them with:

```sh
sh internal/cran/test-postgres.sh
```

Tests cover ingestion, index generation, byte-identical downloads, conflict
behavior under concurrency, and package-owner isolation.

R's `install.packages` against a feed, and pull-through of CRAN, are exercised by `conformance/run.sh local cran`. Local ingestion covers source packages only.

## Remote sources (pull-through)

Ownership is per package name: a name that exists locally is never fetched from upstream, and the merged `PACKAGES` index takes each name from exactly one side. `src/contrib/PACKAGES` and `PACKAGES.gz` read the upstream `PACKAGES.gz` through the metadata cache and merge it with the local index (`PACKAGES.rds` is not offered; R falls back to `PACKAGES.gz`). Source tarballs (current and `Archive/`) and binary packages under `bin/<platform>/contrib/<rver>/` are cached on first download. CRAN publishes only MD5 sums, which the seam cannot verify, so these fills are length checked but not hash verified.
Protocol reference: https://cran.r-project.org/doc/manuals/r-release/R-admin.html