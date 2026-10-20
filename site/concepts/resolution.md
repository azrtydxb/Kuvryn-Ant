# Read order and resolve explain

When a client reads a package through a feed, Ant decides which attached source answers. The rule is **package-owner first**: one source owns a package and answers for all of its versions. This page describes how the owner is chosen, when a read falls through to the next source, and how to inspect a decision with `GET /api/v1/feeds/{id}/resolve`.

## Read order

The sources attached to a feed are ordered by ascending `priority`; see [feeds and sources](feeds-and-sources.md#attaching-sources-and-read-order). Replication jobs attach their replica source at the lowest precedence, after the sources you configured.

## Package ownership

Every read is for a **coordinate**, the normalized package identity of the format. For example, `@myorg/ui` for npm, `org.example:lib` for Maven, or `library/alpine` for an OCI repository inside a feed.

1. **Find the owner.** Ant takes the first source in read order that already contains the coordinate: published locally, cached from a remote, or replicated. That source owns every version, tag and metadata document of the package. Sources further down that also contain the coordinate are **shadowed** and never consulted for it.
2. **A local owner never goes upstream.** If the owner is a local source, or a source the caller cannot read, a version that is missing there is a plain not-found. No upstream request is made. A private package name can therefore never be completed from a public registry, even when a later remote source has more versions.
3. **A remote owner restricts the search to that remote.** New versions of a package first cached from `npmjs` are fetched from `npmjs` only.
4. **No owner: try remote sources in order.** The remote sources of the feed are asked in read order. Local and replica sources without the coordinate are skipped.
   - An upstream `404` or `410` **falls through** to the next remote source.
   - Any other upstream error **stops the search**, and the client gets `503`. An outage of a private upstream earlier in the list cannot silently resolve the name from a public upstream later in the list.
5. **Namespace mismatch.** If the owner holds the coordinate under a different namespace than the one requested, the read answers not-found and does not fall through.

Mutable remote documents (packuments, `maven-metadata.xml`, version lists, OCI tags) are never concatenated across sources. One owner serves a package. For signed repository formats (APT distributions, RPM repositories), a distribution or repository is answered by exactly one owner, and upstream signed indexes are served verbatim. Each Raw path is its own ownership unit.

::: details Why owner-first
Dependency-confusion attacks work by publishing a public package with the same name as a private one. If a resolver merged versions from all sources, a higher public version could win. With owner-first resolution, once `@myorg/ui` lives in a local source, no remote source can contribute versions of it.
:::

### What is cached

| Remote content                                                           | Stored as                                                                                                                                                                         | Expiry                                                                                                |
| ------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| Immutable artifacts (tarballs, wheels, jars, blobs, manifests by digest) | Ordinary components, versions and assets under the remote source, with `provenance.origin = remote`. They are verified against the upstream digest when the format publishes one. | Never by TTL. Only retention, storage pressure or a purge removes them.                               |
| Mutable metadata (indexes, tags, packuments)                             | The runtime's in-process metadata cache                                                                                                                                           | `metadata_ttl_seconds`. Kept for a further `stale_if_error_seconds` for use while the upstream fails. |
| Upstream not-found                                                       | The negative cache                                                                                                                                                                | `negative_ttl_seconds`                                                                                |

Responses served from expired metadata carry `X-Kuvryn-Cache: stale` and `Warning: 110`. Adapters that can compose a listing from cached artifacts when the upstream is unreachable mark it `X-Kuvryn-Cache: offline`. A streamed first download carries `X-Kuvryn-Cache: miss`. See [fallback behaviour](../host-caching/fallback.md) for the outage cases.

::: warning Gap
Read-policy templates (`kind: read`) accept `local_first`, `fallback_on_not_found` and `failover_on_upstream_error`. The OpenAPI contract describes these settings as "recorded and shown in the effective view; adapters adopt them individually". In the current code they are stored and returned by `GET /api/v1/feeds/{id}/effective-policy`, but no read path consults them. The behaviour on this page applies regardless of those settings.
:::

## Resolve explain

```
GET /api/v1/feeds/{id}/resolve?coordinate=<coordinate>[&namespace=<namespace>]
```

The endpoint explains the read decision for one coordinate without contacting any upstream. It needs `read` on the feed. Sources the caller cannot read are left out of `sources` and counted in `hidden_sources`. If the owner itself is unreadable, the answer is `not_found` with no source details, which is the same answer a read would give. When `namespace` is omitted, the owner's namespace is used.

```sh
curl -fsS -G "$ANT/api/v1/feeds/$FEED/resolve" -H "Authorization: Bearer $ANT_TOKEN" \
  --data-urlencode 'coordinate=@myorg/ui'
```

### Response fields (`ResolveExplanation`)

| Field                | Meaning                                                                                                                                                                                                                                                                                                                                                                                                                   |
| -------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `status`             | `served`: an attached source owns the coordinate. `pull_through`: no owner, and at least one remote source would be asked on first read. `not_found`: no owner and no remote candidate. `namespace_mismatch`: the owner holds it under another namespace. `federated`: a local miss on a federated feed that would be proxied or redirected to another member.                                                            |
| `serving`            | The owning source, when `status` is `served`                                                                                                                                                                                                                                                                                                                                                                              |
| `sources[]`          | Readable attached sources in read order, each with a `role` and a human-readable `reason`                                                                                                                                                                                                                                                                                                                                 |
| `sources[].role`     | `serving`, `shadowed` (also has the coordinate but has lower priority than the owner), `absent` (does not have it, or is not reached), or `pull_through_candidate` (a remote source that would be tried)                                                                                                                                                                                                                  |
| `sources[].versions` | Number of versions of the coordinate in that source                                                                                                                                                                                                                                                                                                                                                                       |
| `cache`              | Freshness of mutable metadata when the owner is a remote source. `state` is `not_applicable` (local or replica owner), `not_cached`, `fresh` or `stale`. Also includes `last_cached_at`, `last_accessed_at`, `age_seconds`, `expires_at` and the source's `metadata_ttl_seconds` and `stale_if_error_seconds`. `servable_stale_on_error` reports whether stale metadata would still be served if the upstream failed now. |
| `hidden_sources`     | Attached sources omitted because the caller cannot read them                                                                                                                                                                                                                                                                                                                                                              |
| `federation`         | For federated feeds: the route plan for a local miss (`decision` `local`, `proxy`, `redirect`, `off`, `no_candidates` or `unavailable`, with ranked candidates)                                                                                                                                                                                                                                                           |

`cache` is derived from the newest cached asset of the package and the source's TTL settings. Immutable artifact bytes never expire. The state reports when the mutable upstream metadata for the package would be revalidated.

### Examples

A feed `npm` with `npm-local` (priority 0) and the `npmjs` remote (priority 1).

::: code-group

```json [local package]
{
  "feed_id": "6f1c2a8e-3b7d-4c55-9a0e-2d4b8f7e1a10",
  "coordinate": "@myorg/ui",
  "namespace": "@myorg",
  "status": "served",
  "serving": {
    "source_id": "0b6e2f4a-9c1d-4e8b-a7f3-5d2c1b0a9e88",
    "name": "npm-local",
    "kind": "local",
    "priority": 0,
    "role": "serving",
    "reason": "first source in read order that contains the coordinate owns every version",
    "versions": 4
  },
  "sources": [
    {
      "source_id": "0b6e2f4a-9c1d-4e8b-a7f3-5d2c1b0a9e88",
      "name": "npm-local",
      "kind": "local",
      "priority": 0,
      "role": "serving",
      "reason": "first source in read order that contains the coordinate owns every version",
      "versions": 4
    },
    {
      "source_id": "c3d9a1f0-7e2b-4a6c-8d5e-1f0b2a3c4d5e",
      "name": "npmjs",
      "kind": "remote",
      "priority": 1,
      "role": "absent",
      "reason": "does not contain the coordinate",
      "versions": 0
    }
  ],
  "cache": {
    "state": "not_applicable",
    "metadata_ttl_seconds": 0,
    "stale_if_error_seconds": 0,
    "servable_stale_on_error": false
  },
  "hidden_sources": 0
}
```

```json [cached from upstream]
{
  "feed_id": "6f1c2a8e-3b7d-4c55-9a0e-2d4b8f7e1a10",
  "coordinate": "lodash",
  "namespace": "",
  "status": "served",
  "serving": {
    "source_id": "c3d9a1f0-7e2b-4a6c-8d5e-1f0b2a3c4d5e",
    "name": "npmjs",
    "kind": "remote",
    "priority": 1,
    "role": "serving",
    "reason": "first source in read order that contains the coordinate owns every version",
    "versions": 2
  },
  "sources": [
    {
      "source_id": "0b6e2f4a-9c1d-4e8b-a7f3-5d2c1b0a9e88",
      "name": "npm-local",
      "kind": "local",
      "priority": 0,
      "role": "absent",
      "reason": "does not contain the coordinate",
      "versions": 0
    },
    {
      "source_id": "c3d9a1f0-7e2b-4a6c-8d5e-1f0b2a3c4d5e",
      "name": "npmjs",
      "kind": "remote",
      "priority": 1,
      "role": "serving",
      "reason": "first source in read order that contains the coordinate owns every version",
      "versions": 2
    }
  ],
  "cache": {
    "state": "fresh",
    "last_cached_at": "2026-10-20T09:12:04Z",
    "last_accessed_at": "2026-10-20T09:14:31Z",
    "age_seconds": 147,
    "metadata_ttl_seconds": 300,
    "stale_if_error_seconds": 3600,
    "expires_at": "2026-10-20T09:17:04Z",
    "servable_stale_on_error": false
  },
  "hidden_sources": 0
}
```

```json [not yet cached]
{
  "feed_id": "6f1c2a8e-3b7d-4c55-9a0e-2d4b8f7e1a10",
  "coordinate": "left-pad",
  "namespace": "",
  "status": "pull_through",
  "sources": [
    {
      "source_id": "0b6e2f4a-9c1d-4e8b-a7f3-5d2c1b0a9e88",
      "name": "npm-local",
      "kind": "local",
      "priority": 0,
      "role": "absent",
      "reason": "does not contain the coordinate",
      "versions": 0
    },
    {
      "source_id": "c3d9a1f0-7e2b-4a6c-8d5e-1f0b2a3c4d5e",
      "name": "npmjs",
      "kind": "remote",
      "priority": 1,
      "role": "pull_through_candidate",
      "reason": "not cached; would be fetched from the upstream on first read in read order",
      "versions": 0
    }
  ],
  "cache": {
    "state": "not_cached",
    "metadata_ttl_seconds": 0,
    "stale_if_error_seconds": 0,
    "servable_stale_on_error": false
  },
  "hidden_sources": 0
}
```

:::

When a package exists in both sources, the lower-priority copy shows `"role": "shadowed"` with a reason such as `lower priority than npm-local, which owns this coordinate`. That is the expected outcome when a private package shadows a public name.

### Coordinates per format

The `coordinate` parameter is the identity the adapter stores. Common cases:

| Format                                             | Coordinate                                             | Example                       |
| -------------------------------------------------- | ------------------------------------------------------ | ----------------------------- |
| npm                                                | package name                                           | `@myorg/ui`, `lodash`         |
| Maven                                              | `groupId:artifactId`                                   | `org.example:lib`             |
| OCI                                                | repository name inside the feed, without the feed slug | `library/alpine`, `myorg/app` |
| Go                                                 | module path                                            | `github.com/myorg/mod`        |
| Composer                                           | `vendor/package`                                       | `myorg/tools`                 |
| Ansible                                            | `namespace.name`                                       | `myorg.utils`                 |
| Swift                                              | `scope.name` (lowercase)                               | `myorg.kit`                   |
| Raw                                                | file path                                              | `releases/tool-1.2.tar.gz`    |
| Cargo, PyPI, RubyGems, NuGet, Helm, pub, CocoaPods | package name as normalized by the adapter              | `serde`                       |

::: warning Gap
Some formats use structural coordinates rather than package names: Conda uses `conda/<subdir>`, APK uses `<branch>/<repo>/<arch>`, CRAN uses `src/contrib`, and APT and RPM use distribution or snapshot coordinates. Their exact spelling is not documented per format yet. When unsure, look the coordinate up in the console's package view or in `GET /api/v1/feeds/{id}/packages`.
:::

## Related

- [Publish policies](publish-policies.md) decide where writes go. Reads and writes are independent.
- [Namespaces and scopes](namespaces.md)
- [Fallback behaviour](../host-caching/fallback.md) when Ant or an upstream is down
