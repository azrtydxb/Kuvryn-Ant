# Pull-through reads (remote sources)

Pull-through is implemented once, in `internal/artifact` (`remote.go`), on top of
`internal/remoteruntime`. Adapters add a small hook to their existing read path.
Enabled today: raw, npm, pypi, maven, go, rubygems, conda, conan, composer, terraform, oci (blobs and manifests by digest, tags,
tag lists), helm, cargo, nuget, apt, rpm (see "Format notes"). `cmd/ant/artifacts.go` builds the `remoteruntime.Manager` from the
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

## Format notes (helm, cargo, nuget, apt, rpm)

The remote source origin is the upstream repository root for the format. Absolute
download URLs on another host (Helm chart URLs, crates.io `dl`, a NuGet resource host) need
that host in the source's redirect origins; the source credential is never sent there.

- **Helm** (`internal/helmrepo`). Origin is the chart repository root (`index.yaml` at its
  root). The upstream index is read through the metadata cache, validated (chart name and
  semver version, `urls`, optional sha256 `digest`) and merged into the feed `index.yaml`
  with every `urls` entry rewritten to the pinned feed route
  `charts/<name>/<version>/<source>/<digest>/<file>` (unpinned route when upstream gives
  no digest). Charts whose package owner is a local source are dropped from the upstream
  part. Chart archives are verified against the index digest, cached on first download;
  `.prov` files are cached on demand (the chart is cached first). The first remote source
  with an index serves it; indexes from several remote sources are not unioned.
- **Cargo** (`internal/cargo`). Origin is the sparse index host (`config.json`, `se/rd/serde`
  files). Index files are validated line by line (name, semver, sha256 `cksum`) and served
  verbatim (lines carry no registry URL; the feed's own `config.json` `dl`/`api` already
  point back at the feed). Crate archives come from the upstream `config.json` `dl`
  template (markers `{crate}`, `{version}`, `{prefix}`, `{lowerprefix}`,
  `{sha256-checksum}`), verified against `cksum`; for crates.io add
  `https://static.crates.io` as a redirect origin. The verified index line is cached beside
  the crate so listings survive an outage. Read routes accept crates.io spellings
  (uppercase, underscore); local publication keeps its stricter name rule.
- **NuGet** (`internal/nuget`). Origin is the host serving `v3/index.json` (for example
  `https://api.nuget.org`). `PackageBaseAddress` and `RegistrationsBaseUrl` are discovered
  from it; the feed's own service index advertises the feed flat container/registration
  URLs. Registration documents are re-rendered with `@id`, `packageContent` and
  `registration` rebuilt from the feed base (non-inline pages are fetched and inlined; other
  catalog fields are kept). `.nupkg` and `.nuspec` are cached on first download. NuGet
  publishes no sha256, so fills are length checked, not hash verified.
- **APT** (`internal/aptrepo`). Origin is the repository root (for example
  `https://deb.debian.org/debian`). Trust model: `Release`, `InRelease` and `Release.gpg` are
  served verbatim from upstream, never rewritten, merged or re-signed, so apt verifies the
  upstream archive signature with the upstream key (`signed-by`). Any other `dists/` file
  (Packages*, Contents, Translations, by-hash) is served only if the upstream Release lists it;
  it is fetched by hash when upstream has `Acquire-By-Hash`, verified against the Release
  SHA256, and cached content-addressed. A `.deb` is served only if a Packages index of a
  distribution the feed already served lists its pool path; its SHA256 is verified before
  serving. `Filename` pool paths are relative to the repository root, so they resolve against
  the feed. A distribution published locally is never answered from upstream. The Release
  signature itself is not verified by Ant (no upstream key is configured): integrity of
  upstream bytes rests on TLS to the source plus the client's signature checks. Packages
  indexes are decoded as gzip, xz or bzip2; `apt update` must run through the feed once before
  packages can be pulled (the feed learns distributions from the indexes it served).
