# Cache feeds and policies

A feed has a **purpose**. `artifacts` (the default) holds published content: releases, images, packages. `cache` holds build caches that CI writes and reads constantly and nobody needs to keep: buildx layer caches and sccache compilation results. Cache feeds use the same endpoints as other feeds, but have their own eviction policy and different rules. See also [Feed purposes](../concepts/purposes.md).

## What makes a cache feed different

|                                            | `artifacts` feed                                                                       | `cache` feed                                                                                                                                          |
| ------------------------------------------ | -------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| Formats                                    | any                                                                                    | `oci` (buildx registry cache) and `raw` (sccache over WebDAV) only                                                                                    |
| Deletion by retention and storage pressure | only remote-cached content, or release content whose policy has `allow_release_delete` | always, without `allow_release_delete`; tagged versions and the newest version are not exempt                                                         |
| OCI tag overwrite                          | refused (`409`, tags are immutable)                                                    | allowed; the manifest the tag leaves and the layers nothing else names are released at once                                                           |
| Replication and federation                 | yes                                                                                    | never: replication jobs, federation bindings, topology rules and exports refuse a cache feed, and its changes are not recorded in the change sequence |
| `GET /api/v1/packages` (package search)    | listed                                                                                 | hidden unless `purpose=cache` or `all` (or `feed=<id>`)                                                                                               |
| Storage pressure order                     | after cache feeds                                                                      | evicted first, least recently used first                                                                                                              |

Ordinary retention policies and the instance default retention policy never apply to cache sources; only the feed's cache policy and storage pressure do.

## Create cache feeds

A cache feed needs a **local** source that no other feed uses.

**Console.** Feeds, New feed: set Purpose to "Build cache (buildx, sccache)". The format list is then limited to `oci` and `raw`, and the form shows the cache policy fields ("Evict when idle for (days)" and a size limit). The wizard creates and attaches the local source.

