# Build caches: buildx layer cache and sccache

A feed has a **purpose**. `artifacts` (the default) is published content: releases,
images, packages. `cache` is a build cache that CI writes and reads constantly and
nobody needs to keep. Cache feeds have their own endpoints, their own policy and
different rules:

|                                            | `artifacts` feed                                                                       | `cache` feed                                                                                                                                               |
| ------------------------------------------ | -------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Formats                                    | any                                                                                    | `oci` (buildx registry cache) and `raw` (sccache over WebDAV) only                                                                                         |
| Deletion by retention and storage pressure | only remote-cached content, or release content whose policy has `allow_release_delete` | always, no `allow_release_delete`; tagged versions and the newest version are not exempt                                                                   |
| OCI tag overwrite                          | refused (409, tags are immutable)                                                      | allowed; the manifest the tag leaves and the layers nothing else names are released at once                                                                |
| Replication and federation                 | yes                                                                                    | never: replication jobs, federation bindings, topology rules and exports refuse a cache feed, and its changes are not even recorded in the change sequence |
| `GET /api/v1/packages`                     | listed                                                                                 | hidden unless `purpose=cache` or `all` (or `feed=<id>`)                                                                                                    |
| Pressure order                             | after cache feeds                                                                      | evicted first, least recently used first                                                                                                                   |

Cache feeds are created like any other feed; the purpose is chosen at creation or
changed later by an administrator.

## Create the cache feeds

A cache feed needs a **local** source that no other feed uses (the console creates
and attaches it in the feed wizard). With the API, using an administrator token:

```sh
A=https://ant.azrty.com
H="Authorization: Bearer $ANT_ADMIN_TOKEN"
J='Content-Type: application/json'

# 1. buildx registry cache: an oci cache feed named "buildcache"
SRC=$(curl -fsS -X POST $A/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: buildcache-source-1' \
  -d '{"name":"buildcache-local","kind":"local","format":"oci"}' | jq -r .id)
curl -fsS -X POST $A/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: buildcache-feed-1' -d @- <<JSON
{"slug":"buildcache","format":"oci","purpose":"cache",
 "cache_policy":{"max_idle_seconds":604800,"max_bytes":53687091200},
 "source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON

# 2. sccache: a raw cache feed named "sccache"
SRC=$(curl -fsS -X POST $A/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: sccache-source-1' \
  -d '{"name":"sccache-local","kind":"local","format":"raw"}' | jq -r .id)
curl -fsS -X POST $A/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: sccache-feed-1' -d @- <<JSON
{"slug":"sccache","format":"raw","purpose":"cache",
 "cache_policy":{"max_idle_seconds":604800,"max_bytes":21474836480},
 "source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

Creating a cache feed with any other format answers `400 cache feeds support only
the oci and raw formats`. The same fields exist on `POST /api/v1/feeds` and on the
MCP `create_feed` tool.

### Convert an existing feed

An administrator (the `admin` action on the feed) can change the purpose with
`PATCH /api/v1/feeds/{id}` and `If-Match`; the change is audited as
`feed.purpose.change` with the old and new purpose.

```sh
FEED=$(curl -fsS "$A/api/v1/feeds?prefix=sccache" -H "$H" | jq -r '.items[]|select(.slug=="sccache").id')
REV=$(curl -fsS -D- -o /dev/null "$A/api/v1/feeds/$FEED" -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PATCH "$A/api/v1/feeds/$FEED" -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: sccache-to-cache-1' \
  -d '{"purpose":"cache","cache_policy":{"max_idle_seconds":604800,"max_bytes":21474836480}}'
```

The server refuses the conversion (409, with the reason) when the feed has a source
that is not local or that another feed also uses, or when the feed takes part in
replication or federation (a replication job into it, a federation binding, or a
home-feed record). Remove those first; for a federated feed that means deleting its
replication rule, its topology key and its binding on the authority
(`DELETE /api/v1/federation/replication/rules/{id}`, `DELETE
/api/v1/federation/feeds/{fed}/{slug}`, `DELETE
/api/v1/federation/feed-bindings/{fed}/{slug}`) and the `oke-<slug>` job and binding
on every replica (`DELETE /api/v1/replication/jobs/{id}`). Converting back to
`artifacts` is always allowed; existing content simply stops being evictable by
the cache policy.

## Endpoints

| Client          | Endpoint                                                                                                                                                                                                                                                                     |
| --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| buildx / docker | `ant.azrty.com/<cache feed slug>/<repository>:<tag>`, for example `ant.azrty.com/buildcache/org/repo:main-amd64` (the first path segment is the feed, the rest is the repository name; the existing hosted feed works the same way: `ant.azrty.com/oci-hosted/team/app:1.0`) |
| sccache         | `https://ant.azrty.com/feeds/sccache/raw` (WebDAV on the raw feed)                                                                                                                                                                                                           |

## buildx

```sh
echo "$ANT_TOKEN" | docker login ant.azrty.com -u ci --password-stdin
REF=ant.azrty.com/buildcache/$GITHUB_REPOSITORY
TAG=$(echo "$GITHUB_REF_NAME" | tr -c 'A-Za-z0-9_.\n-' '-')-$(uname -m | sed s/x86_64/amd64/\;s/aarch64/arm64/)
docker buildx build \
  --cache-from type=registry,ref=$REF:$TAG \
  --cache-from type=registry,ref=$REF:main-amd64 \
  --cache-to type=registry,ref=$REF:$TAG,mode=max,image-manifest=true,ignore-error=true \
  -t app:ci .
```

- `image-manifest=true` is required: the default cache export is an OCI image
  _index_ whose entries are layers, which no registry that validates indexes (Ant
  included) accepts. With it the cache is an ordinary OCI image manifest whose
  config uses the buildkit cache config media type.
- `ignore-error=true` keeps a cache outage or an expired token from failing the build.
- The tag is the cache identity. Tags are `[A-Za-z0-9_.-]`, at most 128 characters:
  replace the slashes of a branch name. Use one tag per branch and architecture
  (`<branch>-<arch>`), and a `--cache-from` of the default branch as the fallback.
- Re-pushing the same tag is the normal case and needs no cleanup. In a cache feed
  the tag moves to the new manifest, and the previous manifest, its config and every
  layer that no other manifest of the repository names are deleted from the
  metadata in the same transaction. Their blobs are queued for the **short sweep**
  (below) instead of waiting for the 24 hour garbage-collection grace. Layers shared
  with the new manifest stay.

Two concurrent builds that share layers can race: a layer pushed by build B as
"already there" can be released by build A's tag overwrite before B's manifest
arrives, in which case B's manifest push fails (`MANIFEST_BLOB_UNKNOWN`) and, with
`ignore-error=true`, its cache export is simply skipped.

## sccache

Environment (the key prefix selects the sharing scope):

```sh
export RUSTC_WRAPPER=sccache CARGO_INCREMENTAL=0
export SCCACHE_WEBDAV_ENDPOINT=https://ant.azrty.com/feeds/sccache/raw
export SCCACHE_WEBDAV_TOKEN=$ANT_TOKEN                    # bearer token, or:
# export SCCACHE_WEBDAV_USERNAME=ci SCCACHE_WEBDAV_PASSWORD=$ANT_TOKEN   # Basic, token as password
export SCCACHE_WEBDAV_KEY_PREFIX=$GITHUB_REPOSITORY       # org/repo
```

- Keys are content hashes of the compiler invocation, so entries are valid across
  branches. A prefix per repository (`org/repo`) shares them between branches and
  is what you want by default; add the branch (`org/repo/<branch>`) to isolate
  untrusted pull requests, at the cost of sharing.
- `internal/rawrepo/webdav.go` implements what sccache's OpenDAL client uses:
  `OPTIONS`, `MKCOL` for every nested parent collection, `PUT`, `GET`, `HEAD` and
  `PROPFIND` (with `getlastmodified`). Collections are virtual: they exist while a
  file lives below them. A `PUT` with different bytes for an existing key is a
  `409`, which sccache logs and ignores.
- Tested with the real sccache 0.17 client and cargo (`go run ./scripts/cachesmoke`):
  a cold build wrote 3 entries, a build after `cargo clean` had 3 hits of 3
  executed compilations and no cache errors, with both bearer and Basic
  authentication.

## Granting CI identities

A CI job needs `read` and `publish` on the cache feeds, and `delete` if it cleans up
branch caches. Give a service principal exactly that on the cache feeds and nothing
else; scoped tokens (and GitHub OIDC trust rules) can only narrow what the principal
holds, never widen it.

```sh
P=$(curl -fsS -X POST $A/api/v1/principals -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-principal' \
  -d '{"subject":"ci-cache","kind":"service"}')
R=$(curl -fsS -X POST $A/api/v1/roles -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-role' -d '{"name":"ci-cache"}')
BC=<buildcache feed id>; SC=<sccache feed id>
curl -fsS -X PUT $A/api/v1/roles/$(jq -r .id <<<"$R")/grants -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-grants' \
  -H "If-Match: \"$(jq -r .revision <<<"$R")\"" -d @- <<JSON
{"grants":[
 {"kind":"artifact","effect":"allow","feed_id":"$BC","actions":["read","publish","delete"]},
 {"kind":"artifact","effect":"allow","feed_id":"$SC","actions":["read","publish","delete"]}]}
JSON
curl -fsS -X PUT $A/api/v1/principals/$(jq -r .id <<<"$P")/roles -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-assign' \
  -H "If-Match: \"$(jq -r .revision <<<"$P")\"" -d "{\"role_ids\":[\"$(jq -r .id <<<"$R")\"]}"
```

**GitHub Actions** (no stored secret): configure the audience once
(`PUT /api/v1/auth/github-oidc/config {"audience":"ant.azrty.com"}`), then add a
trust rule per repository whose scopes name the two cache feeds:

```sh
curl -fsS -X POST $A/api/v1/auth/github-oidc/trust-rules -H "$H" -H "$J" -H 'Idempotency-Key: ci-cache-rule-myrepo' -d @- <<JSON
{"name":"myrepo cache","org":"myorg","repo":"myrepo","principal_id":"$(jq -r .id <<<"$P")",
 "ttl_seconds":3600,
 "scopes":[{"feed_id":"$BC","actions":["read","publish","delete"]},
           {"feed_id":"$SC","actions":["read","publish","delete"]}]}
JSON
```

In the job, request an ID token with that audience and exchange it for a short-lived
Ant service token (`POST /api/v1/auth/github-oidc/exchange {"token":"<jwt>"}` returns
`access_token` and `expires_at`). That token is what `docker login`,
`SCCACHE_WEBDAV_TOKEN` and the cleanup call use. It holds the intersection of the
principal's grants and the rule's scopes: read, publish and delete on the two cache
feeds, and on no other feed. Using it on any other feed answers 404 or 403, and the
cleanup API refuses a feed that is not a cache feed.

Namespace limits narrow it further for OCI: the namespace of
`buildcache/org/repo` is `org` (the repository name up to its last slash), and
grants and token scopes match a namespace exactly.

## Branch cleanup

```
DELETE /api/v1/feeds/{id}/cache?prefix=<prefix>
```

Requires the `delete` action on the cache feed, answers `404` to a caller that cannot
read the feed, `409` if the feed's purpose is not `cache`, and `403` for a caller
without `delete` (or a feed whose delete policy is `denied`). It is idempotent and
audited (`cache.clean` per call, `cache.clean.component` per repository or file).

| Feed | `prefix`              | Removes                                                                                                                                                      |
| ---- | --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| oci  | `org/repo:feature-x-` | the tags of repository `org/repo` that start with `feature-x-` (`feature-x-amd64`, `feature-x-arm64`), then the manifests and layers nothing else references |
| oci  | `org/repo`            | every tag of the repository, and everything only they referenced                                                                                             |
| raw  | `org/repo/feature-x/` | every file whose path starts with the prefix                                                                                                                 |

```sh
curl -fsS -X DELETE "$A/api/v1/feeds/$BC/cache?prefix=myorg/myrepo:feature-x-" -H "Authorization: Bearer $ANT_TOKEN"
# {"feed_id":"…","prefix":"myorg/myrepo:feature-x-","matched":1,"tags_removed":2,"versions_removed":9,
#  "components_removed":0,"bytes_released":13099487,"truncated":false}
```

One call handles up to 500 repositories or files; `truncated: true` means repeat it.
Removal is metadata only: the freed blobs are deleted by garbage collection after
the short sweep wait, never inline. A workflow on the `delete` event or a pull
request `closed` event is the natural caller.

## Policies

The cache policy belongs to the feed (`cache_policy` on create and `PATCH`, console:
the feed's Settings tab, shown in its header):

- `max_idle_seconds` (default 604800, 7 days): content whose latest of _created_,
  _last accessed_ and _last downloaded_ is older is evicted. Downloads are counted
  by the traffic flusher, so idle time can lag a recent read by one flush interval.
- `max_bytes` (optional; `0` on a PATCH removes it): when the distinct blob bytes
  the feed references exceed it, the least recently used content is evicted until
  it is under the limit.

Eviction works on whole manifests (OCI) or files (raw): a layer is never evicted on
its own while a live manifest still names it, and a manifest takes the layers only it
named along. Ordinary retention policies and the instance default policy never apply to
cache sources.

Passes: the pressure loop (every `ANT_PRESSURE_INTERVAL`, default 5m, whether or not
pressure eviction is enabled) runs idle and size eviction and the short sweep; the
scheduled maintenance run (`ANT_GC_INTERVAL`) applies them too.

**Short sweep.** Blobs freed from a cache feed (eviction, tag overwrite, cleanup) are
marked with a `sweep_after` of `ANT_PRESSURE_SWEEP_GRACE` (default 15m) instead of the
full `ANT_GC_GRACE` (24h), and the next pressure-loop pass deletes them. The sweep
protocol is unchanged: it locks the `blobs` row, re-checks that nothing references it
(a blob that regained a reference is kept) and only then deletes the bytes.

**Storage pressure.** Above the high watermark, cache feeds are evicted first, least
recently used first, before any remote-cached content, and need no
`allow_release_delete`; see `docs/operations/storage-maintenance.md`.

**Metrics** (labelled by feed purpose): `ant_storage_evicted_versions_total` and
`ant_storage_evicted_bytes_total{purpose,reason}` (reasons `cache_idle`,
`cache_max_bytes`, `pressure_lru`), `ant_feed_used_bytes{purpose="cache",feed}` and
`ant_feed_max_bytes{purpose="cache",feed}`. The console shows the bytes each cache feed
holds on Settings, Retention and cleanup, Overview (`cache_feeds` in
`GET /api/v1/storage/usage`).

## Verification

`go run ./scripts/cachesmoke` starts a disposable PostgreSQL and Ant, creates both
cache feeds, a CI token scoped to them, and drives real sccache (cargo) and real
`docker buildx` (needs docker, cargo, sccache and the PostgreSQL binaries): cold
build, second build served from the cache, tag overwrite with the short sweep, branch
cleanup, and the refusals for non-cache feeds and feeds outside the CI grants.