- **RPM** (`internal/rpmrepo`). Origin is the repository root (for example
  `https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os`). `repomd.xml`, `repomd.xml.asc` and
  `repomd.xml.key` are served verbatim (same trust model as APT: the client checks the
  upstream signature; package signatures stay inside the untouched RPMs). Other `repodata/` files
  are served only when listed in repomd with a sha256 checksum, verified and cached. Package
  paths of any depth (`Packages/a/acl-...rpm`) are served only if the upstream `primary.xml` lists
  them with a sha256 checksum, verified before serving and cached. Repositories with local
  RPMs keep serving their local snapshot; a package name owned locally is never fetched.
  Not supported: zstd-compressed `primary.xml` (Fedora) and non-sha256 repomd checksums.

Remote cache objects live in their own namespaces (`apt-remote`, `rpm-remote`) or in the
adapter's package namespace, so they never become snapshot owners of a local repository.

## sccache (WebDAV on a raw feed)

`SCCACHE_WEBDAV_ENDPOINT=https://<host>/feeds/<slug>/raw` with `SCCACHE_WEBDAV_TOKEN=<token>`
(or `SCCACHE_WEBDAV_USERNAME`/`PASSWORD` with the token as password) works against a raw feed.
sccache's OpenDAL client needs GET/PUT plus `PROPFIND` and `MKCOL` for parent directories, and
rejects a PROPFIND response without `getlastmodified`. `internal/rawrepo/webdav.go` adds
`OPTIONS`, `PROPFIND` (file or virtual collection, itself only, no child listing) and `MKCOL`
(authorizes publish, creates no state). Verified with the real sccache 0.17 client (cold compile
writes, warm compile hits, zero cache errors; test `TestSccacheWebDAV`, skipped without
sccache). Cache entries are immutable raw files: a PUT with different bytes for an existing key
is a 409, which sccache only logs. The S3 backend is not covered by this adapter.

## Limitations

- Remote registries that require a token handshake (Docker Hub style `WWW-Authenticate`
  realms) are unsupported; only the source's static `authorization`/`api_key` header is sent.
- npm tarballs are fetched from `<name>/-/<base>-<version>.tgz`, not from the URL in the
  upstream document; PyPI serves only wheels and `.tar.gz` sdists with a stated sha256, and
  the all-projects `/simple/` index is local only; Maven timestamped snapshots remain unsupported.
- Remote metadata documents (Helm `index.yaml`, NuGet registration, APT/RPM Release/repomd) are limited to 4 MiB, so very large Helm indexes (for example bitnami) cannot be proxied; large APT/RPM index files are not subject to this limit because they are cached as verified artifacts.
- Signed upstream indexes (APT, RPM) are never rewritten or merged; mixed local/remote feeds answer a distribution or repository from exactly one owner.
- Metadata and negative caches are in-process (not shared across instances, lost on restart);
  cached artifacts are durable. Metadata bodies are limited to 4 MiB.
- Upstream latency/hit-rate metrics and per-source retry budgets beyond the fetcher's
  circuit breaker are not exposed yet.
- A cache fill is not rolled back if the source's configuration changes mid-stream; the
  next read re-resolves ownership.

## Format notes (rubygems, conda, conan, composer, terraform)

Enabled in addition to the formats above. In all five the remote source origin is the
upstream registry root, package ownership is decided before any upstream request (a local
owner is never completed from upstream), only GET is sent upstream, and metadata is served
through the metadata cache (stale-if-error) while immutable artifacts are cached on first
download and listed from the store during an outage.

- **RubyGems** (`internal/rubygems`). Origin is the registry root (`https://rubygems.org`).
  `info/<gem>` (compact index) is validated line by line and served as received: it carries
  no URLs, and each line's `checksum` is the sha256 of the `.gem`. A `.gem` is fetched from
  `gems/<file>.gem` on first download, verified against that checksum and cached with a
  metadata record rebuilt from the info line, so the local composition lists it afterwards.
  Gem names contain dashes, so a `<name>-<version>[-<platform>]` filename is resolved by
  trying each split before a digit against the upstream info file. `quick/Marshal.4.8/*.gemspec.rz`
  is cached for gems already cached. `/versions`, `/names` and `specs.4.8.gz` stay feed-local
  (the upstream `/versions` is far larger than the metadata limit and is never unioned), so a
  pure-remote feed is used with the compact index per gem; Bundler falls back to `info/` files.
