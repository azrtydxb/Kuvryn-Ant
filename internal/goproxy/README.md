# Go module adapter

`New(db *pgxpool.Pool, service *artifact.Service) (*Proxy, error)` returns an
`http.Handler`. Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/go/`; the feed format is `go`.
Local modules, pull-through of a remote GOPROXY and the checksum database are described
below and in `site/formats/go.md`.

GET/HEAD supports `{escaped-module}/@v/list`, `@v/{escaped-version}.info`,
`.mod`, `.zip`, and `{escaped-module}/@latest`. Escaping uses `x/mod/module`.
Lists exclude pseudo-versions. Latest prefers releases, then prereleases, then
pseudo-version timestamps. Retractions are preserved in go.mod for the client.

Administrative ingestion uses PUT
`/feeds/{slug}/go/{escaped-module}/@v/{escaped-version}` with multipart/form-data
file parts named `info`, `mod`, `zip` (exactly one each, any order). It requires
the shared scoped publish permission. Complete validation precedes one shared
`PublishBundle` call; all three records become visible atomically. Different
immutable bytes return 409; identical retries return 200 (new bundles: 201).
Every asset is prepared before accepting bytes and pins the same local source.

`.mod` must declare the requested module; a zip's root go.mod, when present,
must match it byte-for-byte. `x/mod/zip.CheckZip` checks canonical paths, versions,
collisions and Go's protocol size bounds; every file is additionally drained to
validate decompression/CRC. The zip is staged to a private temporary file,
never extracted. Limits are Go's 500 MiB compressed/uncompressed zip and 16 MiB
go.mod limits, 1 MiB info metadata, ingestions bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32), 30-second
transfer idle deadlines and 64 KiB streaming buffers. Version indexes fail closed
above 10,000 entries rather than return truncated lists.

Basic username/token and Bearer credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`; the `go` command sends nothing over plain HTTP, so use HTTPS). Configure clients with HTTPS,
GOPROXY pointing at the feed endpoint, and GONOSUMDB/GOPRIVATE for the private
module namespace. When GOPRIVATE is set, set GONOPROXY=none to continue using
this proxy. Public checksum verification remains enabled. This is not a sumdb.

Ownership: reads choose the first package owner, never
merge versions or fall back to another source. Latest checks owner identity
again before opening bytes; regular downloads check record consistency between
Lookup and Open. No metadata writes bypass the shared artifact service.

Tests use ANT_TEST_DATABASE_URL and isolated PostgreSQL schemas. They cover
invalid paths/zips, CRC corruption, wrong module identities, immutable retries
and conflicts, scoped denial, incomplete ingestion, failed second-asset
finalization with no visible records, and package-owner isolation. The real
`go mod download` test uses an isolated module cache and a loopback bridge that
adds a token (Go does not send credentials over HTTP); HTTP token behavior is
separately tested directly. The bridge does not bypass adapter authorization.

Protocol references: https://go.dev/ref/mod#goproxy-protocol and
https://pkg.go.dev/golang.org/x/mod/zip .

The real `go` command (module download and pull-through of proxy.golang.org) is
exercised by `conformance/run.sh local go`.

Checksum database: `/sumdb/sum.golang.org/*` is first requested from the remote
source under `/sumdb/` (for upstreams that mirror it) and otherwise fetched from
`https://sum.golang.org` itself (signed; clients verify it). Read requests for
anything that is not a module path or version answer 404 so the go command can
probe path prefixes and branch names.

Remote source configuration: proxy.golang.org answers some `.zip` downloads
(large modules such as `github.com/aws/aws-sdk-go`) with a 302 to
`https://storage.googleapis.com/proxy-golang-org-prod/...`. Add
`https://storage.googleapis.com` to the source's `redirect_origins`; without it
those downloads fail with the `policy` error class (redirect off origin) while
smaller modules keep working.
