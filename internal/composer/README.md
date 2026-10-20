# Composer adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/composer/` for feeds of format `composer`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/composer.md`.

```go
repo, err := composer.New(db, service)
// Check err, then configure the externally reachable origin before serving:
err = repo.SetPublicURL("https://packages.example")
// Mount at /feeds/{slug}/composer/.
```

`SetPublicURL` accepts HTTPS or literal loopback HTTP, with no credentials, path,
query or fragment. Host, forwarding headers and request TLS never select URLs.
The feed must have format `composer`; its instance must match the service lease.
Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter.
The vendor portion of `vendor/package` is the core namespace for reads and writes.

## Routes and ingestion

- `GET/HEAD packages.json`: Composer 2 metadata URL discovery.
- `GET/HEAD p2/{vendor}/{package}.json`: unminified stable version metadata.
- `GET/HEAD p2/{vendor}/{package}~dev.json`: empty versions for an authorized,
  existing stable package; unknown packages return 404.
- `GET/HEAD dist/{vendor}/{package}/{version}/archive.zip`: immutable archive.
- `PUT ingest/{vendor}/{package}/{version}`: adapter-specific admin ingestion of
  the raw ZIP request body, not a native Composer publish command or the future
  management API staged-session protocol. Optional `X-Checksum-Sha256` must be
  the exact lowercase hexadecimal SHA-256 of the uploaded ZIP.

The ZIP must have exactly one root `composer.json` or one within a single wrapper
directory containing the whole archive. Name and version must match the URL.
Accepted manifest fields: `name`, `version`, optional `type` (library only),
`description`, `require`, `require-dev`, `autoload` (PSR-0/PSR-4 maps), and `source`.
Unknown fields are rejected rather than silently stripped, including scripts,
plugins, repositories, user-supplied dist, and unsupported Composer schema fields.
Only lowercase vendor/package names and canonical stable `major.minor.patch`
versions are supported. Constraints accept numeric/wildcard versions, comparison,
caret and tilde operators, whitespace/comma conjunction and `||` alternatives;
branch names, stability flags, aliases and hyphen ranges are not supported.
Dependency keys accept package names, `php`, and `ext-*`/`lib-*` platform names.
Dependency solver satisfiability is left to Composer; dependencies need not
already exist in this feed. PSR paths must be relative and backed by the archive.

Optional source references are HTTPS git URLs without userinfo/query/fragment and
with a bounded nonempty reference. They remain external metadata, are never
fetched/executed by the server, and are not evidence of offline source closure.
The generated dist points only to this feed; Composer's SHA-1 `shasum` is computed
from the ZIP, while core storage and retained references use SHA-256.

Both asset routes are authorized with `Prepare` before reading the upload body.
Their selected source is pinned through `ExpectedSource`. `PublishBundle` exposes
dist and metadata atomically; metadata retains a reference to the exact dist.
Identical retry returns 201 without new core events; changed content returns 409.
`Lookup`/`Open` enforce the first package owner and namespace authorization. A
missing version or asset never falls through to another source. Enumeration closes
PostgreSQL rows before invoking the service (tested with a one-connection pool).
The core owns audit/outbox and storage behavior.

## Bounds

Uploads use private temporary files, ingestion limited by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32), a two-minute
processing context, 128 MiB compressed / 512 MiB expanded bounds, 10,000 ZIP entries,
8 MiB central directory, 1 KiB names / 32 path levels and 1 MiB manifest. All entries
are decompressed with cancellation and CRC verification. ZIP64, symlinks/special
files, traversal, case-insensitive duplicates, file/directory collisions, split
roots, duplicate JSON keys and excessive expansion ratios are rejected. Metadata
JSON depth is bounded; PSR paths are capped at 1,024. Metadata listing is capped at
1,024 versions and 8 MiB stored metadata with a 15-second context. Streams use
64 KiB chunks and 30-second progress deadlines. Reads are private/no-store; no
conditional cache or range support is claimed.

## Verification

Run from repository root:

```sh
sh internal/composer/test-postgres.sh
```

This follows `scripts/test-postgres.sh`: temporary Unix-socket-only PostgreSQL,
isolated schema, automatic cleanup, and `go test -race -count=1 -v` scoped to this
package. Tests include real Composer update and locked install after removing
vendor/cache, Basic/Bearer and namespace denial, foreign instance isolation,
trusted URLs, immutable concurrency, injected storage failure with no partial
publication, package-owner shadowing, checksum mismatch and unsafe archives.
Client credentials live in a temporary mode-0600 auth file; client environment and
cache are isolated. No shared environment or user Composer configuration changes.
The client test skips when Composer is absent and reports the actual version/hash.
`conformance.lock.json` records the observed Composer 2.9.7/PHP 8.5.6 run; it does
not claim the contract's 2.10.3 baseline. Fuzz seed coverage runs with ordinary
tests; an extended fuzz campaign has not been run.

The real Composer client (update, locked install, Packagist pull-through, delete) is exercised by `conformance/run.sh local composer`. Not covered: the full Composer schema and constraint grammar for ingested packages (branch names, stability flags, aliases), management API ingestion sessions and an extended fuzz campaign.

## Remote sources (pull-through)

The metadata template comes from the upstream `packages.json` (`metadata-url`, default `/p2/%package%.json`). `p2/<vendor>/<pkg>.json` and the `~dev` file are read through the metadata cache, expanded from Composer's minified form and re-rendered without `source` (a VCS URL would send clients around the feed) and with every `dist.url` pointing at this feed. The feed's `dist/.../archive.zip` resolves the upstream `dist.url` again from the cached metadata, so the client never chooses the upstream URL. Packagist publishes no archive digest, so an archive is checked to be a well-formed ZIP of bounded size and, when the upstream states a SHA-1 `shasum`, against it. Dist URLs usually live on another host (GitHub `api.github.com`, redirecting to `codeload.github.com`): add those origins to the source's redirect origins; the source credential is never sent there. A package owned by a local source is never answered from upstream.

Protocol reference: https://getcomposer.org/doc/05-repositories.md
