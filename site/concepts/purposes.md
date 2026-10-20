# Feed purposes

Every feed has a **purpose**: `artifacts` or `cache`. The purpose controls how long content is kept, whether it is replicated, and whether it appears in package search. Choose it when the feed is created. An administrator can change it later.

| Purpose               | Use for                                                                                                                                            |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| `artifacts` (default) | Published releases, images and packages, and pull-through caches of public registries                                                              |
| `cache`               | CI build caches that are written and read constantly and that nobody needs to keep: BuildKit registry layer caches and sccache entries over WebDAV |

## Differences

|                                            | `artifacts` feed                                                                       | `cache` feed                                                                                                                                           |
| ------------------------------------------ | -------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Formats                                    | Any                                                                                    | `oci` (BuildKit registry cache) and `raw` (sccache over WebDAV) only                                                                                   |
| Deletion by retention and storage pressure | Only remote-cached content, or release content whose policy has `allow_release_delete` | Always, without `allow_release_delete`. Tagged versions and the newest version are not exempt.                                                         |
| OCI tag overwrite                          | Refused (`409`, tags are immutable)                                                    | Allowed. The manifest the tag leaves, and layers that nothing else names, are released at once.                                                        |
| Replication and federation                 | Yes                                                                                    | Never. Replication jobs, federation bindings, topology rules and exports refuse a cache feed, and its changes are not recorded in the change sequence. |
| `GET /api/v1/packages`                     | Listed                                                                                 | Hidden unless `purpose=cache` or `purpose=all` is passed (or `feed=<id>`)                                                                              |
| Eviction order under storage pressure      | After cache feeds                                                                      | First, least recently used first                                                                                                                       |

## Cache policy

A cache feed has its own `cache_policy`:

| Field              | Meaning                                                                                                                                                                                                                                        |
| ------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `max_idle_seconds` | Minimum 60, default 604800 (7 days). Content whose latest of created, last accessed and last downloaded is older than this is evicted. Downloads are counted by the traffic flusher, so idle time can lag a recent read by one flush interval. |
| `max_bytes`        | Optional. When the distinct blob bytes the feed references exceed it, the least recently used content is evicted until the feed is under the limit. Absent means no limit, and `0` on a `PATCH` removes it.                                    |

Eviction works on whole OCI manifests or whole Raw files. A layer is never evicted on its own while a live manifest still names it, and a manifest takes the layers only it named along with it. Ordinary retention policies and the instance default policy never apply to cache sources.

Blobs freed from a cache feed (by eviction, tag overwrite or cleanup) are deleted after a short sweep grace of `ANT_PRESSURE_SWEEP_GRACE` (default 15 minutes) rather than the full garbage-collection grace (`ANT_GC_GRACE`, default 24 hours). The pressure loop runs every `ANT_PRESSURE_INTERVAL` (default 5 minutes). It applies idle and size eviction and the short sweep whether or not storage-pressure eviction is enabled.

## Constraints

- **Formats.** Creating a cache feed with any other format answers `400 cache feeds support only the oci and raw formats`.
- **Local sources only, never shared with release content.** A cache feed accepts only local sources. A source attached to a cache feed cannot also be attached to a feed that is not a cache feed, and the reverse is also refused, because retention treats every source of a cache feed as evictable. The console creates and attaches a dedicated local source in the feed wizard.
- **Changing the purpose.** `PATCH /api/v1/feeds/{id}` with `If-Match` requires the `admin` action on the feed and is audited as `feed.purpose.change`. Converting to `cache` is refused with `409` (and the reason) when:
  - the feed has a source that is not local, or that any other feed also uses
  - the feed takes part in replication or federation (a replication job into it, a federation binding, or a home-feed record)

  Converting back to `artifacts` is always allowed. Existing content then simply stops being evicted by the cache policy.

- **Branch cleanup.** `DELETE /api/v1/feeds/{id}/cache?prefix=...` works only on cache feeds and answers `409` otherwise.

## Create a cache feed

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: buildcache-source-1' \
  -d '{"name":"buildcache-local","kind":"local","format":"oci"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: buildcache-feed-1' -d @- <<JSON
{"slug":"buildcache","format":"oci","purpose":"cache",
 "cache_policy":{"max_idle_seconds":604800,"max_bytes":53687091200},
 "source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The same fields exist on `POST /api/v1/feeds` and on the MCP `create_feed` tool. Client setup and CI identities are covered in [build caches](../build-caches/index.md), [buildx registry cache](../build-caches/buildx.md) and [sccache over WebDAV](../build-caches/sccache.md).

::: tip
A pull-through mirror of Docker Hub is an `artifacts` feed with a remote source, not a cache feed. Its cached content is already evictable by retention and storage pressure as remote-cached content. A cache feed cannot have a remote source.
:::
