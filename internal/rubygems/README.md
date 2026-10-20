# RubyGems adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/rubygems/` for feeds of format `rubygems`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/rubygems.md`.
The repository binds to `service.Lease().InstanceID`. PostgreSQL is the only metadata
backend.

## Implemented routes

- `POST api/v1/gems`: raw `.gem` push; raw Authorization API key, Basic password
  token or Bearer token. Basic and Bearer credentials are verified by the shared
  client-authentication layer; a raw API key (what `gem push` sends) is checked directly
  against service tokens (see `site/formats/rubygems.md`). Returns 201, including identical-byte retries; different
  bytes at the same name/version/platform conflict with 409.
- `GET/HEAD names`, `versions`, `info/{name}`: Bundler compact index, runtime
  dependency constraints, Ruby/RubyGems requirements and gem SHA256; versions
  includes each info document's protocol MD5. ETags, conditional requests and
  byte ranges are supported. Indexes are regenerated snapshots, not append logs.
- `GET/HEAD specs.4.8.gz`, `prerelease_specs.4.8.gz`: legacy Marshal 4.8 tuples.
- `GET/HEAD quick/Marshal.4.8/{name}-{version}[-{platform}].gemspec.rz`: generated
  dependency gemspec. Its fixed, write-only Marshal encoder never decodes an
  uploaded Marshal object. It retains identity, dependencies, requirements,
  summary, description, authors, licenses, homepage and date; absent dates use
  Unix epoch. Other gemspec fields remain in the exact original gem archive.
- `DELETE api/v1/gems/yank`: `gem yank`, removes the version through the artifact core
  (gated by the feed's delete policy).
- `GET/HEAD gems/{name}-{version}[-{platform}].gem`: exact immutable upload bytes,
  ETag/conditional GET and single byte range (multipart ranges return 416).

Native push with a trailing slash in `--host` appends a second slash; this one
mount-boundary slash is accepted. Names and platforms remain case-sensitive.
Platform variants use version keys `version-platform` (plain `version` for ruby).
Ambiguous filename collisions fail closed, rather than choosing another package.
There is no RubyGems.org sign-in/key creation API: provision a scoped service token
through the shared identity service. Basic uses the password as token, ignoring
username. Browser principals are replaced; anonymous reads of a feed with `anonymous_read` are decided by the client-authentication layer.

All responses are private/no-store. These wire representations emit no absolute
URLs and never derive an origin from Host, TLS or forwarded headers. The optional
`SetPublicURL` only accepts HTTPS or literal loopback HTTP origins; it is reserved
for future URL-bearing responses, and is not needed for these relative routes.

## Publication, reads and bounds

Uploads are privately spooled, SHA256 hashed and validated before permanent
storage. `Prepare` authorizes each asset and pins one source with ExpectedSource;
`PublishBundle` atomically commits the gem, generated gemspec and canonical JSON
metadata. Failed validation or bundle commit exposes no partial index entries.
Metadata and bytes use the first component owner, even if that owner has no
requested version. SQL candidate enumeration chooses owners first; every selected
entry is rechecked via shared `Open`/`Lookup`. Rows close before nested service
calls, including with a one-connection pool. Indexes require feed-level read
permission and filter entries through source authorization; source-only scopes
without feed-level read are not sufficient for these index routes.

Limits: concurrent uploads bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32), 256 MiB compressed gem, 1 MiB metadata/checksum
expansion, 512 MiB data expansion, 20,000 data entries, 20,000 YAML nodes, YAML depth
24, 256 dependencies and 16 requirement clauses per dependency. Validation has a
15-second context budget. Index reads have a 20-second budget and fail at more than
4,096 candidate releases or 16 MiB of canonical metadata; no silently truncated
index is returned. Streams use 64 KiB buffers and renewable 30-second progress
deadlines. Temporary files are closed and removed on every return path.

YAML is parsed as inert nodes with fixed Ruby data tags. Aliases, duplicate keys,
merge keys, arbitrary object tags and multiple documents are rejected. No gemspec,
Ruby hooks or extension code executes on the server. TAR paths must be clean and
relative; symlinks/hardlinks, duplicate entries, unknown outer members, corrupt
gzip streams, nonzero trailing payload and expansion excess are rejected. Standard
`metadata.gz` and `data.tar.gz` are required; optional `checksums.yaml.gz` must have
complete, correct SHA256 and (if advertised) SHA512 tables. Signed gem members,
Ruby object-form platform metadata, and noncanonical versions outside the bounded
numeric/dot/alphanumeric grammar are currently rejected.

## Evidence and reproduction

From repository root, with local PostgreSQL binaries on PATH:

```sh
PATH="/opt/homebrew/opt/postgresql@17/bin:$PATH" sh internal/rubygems/test-postgres.sh
```

The script uses the existing project fixture approach: temporary Unix-socket-only
PostgreSQL, per-test schemas and real migrations, disk blobs, and `go test -race`.
It neither changes an existing database nor edits global client configuration.
Native tests discover installed Ruby/gem/Bundler; child HOME, caches, gem paths and
credentials are isolated to the test. Tokens are redacted from client output.

Observed client identity/hashes are in `testdata/observed-client.json`: Ruby 4.0.6,
RubyGems 4.0.16 and Bundler 4.0.16. The tests build and push trusted Ruby fixtures,
including the host platform variant, decode the legacy tuples with RubyGems SafeMarshal, install a dependency-bearing
gem using compact and quick gemspec metadata, then install/check/freeze the
dependency set through Bundler compact indexes. Local build artifacts are removed
before installation. Assertions require the actual legacy and compact routes
to have been requested. HTTP tests cover HEAD/range/conditional behavior, immutable retry and
conflict, concurrent conflicts, malformed input, token denial, cross-instance
isolation, first-owner missing-version behavior, and injected atomic bundle failure.

## Conformance and remote sources

The real RubyGems and Bundler clients (push, install, yank, pull-through of rubygems.org) are exercised by `conformance/run.sh local rubygems`. Not implemented: latest_specs, the dependency API, search, owners, API key creation and signed gem validation.

With a remote source, the per-gem compact index file (`info/<gem>`) is read through the metadata cache and validated line by line; it carries no URLs, so it is served as received. A `.gem` is fetched on first download from `gems/<file>.gem`, verified against the sha256 `checksum` of its info line and cached together with a metadata record rebuilt from that line, so the local composition lists it afterwards and keeps serving it during an outage. `/versions`, `/names` and the legacy specs listings are feed-local views: the upstream versions of a large registry exceed the metadata limit and are never unioned with local content.

Wire references: [RubyGems API](https://guides.rubygems.org/rubygems-org-api/) and
[compact index API](https://guides.rubygems.org/rubygems-org-compact-index-api/).
