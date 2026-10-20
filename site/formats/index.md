# Formats overview

Kuvryn Ant serves 26 package ecosystems from one artifact core. Every format is a
thin protocol adapter: it translates the client's protocol onto the same
publication, read-resolution, policy, delete and pull-through machinery, so feeds,
sources, publish policies, retention, audit and replication behave the same way for
every format.

This page summarises what each adapter supports and explains the parts that are
common to all of them: feeds per format, pull-through semantics and the remote
source configuration. The per-format pages hold client configuration and the exact
limits.

## How a format is exposed

A **feed** is the stable client endpoint for exactly one format. Its URL is
derived from the feed slug and the format:

| Format family                | Client endpoint                                                                                                    |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------ |
| OCI (`oci`, `oci-artifacts`) | `https://ant.example.com/v2/<feed>/<repository>/...`, image references `ant.example.com/<feed>/<repository>:<tag>` |
| Everything else              | `https://ant.example.com/feeds/<slug>/<format-path>/...`                                                           |

The format path is the feed format, for example `/feeds/npm/npm/` for an `npm` feed
with slug `npm`, or `/feeds/charts/helm/` for a `helm` feed with slug `charts`. A
feed is backed by one or more **sources** of the same format:

- `local`: content published to Ant. Every publish resolves to exactly one local
  source through the feed's [publish policy](../concepts/publish-policies.md).
- `remote`: an upstream registry that Ant reads through and caches
  (pull-through). Publishes never go upstream.
- `replica`: a read-only source filled by [replication](../replication/index.md)
  from another Ant instance.

The feed's read order decides which source owns a package; see
[Feeds and sources](../concepts/feeds-and-sources.md) and
[Read order and resolve explain](../concepts/resolution.md).

## Support matrix

Status is derived from the adapter code, each `internal/<format>/README.md`, and the
real-client conformance run of 2026-10-19 (pinned client containers against a local
instance; harness described in
[`conformance/README.md`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/conformance/README.md)).

Legend for **Publish**: _native_ means the ecosystem's own publish command works;
_adapter API_ means the format has no native publish command (or the native one is
not supported) and content is uploaded with an HTTP call to an adapter route, which
needs the matching role. Legend for **Remote**: _verified_ means a real client pulled
through a public upstream and then re-read the cached copy with the upstream
unreachable; _implemented_ means the code path exists but the real-client run did
not pass or did not cover it.

