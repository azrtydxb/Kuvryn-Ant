# Pull-through reads (remote sources)

Pull-through is implemented once, in `internal/artifact` (`remote.go`), on top of
`internal/remoteruntime`. Adapters add a small hook to their existing read path.
Enabled today: all 26 formats: raw, npm, pypi, maven, go, rubygems, conda, conan, composer, terraform, oci (blobs and manifests by digest, tags,
tag lists), helm, cargo, nuget, apt, rpm, pub, cocoapods, cran, ansible, huggingface, apk, git-lfs, p2, swift (see "Format notes"). `cmd/ant/artifacts.go` builds the `remoteruntime.Manager` from the
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
  upstream, with one narrow exception: the Git LFS batch query (a bounded, same-origin JSON POST, operation `download` only, see
  below); `Publish`/`Prepare` accept only local sources.

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

## Format notes (pub, cocoapods, cran, ansible, huggingface)

- **Pub** (`internal/pubrepo`). Origin is the pub.dev API host (`api/packages/<name>`). The
  package document is validated (name, semver, pubspec identity, `archive_sha256` hex) and
  re-rendered with every `archive_url` pointing at the feed (`packages/<name>/versions/<v>.tar.gz`);
  unknown fields (`isDiscontinued`, `published`, `retracted`, ...) are kept and `latest` follows
  upstream. Archives come from the upstream `archive_url` (pub.dev redirects to a storage host:
  add it as a redirect origin), are verified against `archive_sha256` before the first byte is
  served and cached; the pubspec is cached beside it so cached versions stay listable offline.
  Documents above the source's metadata bound (24 MiB by default) cannot be proxied.
- **CocoaPods** (`internal/cocoapods`). Origin is the CDN root (`cdn.cocoapods.org`).
  `all_pods_versions_<a>_<b>_<c>.txt` shards come through the metadata cache and are merged by pod
  name (a pod owned by a local source keeps only its local versions; cached remote pods are
  replaced by the upstream list while it is reachable). `Specs/.../<Pod>.podspec.json` is served
  verbatim and cached on first read. Only specs are proxied: a podspec's `source` (VCS or http)
  is not rewritten, so clients fetch pod sources from their original hosts, and `archives/...`
  serves locally published archives only. `CocoaPods-version.yml` and
  `deprecated_podspecs.txt` stay local.
- **CRAN** (`internal/cran`). Origin is the mirror root (`cloud.r-project.org`). Upstream
  `src/contrib/PACKAGES.gz` is read (compressed, so it stays well below the metadata bound), validated and merged
  into the feed `PACKAGES`/`PACKAGES.gz` by package name: names present in a local source never take
  upstream metadata or archives (local ownership is checked per package name across all versions).
  Source tarballs (`src/contrib/<pkg>_<ver>.tar.gz`, `src/contrib/Archive/<pkg>/...`) share one cache
  entry per version; `bin/<platform>/contrib/<rver>/PACKAGES[.gz]` is passed through and binaries are
  cached on first download. CRAN publishes MD5 only, so fills are length checked, not hash verified.
  `PACKAGES.rds`, `Meta/archive.rds` and `Archive/<pkg>/` listings are not served (R falls back to
  `PACKAGES.gz`).
- **Ansible Galaxy** (`internal/ansiblerepo`). Origin is the API root serving `v3/` (for example
  `https://galaxy.ansible.com/api/`). Collection detail, the version list (all pages are fetched; only
  absolute `links.next`, or relative ones when an absolute `href` reveals the origin, can be
  followed, at most 50 pages) and version detail are re-rendered with feed `href`/`download_url`
  values. Archives come from the upstream `download_url`, are verified against `artifact.sha256` and
  cached with a `metadata` asset for offline listing. A collection owned by a local source is never
  answered from upstream. The rendered list keeps the upstream `data`/`meta`/`links` shape (single page).
