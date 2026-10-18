# npm local registry adapter

`New(db, artifactService) (*Registry, error)` constructs the handler for
`/feeds/{slug}/npm/`. Before serving, call `registry.SetPublicURL(ANT_PUBLIC_URL)`
with the externally configured origin. Only HTTPS or literal-loopback HTTP is
accepted; userinfo, paths (including `/`), queries and fragments are rejected.
An unset origin returns 503 for authenticated requests. Host, TLS state and
forwarding headers never determine tarball links. The handler is bound to the
artifact service lease's instance and requires explicit npm Bearer credentials;
browser context cannot substitute for machine authentication.

Implemented local operations:

- `PUT /{package}`: one version, one base64 attachment and one initial dist-tag.
  Scoped names work as `@scope%2fname` or `@scope/name`.
- `GET/HEAD /{package}`: package-owner packument, including `dist-tags`.
- `GET/HEAD /{package}/{version-or-tag}`: one manifest.
- `GET/HEAD /-/package/{package}/dist-tags`: tags for the selected package owner.
- `GET/HEAD /{package}/-/{basename}-{version}.tgz`: authorized tarball bytes.

Publication validates normalized lowercase names, strict SemVer, attachment
length/base64, SHA-1 shasum, SHA-512 integrity, and matching archive package.json.
Archive traversal, links/devices and noncanonical paths are rejected. The
metadata and exact tarball bytes commit in one immutable `PublishBundle` with a
verified reference. `Prepare` authorizes before reading the upload and pins the
local source. Identical retries succeed; conflicting bytes or attempts to move
an existing tag return 409 with no partially visible version. Metadata stores no
uploader-controlled tarball destination. Generated tarball URLs carry the source
identity; a later owner change fails closed. Index generation uses only one
owner and checks authorization on each manifest; versions never fall through.
Database rows are closed before shared artifact/storage calls.

## Deliberate limits

Publish JSON has a **64 MiB encoded-body cap**, so base64 overhead reduces the
maximum tarball size below 48 MiB. This is an npm protocol adapter cap, not the
shared artifact/object size limit. At most two JSON publications are admitted
concurrently. JSON and decoded attachments are held in bounded memory, not
advertised as constant-memory streaming. Metadata is at most 2 MiB/version;
packuments are limited to 10,000 versions and 16 MiB aggregate stored manifests,
with a 15-second generation context. Network and archive reads use at most
64 KiB per call; transfer deadlines refresh to 30 seconds to replace short
control-plane HTTP deadlines. Archive expansion is limited to 256 MiB, 1000:1
compressed size and 10,000 entries, with cancellation checks per 64 KiB read.

Mutable dist-tag PUT/DELETE and withdrawals/unpublish return **501**, pending
shared audited CAS lifecycle support. Initial publish tags are immutable: a
second version using `latest` conflicts; publish under a distinct tag until
mutable tags are implemented. This is intentionally not full F14 conformance.
Remote pull-through, replicas, search, audit advisories, provenance attachments,
anonymous access, range requests, npm login and organization APIs are not
implemented by this package. No upstream fallback occurs.

## Tests and evidence

Run unit tests with `go test ./internal/npmregistry`. PostgreSQL fixtures use an
isolated schema in `ANT_TEST_DATABASE_URL`; set `ANT_REQUIRE_INTEGRATION=1` to
fail instead of skipping when it is absent. Native tests use installed `npm`,
a temporary npmrc/cache, local HTTP loopback and no external package sources.

On 2026-10-18, local PostgreSQL/disk tests ran with:

```
ANT_TEST_DATABASE_URL='...' ANT_REQUIRE_INTEGRATION=1 \
  go test -race -count=1 ./internal/npmregistry
```

The installed **npm 10.9.8** fixture executes scoped `publish`, `view`, `dist-tag
ls`, `install`, and lockfile `ci --offline`, then checks installed bytes. Tests
also cover malformed names/versions/digests/archives, cancellation, browser auth
rejection, scoped denial, foreign-instance rejection, trusted origin validation,
immutable tag conflict atomicity, concurrent immutable publication conflicts,
owner isolation/no fallback, and a one-connection database pool. The contract's
npm **12.2.1** target, MinIO, proxy/cache/replica behavior and mutable tag moves
have not been certified by these tests. No module dependencies were added.

Integration owner must mount the handler, configure the trusted origin, and
provide the existing artifact/policy/routing/storage/lease services. No main,
OpenAPI, migration or module files are changed by this package.

References: [F14 contract](../../docs/planning/contracts/F14.md),
[shared contract](../../docs/planning/contracts/common.md),
[official npm registry API](https://github.com/npm/registry/blob/main/docs/REGISTRY-API.md),
[official npm publish implementation](https://github.com/npm/cli/blob/latest/workspaces/libnpmpublish/lib/publish.js).

Remote packuments are rewritten as a stream (one version in memory at a time), so
the 16 MiB limit applies only to locally stored packuments. A remote packument
above the metadata cache bound is streamed uncached from upstream on each request.