| Format                                        | Feed `format`                 | Endpoint (after the host)                | Publish                                                                           | Remote (pull-through)                  | Replica |
| --------------------------------------------- | ----------------------------- | ---------------------------------------- | --------------------------------------------------------------------------------- | -------------------------------------- | ------- |
| [OCI / Docker](./oci.md)                      | `oci`                         | `/v2/<feed>/<repo>`                      | Native (`docker push`, `crane`, buildx)                                           | Verified (Docker Hub, `mirror.gcr.io`) | Yes     |
| [OCI artifacts](./oci.md#oci-artifacts-feeds) | `oci-artifacts`               | `/v2/<feed>/<repo>`                      | Native (`oras push`, `oras attach`)                                               | Verified (`ghcr.io`)                   | Yes     |
| [Helm (classic and OCI)](./helm.md)           | `helm` (OCI charts use `oci`) | `/feeds/<slug>/helm/`                    | Adapter API (`POST charts/<name>/<version>`); `helm push oci://` on an `oci` feed | Verified                               | Yes     |
| [npm / pnpm / Yarn](./npm.md)                 | `npm`                         | `/feeds/<slug>/npm/`                     | Native (`npm publish`, `pnpm publish`), with tag limits                           | Verified (`registry.npmjs.org`)        | Yes     |
| [PyPI](./pypi.md)                             | `pypi`                        | `/feeds/<slug>/pypi/simple/`             | Native (`twine upload`)                                                           | Verified (`pypi.org`)                  | Yes     |
| [Maven / Gradle](./maven.md)                  | `maven`                       | `/feeds/<slug>/maven/`                   | Native (`mvn deploy`) and adapter bundle API                                      | Verified (Maven Central)               | Yes     |
| [Go modules](./go.md)                         | `go`                          | `/feeds/<slug>/go`                       | Adapter API (Go has no publish command)                                           | Verified (`proxy.golang.org`)          | Yes     |
| [Cargo](./cargo.md)                           | `cargo`                       | `sparse+https://.../feeds/<slug>/cargo/` | Native (`cargo publish`)                                                          | Verified (crates.io)                   | Yes     |
| [NuGet](./nuget.md)                           | `nuget`                       | `/feeds/<slug>/nuget/v3/index.json`      | Native (`dotnet nuget push`), see page                                            | Verified (`api.nuget.org`)             | Yes     |
| [RubyGems](./rubygems.md)                     | `rubygems`                    | `/feeds/<slug>/rubygems/`                | Native (`gem push`)                                                               | Verified (rubygems.org)                | Yes     |
| [Composer](./composer.md)                     | `composer`                    | `/feeds/<slug>/composer/`                | Adapter API (Composer has no publish command)                                     | Verified (Packagist)                   | Yes     |
| [Conda](./conda.md)                           | `conda`                       | `/feeds/<slug>/conda/`                   | Adapter API                                                                       | Verified (small channels only)         | Yes     |
| [Conan](./conan.md)                           | `conan`                       | `/feeds/<slug>/conan/`                   | Native (`conan upload`)                                                           | Verified (ConanCenter)                 | Yes     |
| [CRAN](./cran.md)                             | `cran`                        | `/feeds/<slug>/cran/`                    | Adapter API                                                                       | Verified (CRAN cloud mirror)           | Yes     |
| [pub (Dart / Flutter)](./pub.md)              | `pub`                         | `/feeds/<slug>/pub/`                     | Native (`dart pub publish`)                                                       | Verified (pub.dev)                     | Yes     |
| [CocoaPods](./cocoapods.md)                   | `cocoapods`                   | `/feeds/<slug>/cocoapods/`               | Adapter API                                                                       | Implemented                            | Yes     |
| [Swift packages](./swift.md)                  | `swift`                       | `/feeds/<slug>/swift/`                   | Adapter API (native `swift package-registry publish` not covered)                 | Implemented                            | Yes     |
| [Terraform / OpenTofu](./terraform.md)        | `terraform`                   | `/feeds/<slug>/terraform/`               | Adapter API                                                                       | Implemented                            | Yes     |
| [Ansible Galaxy](./ansible.md)                | `ansible`                     | `/feeds/<slug>/ansible/`                 | Adapter API (native `ansible-galaxy collection publish` did not pass)             | Implemented                            | Yes     |
| [APT](./apt.md)                               | `apt`                         | `/feeds/<slug>/apt/`                     | Adapter API                                                                       | Verified (`deb.debian.org`)            | Yes     |
| [RPM](./rpm.md)                               | `rpm`                         | `/feeds/<slug>/rpm/`                     | Adapter API                                                                       | Verified (Rocky Linux BaseOS)          | Yes     |
| [APK](./apk.md)                               | `apk`                         | `/feeds/<slug>/apk/`                     | Adapter API                                                                       | Implemented                            | Yes     |
| [Git LFS](./git-lfs.md)                       | `git-lfs`                     | `/feeds/<slug>/git-lfs/`                 | Native (`git lfs push`)                                                           | Implemented                            | Yes     |
| [P2 (Eclipse)](./p2.md)                       | `p2`                          | `/feeds/<slug>/p2/`                      | Adapter API (`repository.zip`)                                                    | Implemented                            | Yes     |
| [Hugging Face](./huggingface.md)              | `huggingface`                 | `/feeds/<slug>/huggingface/`             | Adapter API (the hub commit API is not implemented)                               | Verified (huggingface.co)              | Yes     |
| [Raw / WebDAV](./raw.md)                      | `raw`                         | `/feeds/<slug>/raw/`                     | Native (HTTP `PUT`, WebDAV for sccache)                                           | Verified                               | Yes     |

Consumption with the ecosystem's real client works for every format above, with the
exceptions recorded in the gap block below.

::: warning Gap

Items the 2026-10-19 conformance run did not pass or did not cover. Check the
per-format page before relying on them.

- **Remote, implemented but not verified with a real client:** CocoaPods and
  Ansible Galaxy (the pull-through run failed), Terraform (provider install through
  the feed failed, locally published and remote), Swift, APK, Git LFS and P2 (the
  harness recorded them as having no remote support; the pull-through code paths
  were added to the adapters, but no real-client run covers them).
- **Consume:** the Hugging Face `hf download` / `snapshot_download` run failed in
  that run, `terraform init` could not install a provider from the feed (modules
  worked); the Swift `swift package resolve` and the Eclipse p2 director / Tycho
  runs were not executed.
- **Publish:** `dotnet nuget push` of unmodified `dotnet pack` output failed (a
  nuspec with a byte-order mark is rejected); a `.deb` built with the zstd default
  compression of current dpkg is rejected; a PyPI wheel without a long description
  is rejected; the native `ansible-galaxy collection publish` failed (the direct
  multipart API works).
- **Replica:** replication is format-agnostic (see below), but no format has a
  recorded end-to-end replica test with a real client.

:::

### Replica support

Replication works at the artifact-core level: the authority exports assets, tags,
artifact subjects and component metadata of a feed, and the destination applies them
into a `replica` source, whatever the format. The only format rule is that the source
and destination feed must have the same format (a mismatch is a conflict). Feeds with
`purpose: cache` (build caches, `oci` and `raw` only) are never replicated. See
[Replication and multi-site](../replication/index.md).

### OCI artifacts feeds

`oci-artifacts` is a separate feed format for non-image OCI content (SBOMs,
signatures, attestations, arbitrary files pushed with ORAS). It is served by the same
registry adapter on the same `/v2/` route and accepts the same requests as an `oci`
feed, including the referrers API. Use it to keep images and other OCI artifacts in
separate feeds with separate policies. Details on the [OCI page](./oci.md#oci-artifacts-feeds).

## Pull-through semantics

Pull-through is implemented once, in the artifact core, and every adapter hooks its
read path into it. It is active only when the server has its secret keyring (the root
key); without it, remote sources cannot authenticate and feeds serve local content
only.

- **Package owner first.** Before any upstream request, Ant looks up which source of
  the feed owns the coordinate (package, chart, image repository, module). If a local
  source owns it, the answer comes from that source only and no upstream request is
  made: a private package name can never be completed from a public upstream. If a
  remote source owns it (it was cached from there), only that source is asked. With no
  owner, the remote sources are tried in the feed's read order.
- **Not found falls through, errors stop.** An upstream `404` or `410` moves on to the
  next remote source. Any other upstream failure (timeout, `5xx`, TLS error, policy
  refusal) stops the search and the client gets `503`, so an outage of a private
  upstream cannot silently resolve a name from a lower-priority public one.
- **Immutable artifacts are cached.** Tarballs, wheels, blobs, manifests by digest and
  similar files are stored under the remote source on first download, as ordinary
  versions with remote provenance (audit action `artifact.materialize`). The next read
  is a local hit and keeps working while the upstream is down.
- **Misses stream.** On a miss the client receives bytes while they are staged; the
  cache entry commits only on a clean end of stream with the declared length. Where
  the upstream metadata states a digest (OCI, PyPI files, and the formats listed on
  their pages) the object is verified before it is served or cached.
- **Concurrent misses are coalesced** into one upstream request.
- **Mutable documents are not stored.** Indexes, packuments, `maven-metadata.xml`,
  version lists and OCI tags go through a bounded in-memory metadata cache per source
  (TTL, negative cache, stale-if-error) and are rewritten so every URL points back at
  the feed.
- **Only `GET` goes upstream**, except the Git LFS batch query (a bounded same-origin
  JSON `POST`, `download` operation only).

### Cache headers

| Header                                                              | Meaning                                                                                                                                                                                                                                                                               |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `X-Kuvryn-Cache: miss`                                              | The artifact was fetched from the upstream for this request (streamed miss).                                                                                                                                                                                                          |
| `X-Kuvryn-Cache: stale` (with `Warning: 110 - "Response is Stale"`) | A metadata document was served from the metadata cache after its TTL because the upstream failed, within `stale_if_error_seconds`.                                                                                                                                                    |
| `X-Kuvryn-Cache: offline`                                           | The upstream was unreachable and the adapter served its locally composed view (local content plus what is already cached). Set by the adapters that compose an index locally, for example Helm, Maven, Go, Cargo, Composer, Conda, CRAN, pub, RubyGems, Conan, Ansible and CocoaPods. |

Metadata and negative caches live in process memory: they are not shared between
replicas and are lost on restart. Cached artifacts are durable.

## Remote source configuration

A remote source carries one upstream configuration
(`PUT /api/v1/sources/{id}/remote-config`, the "Upstream" tab of the source in the
console):

| Field                          | Constraints                                                                                        | Meaning                                                                                                                                                                                                              |
| ------------------------------ | -------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `origin`                       | `https://` only, no credentials, query, fragment or encoded path segments; may include a base path | Upstream root for the format. Requests are resolved relative to it, for example `https://registry.npmjs.org` or `https://deb.debian.org/debian`.                                                                     |
| `redirect_origins`             | Up to 16 `https://` origins (scheme, host, optional port; no path)                                 | Hosts the upstream may redirect to or reference by absolute URL (download CDNs, token realms). They never receive the source credential. A redirect to a host that is neither the origin nor listed here is refused. |
| `credential_id`                | UUID, optional                                                                                     | Set by `PUT /api/v1/sources/{id}/remote-credential`; omit to detach.                                                                                                                                                 |
| `ca_ref`                       | Operator CA identifier, optional                                                                   | A key from the CA map in `ANT_UPSTREAM_CA_MAP_FILE`, for upstreams with a private CA. Never a path.                                                                                                                  |
| `allow_private`                | boolean (required)                                                                                 | Allow the origin to resolve to private or loopback addresses (an internal registry). Set `false` for public upstreams.                                                                                               |
| `timeout_seconds`              | 1 to 300                                                                                           | Upstream request timeout.                                                                                                                                                                                            |
| `cache.metadata_ttl_seconds`   | 1 to 86400                                                                                         | How long a metadata document is fresh.                                                                                                                                                                               |
| `cache.negative_ttl_seconds`   | 1 to 300                                                                                           | How long an upstream `404` is remembered.                                                                                                                                                                            |
| `cache.stale_if_error_seconds` | 0 to 86400                                                                                         | How long an expired document may still be served when the upstream fails (`0` disables stale serving).                                                                                                               |
| `cache.max_entries`            | 1 to 100000                                                                                        | Metadata cache entries for this source.                                                                                                                                                                              |
| `cache.max_bytes`              | 1024 to 1073741824                                                                                 | Metadata cache size for this source.                                                                                                                                                                                 |

The credential (`PUT /api/v1/sources/{id}/remote-credential`) is
`{"authorization": "<header value>"}` (sent as `Authorization`) and/or
`{"api_key": "<value>"}` (sent as `X-Api-Key`). It is stored encrypted per source,
never returned by the API, and only sent to the source origin (and, for OCI, to a
token realm that is the origin or a listed redirect origin). A remote origin that
points back at the same Ant instance is rejected.

Other operations on a remote source:

- `POST /api/v1/sources/{id}:test` performs one bounded `GET` of the origin with the
  configured credential, CA and egress policy, and records the result in the source
  health.
- `GET /api/v1/sources/{id}/health` shows upstream health.
- `DELETE /api/v1/sources/{id}/cache` purges the content cached from that source
  (blobs are reclaimed later by garbage collection).
- `GET /api/v1/feeds/{id}/resolve?coordinate=<name>` explains which source owns a
  coordinate and the cache freshness.

### Limits common to all formats

- Remote metadata documents are bounded in size by the server (24 MiB per document
  in the shipped build). Very large upstream indexes (for example full conda-forge
  `repodata.json`) cannot be proxied. npm is the exception: an oversized packument is
  streamed from upstream uncached.
- Upstream authentication is a static header per source. The only exchange
  protocol is the OCI bearer-token handshake (see the [OCI page](./oci.md#remote-upstreams)).
- Upstream latency and hit-rate metrics per source are not exposed yet.

## Client authentication

All feed routes share one client-authentication layer. Clients send a service token
(`ant_...`) or a personal access token (`antp_...`) as a Bearer token or as the
password of HTTP Basic authentication; directory (LDAP) users can use Basic with
their password where the feed policy allows it, and GitHub Actions can use its OIDC
token directly or exchange it for a short-lived Ant token. Publishing is never
anonymous; anonymous reads need `anonymous_read` on the feed and a policy that allows
them. See [Package client auth](../identity/client-auth.md) and
[GitHub Actions](../ci/github-actions.md).

Some adapters accept only one credential form for a given token type; the format
pages say which client setting to use.