- **Hugging Face** (`internal/huggingface`). Origin is the hub endpoint; the CDN hosts the hub redirects
  to (cdn-lfs, xethub) must be redirect origins. The source credential (the way to reach gated or
  private repos) is sent to hub requests only, never to a CDN host, and the client's own token is never
  forwarded. Files are cached per upstream commit under `models|datasets/<repo>`; a branch or tag is
  resolved through the metadata cache and remembered as a ref so cached files stay reachable by
  branch name in an outage. sha256 and size come from the upstream tree entry (LFS oid): a miss streams
  to the client while staging and the digest is checked at EOF (a mismatch aborts the connection and caches
  nothing; `Origin.StreamVerified`), so multi-GB files are not held back. HEAD answers from the tree entry
  without downloading. A Range request on a miss triggers a verified fill that survives the client giving
  up, then is answered from the store (resume hits the cache). Non-LFS files carry only a blob sha1
  upstream and are length checked only. Upstream 401/403 (gated, no access) surfaces as 503. Listing
  limits: tree pages beyond the first are not followed and the recursive tree is a bounded directory walk
  (64 directories, 10,000 entries); revision names containing `/` are not supported.

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
For CI, make the feed a cache feed (`purpose: cache`): its entries are then evicted by idle time
and size and are never replicated; see `docs/operations/build-caches.md`.

## Format notes (apk, git-lfs, p2, swift)

The last four formats; per-adapter details are in each `internal/<format>/README.md`.

- **APK** (`internal/apkrepo`). Origin is the repository root (`https://dl-cdn.alpinelinux.org/alpine`). `APKINDEX.tar.gz` is served verbatim
  from upstream, so `apk` verifies the upstream signature; a package is served only if the upstream index lists it and its control
  segment matches the index checksum (`C:Q1` SHA-1) and size, checked before the first byte.
- **Git LFS** (`internal/gitlfs`). Origin is the upstream LFS endpoint (`lfs.url`). The upstream batch API (`artifact.Service.RemoteQuery`,
  `upstream.Fetcher.Post`: JSON POST, same origin, bounded, `download` only) decides whether an object exists; the bytes come from the
  action href on first download and are verified against the oid. Limits: basic transfer only, upstream action headers are not
  forwarded, hrefs on other hosts need a redirect origin.
- **p2** (`internal/p2repo`). Origin is the repository root. Indexes verbatim; bundles/features/binaries only if the artifacts index lists
  them, verified against `download.checksum.sha-256` (or md5 + size). No composite repositories or Pack200.
- **Swift** (`internal/swiftrepo`). Origin is the registry root. Release list and release resource are validated and re-rendered; the
  archive is verified against the release resource's SHA-256, upstream signing data is preserved. Only the default `Package.swift` is
  proxied.

## Limitations

- The only token exchange is the OCI/Docker bearer-token handshake (Docker Hub style
  `WWW-Authenticate: Bearer realm=...` challenges, see `oci-mirror.md`); every other
  format sends only the source's static `authorization`/`api_key` header.
- npm tarballs are fetched from `<name>/-/<base>-<version>.tgz`, not from the URL in the
  upstream document; PyPI serves only wheels and `.tar.gz` sdists with a stated sha256, and
  the all-projects `/simple/` index is local only; Maven timestamped snapshots remain unsupported.
