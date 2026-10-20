# Feeds and sources

A **feed** is what a package client points at. A **source** is where content lives or comes from. A feed combines an ordered set of same-format sources into one endpoint and decides which source answers a read and which source receives a write.

## Feeds

A feed has one format and a slug that is unique per instance. The slug appears in every client URL:

| Format                 | Client endpoint                                                                                                |
| ---------------------- | -------------------------------------------------------------------------------------------------------------- |
| `oci`, `oci-artifacts` | `https://ant.example.com/v2/<slug>/<repository>/...`, image refs `ant.example.com/<slug>/<repo>:<tag>`         |
| all others             | `https://ant.example.com/feeds/<slug>/<format-path>/...`, for example `https://ant.example.com/feeds/npm/npm/` |

Feed fields (`POST /api/v1/feeds`, schema `CreateFeed`):

| Field            | Meaning                                                                                                                                                                                           |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `slug`           | `^[a-z0-9][a-z0-9_-]{0,62}$`. You can change it later with `PATCH`, but clients use it in their URLs.                                                                                             |
| `format`         | One of `apk ansible apt cocoapods composer conan conda oci git-lfs go helm huggingface maven npm nuget oci-artifacts p2 pub pypi cran raw rubygems cargo swift terraform rpm`. Fixed at creation. |
| `anonymous_read` | Default `false`. When `true`, unauthenticated `GET`/`HEAD` requests are served as the internal `anonymous-reader` principal, which only has `read` on such feeds. Writes always need credentials. |
| `purpose`        | `artifacts` (default) or `cache`. See [feed purposes](purposes.md).                                                                                                                               |
| `cache_policy`   | Only for cache feeds: `max_idle_seconds` (minimum 60, default 604800) and `max_bytes` (`0` removes the limit).                                                                                    |
| `revision`       | Optimistic concurrency counter. Every change to the feed increments it: attachments, read order, publish policy, settings. Mutations send it as `If-Match`.                                       |

Deleting a feed (`DELETE /api/v1/feeds/{id}`) keeps its sources and their content. Only the attachments, the publish policy, feed-scoped grants and retention policies are removed. The request is refused with `409` while replication jobs target the feed, unless `force=true` is passed.

## Sources

A source has a name, a kind and a format (`POST /api/v1/sources`, schema `CreateSource`).

| Kind      | Holds                                     | Writes                                          | Configured by                                                                                                                              |
| --------- | ----------------------------------------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `local`   | Artifacts published to Ant                | Yes. The only kind a publish policy can target. | Nothing extra                                                                                                                              |
| `remote`  | Artifacts cached from one upstream origin | Never. Publishes never go upstream.             | `PUT /api/v1/sources/{id}/remote-config`, optional `PUT /api/v1/sources/{id}/remote-credential`                                            |
| `replica` | Content replicated from a peer instance   | Never                                           | A replication job creates it and attaches it at the lowest precedence of the destination feed. See [replication](../replication/index.md). |

A source can only be attached to feeds of the same format. The same source can be attached to more than one feed. For example, one `npmjs` remote source can serve several npm feeds and share one cache. The exception is a [cache feed](purposes.md). It accepts only local sources, and those sources cannot also be attached to a feed that is not a cache feed.

Deleting a source is refused with `409` in these cases:

- it is attached to a feed
- it holds content
- it is a publish-rule target
- it belongs to a replication job

`force=true` detaches the source from feeds and removes its content. Sources that belong to a replication job are never removed this way. Stored bytes are reclaimed later by garbage collection.

### Remote source configuration

A remote source proxies exactly one origin. The configuration (`RemoteSourceConfig`) is:

| Field                                  | Meaning                                                                                                                                                                                             |
| -------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `origin`                               | HTTPS URL of the upstream registry root, for example `https://registry.npmjs.org` or `https://registry-1.docker.io`                                                                                 |
| `redirect_origins`                     | Up to 16 additional origins the upstream redirects downloads to, or uses as a token realm: CDN hosts, `https://auth.docker.io`. They never receive the source credential.                           |
| `credential_id`                        | Reference to the encrypted credential of this same source                                                                                                                                           |
| `ca_ref`                               | Operator-configured CA ID from `ANT_UPSTREAM_CA_MAP_FILE`. Never a path supplied in the request.                                                                                                    |
| `allow_private`                        | Allow origins that resolve to private addresses                                                                                                                                                     |
| `timeout_seconds`                      | 1 to 300. Bounds connecting and receiving response headers. Streaming bodies use idle timeouts instead.                                                                                             |
| `cache.metadata_ttl_seconds`           | 1 to 86400. How long mutable metadata (indexes, tags, packuments) is fresh.                                                                                                                         |
| `cache.negative_ttl_seconds`           | 1 to 300. How long an upstream `404`/`410` is remembered.                                                                                                                                           |
| `cache.stale_if_error_seconds`         | 0 to 86400. How long expired metadata may still be served when the upstream fails.                                                                                                                  |
| `cache.max_entries`, `cache.max_bytes` | Bounds of the in-process metadata cache (1 to 100000 entries, 1 KiB to 1 GiB)                                                                                                                       |
| `cache.max_metadata_bytes`             | Optional. Largest single metadata document held in memory (0 to 1 GiB, 0 = instance default of 24 MiB). Larger documents are streamed uncached where the format supports it, as for Conda repodata. |

The credential (`{"authorization":"Bearer <upstream-token>"}` or `{"api_key":"..."}`, sent as `X-Api-Key`) is stored encrypted under a key bound to the source and is never returned or logged. `POST /api/v1/sources/{id}:test` performs one bounded `GET` of the origin with the stored configuration and records the result in `GET /api/v1/sources/{id}/health`. `DELETE /api/v1/sources/{id}/cache` purges what the source has cached.

::: tip
Remote metadata caches (TTL, negative and stale entries) are kept in process memory. They are lost on restart and are not shared between instances. Cached artifacts are durable rows and blobs and survive restarts.
:::

### Resolution loops

A remote source whose origin is this same instance (its public URL or listen address) depends on the feed that the origin path names. A replication job filling a replica from this instance creates the same kind of dependency. Ant rejects any configuration change that would close a cycle, with the error `feed resolution loop: a -> b -> a`. A feed can therefore never resolve to itself, directly or through remote sources and replicas.

## Attaching sources and read order

Each attachment has a `priority` from 0 to 1000000. Lower values are read first.

| Operation                 | Endpoint                                                                                                                                        |
| ------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| Attach or move one source | `PUT /api/v1/feeds/{id}/sources/{source_id}` with `{"priority": 10}`                                                                            |
| Detach                    | `DELETE /api/v1/feeds/{id}/sources/{source_id}` (empty body)                                                                                    |
| Replace the whole order   | `PUT /api/v1/feeds/{id}/sources` with `{"source_ids": [...]}`. The list must name exactly the attached sources. The index becomes the priority. |
| List in read order        | `GET /api/v1/feeds/{id}/sources`. Sources the caller cannot read are omitted.                                                                   |

All mutations take `If-Match: <feed revision>` and `Idempotency-Key`, and need `configure` on both the feed and the source. `POST /api/v1/feeds/batch` creates a feed, attaches sources in the order of `source_ids` and sets the publish policy in one transaction.

The usual shape is local sources first, then remote sources:

| Priority | Source      | Kind                                        |
| -------- | ----------- | ------------------------------------------- |
| 0        | `npm-local` | local                                       |
| 1        | `npmjs`     | remote, origin `https://registry.npmjs.org` |

How the order is applied is described in [read order and resolve explain](resolution.md).

## Create a feed

In the console, create the sources on the Sources page (a remote source's upstream settings are on its Upstream tab). Then create the feed on the Feeds page, and attach and order the sources on the feed's detail page. The same result with the API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: npm-local-1' \
  -d '{"name":"npm-local","kind":"local","format":"npm"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: npm-remote-1' \
  -d '{"name":"npmjs","kind":"remote","format":"npm"}' | jq -r .id)

REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: npm-remote-cfg-1' -d '{
  "origin":"https://registry.npmjs.org","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: npm-feed-1' -d @- <<JSON
{"slug":"npm","format":"npm","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The feed is now available at `https://ant.example.com/feeds/npm/npm/`. Per-format client setup is on each [format page](../formats/index.md).

## Permissions

Source administration does not grant read access to a feed, and feed administration does not grant source administration. Reading through a feed needs `read` on the feed's artifacts. Resources and actions are described in [roles and grants](../identity/roles.md). Tokens can be narrowed further to a feed and a [namespace](namespaces.md).