**API.** With an administrator token, create a local source and the feed with `POST /api/v1/feeds/batch` (or `POST /api/v1/feeds`; the MCP `create_feed` tool takes the same fields):

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# buildx registry cache: an oci cache feed "buildcache" (7 days idle, 50 GiB)
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: buildcache-source-1' \
  -d '{"name":"buildcache-local","kind":"local","format":"oci"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: buildcache-feed-1' -d @- <<JSON
{"slug":"buildcache","format":"oci","purpose":"cache",
 "cache_policy":{"max_idle_seconds":604800,"max_bytes":53687091200},
 "source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON

# sccache: a raw cache feed "sccache" (7 days idle, 20 GiB)
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: sccache-source-1' \
  -d '{"name":"sccache-local","kind":"local","format":"raw"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: sccache-feed-1' -d @- <<JSON
{"slug":"sccache","format":"raw","purpose":"cache",
 "cache_policy":{"max_idle_seconds":604800,"max_bytes":21474836480},
 "source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

Creating a cache feed with any other format answers `400 cache feeds support only the oci and raw formats`.

Endpoints:

| Client          | Endpoint                                                                                                                                                                           |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| buildx / docker | `ant.example.com/buildcache/<repository>:<tag>`, for example `ant.example.com/buildcache/myorg/myrepo:main-amd64` (the first path segment is the feed, the rest is the repository) |
| sccache         | `https://ant.example.com/feeds/sccache/raw` (WebDAV on the raw feed)                                                                                                               |

## Grant CI access

A CI identity needs `read` and `publish` on the cache feeds, and `delete` if it cleans up branch caches. Give a service principal exactly that and nothing else; scoped tokens and GitHub OIDC trust rules can only narrow what the principal holds.

```sh
BC=<buildcache feed id>; SC=<sccache feed id>
P=$(curl -fsS -X POST $ANT/api/v1/principals -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-principal' \
  -d '{"subject":"ci-cache","kind":"service"}')
R=$(curl -fsS -X POST $ANT/api/v1/roles -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-role' -d '{"name":"ci-cache"}')
curl -fsS -X PUT $ANT/api/v1/roles/$(jq -r .id <<<"$R")/grants -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-grants' \
  -H "If-Match: \"$(jq -r .revision <<<"$R")\"" -d @- <<JSON
{"grants":[
 {"kind":"artifact","effect":"allow","feed_id":"$BC","actions":["read","publish","delete"]},
 {"kind":"artifact","effect":"allow","feed_id":"$SC","actions":["read","publish","delete"]}]}
JSON
curl -fsS -X PUT $ANT/api/v1/principals/$(jq -r .id <<<"$P")/roles -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-assign' \
  -H "If-Match: \"$(jq -r .revision <<<"$P")\"" -d "{\"role_ids\":[\"$(jq -r .id <<<"$R")\"]}"
```

For GitHub Actions, add a trust rule whose scopes name the two cache feeds (see [GitHub OIDC trust](../identity/github-oidc.md) and [GitHub Actions](../ci/github-actions.md)). The exchanged token then holds read, publish and delete on the two cache feeds and on no other feed. For OCI, grants and scopes with a `namespace` match the repository name up to its last slash exactly: the namespace of `buildcache/myorg/myrepo` is `myorg`.

## Cache policy

The cache policy belongs to the feed (`cache_policy` on create and on `PATCH /api/v1/feeds/{id}`; in the console on the feed's Settings tab, summarized in its header):

| Field              | Meaning                                                                                                                                                                                                                                                   |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `max_idle_seconds` | Required, at least 60 (default 604800, 7 days). Content whose latest of created, last accessed and last downloaded is older than this is evicted. Downloads are counted by the traffic flusher, so idle time can lag a recent read by one flush interval. |
| `max_bytes`        | Optional. When the distinct blob bytes the feed references exceed it, the least recently used content is evicted until it is under the limit. Absent means no limit; `0` on a `PATCH` removes the limit.                                                  |

Eviction works on whole manifests (OCI) or files (raw): a layer is never evicted on its own while a live manifest still names it, and a manifest takes the layers only it named along.

Metrics, labelled by feed purpose: `ant_storage_evicted_versions_total` and `ant_storage_evicted_bytes_total{purpose,reason}` with reasons `cache_idle`, `cache_max_bytes` and `pressure_lru`, plus `ant_feed_used_bytes{purpose="cache",feed}` and `ant_feed_max_bytes{purpose="cache",feed}`. The console shows the bytes each cache feed holds on its Settings page, on Retention and cleanup, and on the Overview (`cache_feeds` in `GET /api/v1/storage/usage`). See [Observability](../operations/observability.md).

## Tag overwrite releases old content

Re-pushing the same cache tag is the normal case for buildx and needs no cleanup. In a cache feed, when a tag moves to a new manifest:

1. The tag is moved to the new manifest.
2. In the same transaction, the previous manifest is deleted from the metadata if no other tag or manifest refers to it, together with what it named (its config and layers, and for an index its child manifests), transitively, as long as nothing else in the repository still refers to them. Layers shared with the new manifest stay.
3. The blobs that are left without any reference are queued for the **short sweep** instead of waiting for the full garbage-collection grace. No bytes are deleted inline.

This release logic lives in [`internal/cacherelease`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/cacherelease/release.go) and is shared by tag overwrite, branch cleanup and idle/size eviction. It runs while the repository's publication lock is held, so it is serialized with pushes to the same repository.

::: warning Concurrent builds sharing layers
Two concurrent builds that share layers can race: a layer that build B skipped as "already there" can be released by build A's tag overwrite before B's manifest arrives. B's manifest push then fails with `MANIFEST_BLOB_UNKNOWN`, and with `ignore-error=true` its cache export is simply skipped. The next build exports again.
:::

## Branch cleanup

```
DELETE /api/v1/feeds/{id}/cache?prefix=<prefix>
```

Removes cache content by prefix from a cache feed. It requires the `delete` action on the feed (a scoped CI token, including one issued by a GitHub OIDC trust rule, can hold it), answers `404` to a caller that cannot read the feed, `409` if the feed's purpose is not `cache`, and `403` for a caller without `delete` or a feed whose delete policy is `denied`. It is idempotent and audited (`cache.clean` per call, `cache.clean.component` per repository or file).

| Feed | `prefix`                  | Removes                                                                                                                                                          |
| ---- | ------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| oci  | `myorg/myrepo:feature-x-` | the tags of repository `myorg/myrepo` that start with `feature-x-` (`feature-x-amd64`, `feature-x-arm64`), then the manifests and layers nothing else references |
| oci  | `myorg/myrepo`            | every tag of the repository (no colon), and everything only they referenced                                                                                      |
| raw  | `myorg/myrepo/feature-x/` | every file whose path starts with the prefix                                                                                                                     |

```sh
curl -fsS -X DELETE "$ANT/api/v1/feeds/$BC/cache?prefix=myorg/myrepo:feature-x-" \
  -H "Authorization: Bearer $ANT_TOKEN"
```

```json
{
  "feed_id": "…",
  "prefix": "myorg/myrepo:feature-x-",
  "matched": 1,
  "tags_removed": 2,
  "versions_removed": 9,
  "components_removed": 0,
  "bytes_released": 13099487,
  "truncated": false
}
```

| Field                | Meaning                                                                         |
| -------------------- | ------------------------------------------------------------------------------- |
| `matched`            | Repositories (OCI) or files (raw) that had content removed.                     |
| `tags_removed`       | Tags deleted.                                                                   |
| `versions_removed`   | Manifests, layers or files released.                                            |
| `components_removed` | Repositories or files left empty and removed.                                   |
| `bytes_released`     | Bytes of blobs left unreferenced and queued for the short sweep.                |
| `truncated`          | One call handles up to 500 repositories or files; `true` means repeat the call. |

Removal is metadata only; the freed blobs are deleted by garbage collection after the short sweep wait. A workflow on the `delete` event or on a pull request `closed` event is the natural caller; the `cache-cleanup` sub-action of [ant-setup-action](../ci/github-actions.md#sub-action-cache-cleanup) does this for GitHub.

## When the bytes are freed

| Pass                  | When                                                                                      | What it does                                                             |
| --------------------- | ----------------------------------------------------------------------------------------- | ------------------------------------------------------------------------ |
| Pressure loop         | every `ANT_PRESSURE_INTERVAL` (default `5m`), whether or not pressure eviction is enabled | idle and size eviction of cache feeds, and the short sweep               |
| Scheduled maintenance | every `ANT_GC_INTERVAL` (default `6h`)                                                    | applies the cache policies too, plus regular retention and GC            |
| Short sweep           | next pressure-loop pass after `ANT_PRESSURE_SWEEP_GRACE` (default `15m`)                  | deletes blobs freed from a cache feed (eviction, tag overwrite, cleanup) |
| Regular GC            | after `ANT_GC_GRACE` (default `24h`)                                                      | deletes other unreferenced blobs                                         |

The sweep protocol is the same as for regular garbage collection: it locks the `blobs` row, re-checks that nothing references the blob (a blob that regained a reference is kept) and only then deletes the bytes. Above the storage-pressure high watermark, cache feeds are evicted first, least recently used first, before any remote-cached content. See [Storage maintenance](../operations/storage-maintenance.md).

## Convert an existing feed

An administrator (the `admin` action on the feed) can change the purpose with `PATCH /api/v1/feeds/{id}` and `If-Match`; the change is audited as `feed.purpose.change` with the old and new purpose. In the console, the Purpose field on the feed's settings does the same.

```sh
FEED=$(curl -fsS "$ANT/api/v1/feeds?prefix=sccache" -H "$H" | jq -r '.items[]|select(.slug=="sccache").id')
REV=$(curl -fsS -D- -o /dev/null "$ANT/api/v1/feeds/$FEED" -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PATCH "$ANT/api/v1/feeds/$FEED" -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: sccache-to-cache-1' \
  -d '{"purpose":"cache","cache_policy":{"max_idle_seconds":604800,"max_bytes":21474836480}}'
```

The server refuses the conversion with `409` and the reason when:

- the feed has a source that is not local, or a source another feed also uses;
- the feed takes part in replication or federation (a replication job into it, a federation binding, or a home-feed record).

Remove those first. For a federated feed that means deleting its replication rule, its topology key and its binding on the authority (`DELETE /api/v1/federation/replication/rules/{id}`, `DELETE /api/v1/federation/feeds/{fed}/{slug}`, `DELETE /api/v1/federation/feed-bindings/{fed}/{slug}`) and the replication job and binding on every replica (`DELETE /api/v1/replication/jobs/{id}`). Converting back to `artifacts` is always allowed; existing content simply stops being evictable by the cache policy.

## Verification in the repository

`go run ./scripts/cachesmoke` starts a disposable PostgreSQL and Ant, creates both cache feeds and a CI token scoped to them, and drives real sccache (through cargo) and real `docker buildx`: cold build, second build served from the cache, tag overwrite with the short sweep, branch cleanup, and the refusals for non-cache feeds and feeds outside the CI grants. It needs docker, cargo, sccache and the PostgreSQL binaries.