- Remote metadata documents (Helm `index.yaml`, NuGet registration, APT/RPM Release/repomd) are held in the in-memory metadata cache and limited to 24 MiB by default; the limit can be changed per source with `cache.max_metadata_bytes` (never above the source's cache quota). A larger document is streamed uncached by the adapters that can (Conda repodata, npm packuments, p2 manifests) and refused with 503 by the others, so very large Helm indexes (for example bitnami) cannot be proxied; large APT/RPM index files are not subject to this limit because they are cached as verified artifacts.
- Signed upstream indexes (APT, RPM) are never rewritten or merged; mixed local/remote feeds answer a distribution or repository from exactly one owner.
- Metadata and negative caches are in-process (not shared across instances, lost on restart);
  cached artifacts are durable. Metadata bodies are limited as described above.
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
  verified `.tar.bz2` entry is cached beside it for offline listing. `current_repodata.json` is served;
  the `.zst` and `.bz2` variants and sharded repodata are not (clients fall back to `repodata.json`).
  A repodata above the source's metadata bound (conda-forge `linux-64`/`noarch`) is streamed instead:
  `repodata.json.zst` is preferred upstream, entries are validated one at a time and written to the
  client, and a spooled digest index verifies later package downloads, so memory does not grow with the
  document (see `internal/condarepo/README.md`).
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

## Observed tags and index grouping (package discovery)

When a client resolves an OCI tag through a remote source, the adapter records
the answer with `artifact.ObserveTag`: a `tags` row with `observed_at` set, on
the cached component, pointing at the cached manifest digest. Re-resolving a
moved tag updates the row (revision bump, audit `artifact.observe_tag`, outbox
topic of the same name); an unchanged tag is only refreshed every 10 minutes. An
observed tag is discovery metadata only: `ResolveTag` ignores it (the remote seam
stays the source of truth for the mutable pointer), and a tag without
`observed_at` (published or replicated locally) is never overwritten.

Image-index children are recorded in `version_children` (parent version, child
digest, platform) at publish and at cache time, and are excluded from the package
`version_count`, `latest_version` and the `top_level=true` version listing. They
appear in the versions listing with `parent_version` and `platform`.

Docker Hub feeds (any remote source whose origin is `registry-1.docker.io`,
`docker.io`, `index.docker.io` or `registry.hub.docker.com`) canonicalise
single-segment read names (`alpine`) to `library/alpine`, unless a component with
the short name already exists. Writes are never rewritten.

Backfill for data cached or published before this: tags appear on the next
resolution of each tag; index links are also recorded then. For indexes that are
not re-resolved, run `ant repair oci-index-children [--dry-run]` (idempotent,
reads manifest bytes from blob storage, never contacts an upstream).

## OCI blobs are not versions

An OCI blob (image config or layer) is stored, like every artifact, as an asset
(`blob`) of a digest-named version row of its repository's component. That row
is the storage carrier that reference checks, access accounting
(`last_accessed_at`, downloads), retention/pressure eviction and GC operate on.
It is not a version a person pulls, so `versions.blob_only` (migration 047) marks
it. The flag is set by a database trigger when a `blob` asset is inserted into a
version of an `oci`/`oci-artifacts` source (publish, pull-through, replication
and import alike) and cleared if a manifest asset is later stored under the same
digest. `version_count`, `latest_version`, `updated_at`, the package versions
listing and the feed/source version statistics skip blob-only rows. Retention,
pressure eviction and GC still see them, so blob-only cached data stays
evictable by idle time and pressure.

A top-level manifest that no tag names and no index groups is returned by the
versions listing with `pulled_by_digest: true` (the console shows a "pulled by
digest" badge). Platform is shown for manifests grouped under an index; a
standalone manifest's platform lives in its config blob, which the management
plane does not read.

Rows created before migration 047 are classified by
`ant repair oci-blob-versions [--dry-run] [--feed SLUG]`. It examines digest-named
OCI versions that carry only `blob` assets, keeps any whose media type is a
manifest type or whose bytes parse as a manifest/index, and marks the rest
blob-only. No asset, blob row or byte changes, so timestamps and references are
untouched. It is idempotent, batched, reports counts per feed and repository, and
writes one `repair.oci_blob_versions` audit event.

Retention ranks only visible versions: blob-only rows do not count toward
`keep_last` or `min_keep` and are never deleted by `keep_last` directly. When a
policy deletes OCI manifests, blob-only rows named only by those manifests (read
from the stored manifest bytes) are released in the same transaction and their
bytes fall to normal GC; rows still named by a remaining manifest stay, and if a
remaining manifest cannot be read nothing is released. `max_age`, `max_idle` and
pressure eviction treat blob-only rows as before.

Index children follow their parent. `keep_last` and `min_keep` (retention and
pressure) rank top-level versions only: visible versions that are not named in
`version_children`. A child is never selected on its own by `keep_last`,
`max_age`, `max_idle` or pressure eviction; a top-level version's last-used time
includes its children, so an image is idle only when every platform manifest
is. When an index is deleted, each child whose every parent is also deleted is
deleted with it (a child another index names, or a tagged one unless the policy
includes tagged versions, stays), and blob-only rows named only by the deleted
manifests are released as above.

Blob-only rows are never selected on their own timestamps while a manifest still
names them. Every path that can delete versions (retention `max_age`/`max_idle`,
cache-feed idle and `max_bytes`, pressure eviction) drops a blob-only row that
any remaining visible manifest, top-level or child, names, and drops all of them
when a remaining manifest cannot be read. A blob-only row no manifest names (an
orphan upload or stray pull) can still be evicted by idle or age. Pressure
eviction does not pick blob-only rows at all; they are released with the
manifests that name them, and its byte projection counts the children and
released blobs of each picked image.