- **Conda** (`internal/condarepo`). Origin is the channel root (`https://conda.anaconda.org/conda-forge`).
  `<subdir>/repodata.json` is validated entry by entry (filename must equal name-version-build,
  sha256 required) and re-rendered with `info` reduced to the subdir (a `base_url` would send
  clients around the feed). A subdir is one ownership unit, as for local channels. `.tar.bz2` and
  `.conda` packages are verified against the repodata sha256 before they are served or cached; the
  verified `.tar.bz2` entry is cached beside it for offline listing. `current_repodata.json`, `.zst`
  and `.bz2` variants and sharded repodata are not served. **Limit:** repodata is bounded by the
  runtime metadata limit (at most 8 MiB, default 1 MiB), so very large channels such as
  conda-forge `linux-64`/`noarch` cannot be proxied today; small channels and mirrors work.
- **Conan** (`internal/conan`). Origin is the server root (`https://center2.conan.io`); the Conan v2
  API has no absolute URLs. `latest`, `revisions`, package-id `search` and `files` listings are
  validated and re-rendered. Revision files are immutable and cached on first download. Conan
  publishes md5, not sha256: `conanmanifest.txt` is checked against the revision in the URL (the
  revision is the md5 of the sorted manifest lines) and every other file against the manifest (md5 of
  a file, or of each tarball member). This uses the additive `artifact.Origin.Verify` hook, which
  stages the bytes, runs the check and only then commits and serves. `v2/conans/search` stays local.
- **Composer** (`internal/composer`). Origin is the repository root (`https://repo.packagist.org`).
  The metadata template comes from the upstream `packages.json` (`metadata-url`, default
  `/p2/%package%.json`). `p2/<vendor>/<pkg>.json` and the `~dev` file are expanded from Composer's
  minified form and re-rendered without `source` (a VCS URL would bypass the feed) and with every
  `dist.url` pointing at `dist/<id>/<version>/archive.zip` on the feed; the feed resolves the upstream
  dist URL again from the cached metadata, so clients never choose it. Packagist publishes no archive
  digest (`shasum` is empty): archives are checked to be well-formed ZIPs of bounded size and against a
  SHA-1 `shasum` when upstream states one. Dist URLs live on GitHub: add `https://api.github.com` and
  `https://codeload.github.com` (and `https://gitlab.com`/`https://bitbucket.org` if used) to the
  source's redirect origins; the credential is never sent there. Cached versions keep their entry as
  asset `remote-metadata` (ignored by the local composition) for the offline listing.
- **Terraform** (`internal/terraform`). Origin is the registry root (`https://registry.terraform.io`),
  `v1/modules` and `v1/providers` map one to one. Providers: `versions` and the per-platform `download`
  document are validated; the feed answer keeps `shasum` and `signing_keys` verbatim and points
  `download_url`, `shasums_url` and `shasums_signature_url` at the feed's `files/<name>` routes, so Terraform
  verifies the zip, SHA256SUMS and the signature against the publisher key (HashiCorp's) itself. Ant also
  verifies what it caches: the zip against `shasum`, SHA256SUMS against the zip line, and the signature against
  the supplied key when it parses as a single public key. Add `https://releases.hashicorp.com` to the redirect
  origins. Modules: the registry answers `download` with a 204 and an `X-Terraform-Get` header, which the
  runtime does not expose (it forwards a fixed set of response headers). The feed therefore resolves the module
  detail document (`source` GitHub repository and `tag`) to the GitHub tarball, caches the tar.gz once (checked as
  a bounded, safe tar.gz; no digest is published) and answers `download` with its own archive URL plus the `//*`
  subdir that unwraps the GitHub top-level directory. Registries whose modules are not GitHub repositories, other git
  hosts and zip archives are not supported (forwarding `X-Terraform-Get` from the runtime would lift this). Add
  `https://codeload.github.com` to the redirect origins. Providers have no store-based offline listing (versions and
  download documents rely on the metadata cache); cached files and module versions do.

The seam gained one small helper for these formats: `Origin.Verify`, a format specific check run on the staged
object before it is committed or served (Conan manifests, Composer zips, Terraform provider files and module
archives).
