# Raw files and WebDAV

A raw feed is a generic, path-addressed file store over plain HTTP. You `PUT` bytes at a path and `GET` them back. Every path is an immutable artifact with a SHA-256 digest, audited like any other publish. Raw feeds can also proxy any HTTPS origin path for path, which suits GitHub release downloads and vendor download sites. A small WebDAV subset lets WebDAV-only clients such as sccache use a raw feed as a build cache.

Endpoint: `https://ant.example.com/feeds/<slug>/raw/<path>` (feed format `raw`).

## Support matrix

| Publish    | Consume                                        | Remote (pull-through)                       | Replica                    | Notes                                            |
| ---------- | ---------------------------------------------- | ------------------------------------------- | -------------------------- | ------------------------------------------------ |
| Yes, `PUT` | Yes, `GET`/`HEAD` with ranges and conditionals | Yes, any HTTPS origin, mapped path for path | Yes, for `artifacts` feeds | `purpose: cache` raw feeds are never replicated. |

Methods on `/feeds/<slug>/raw/<path>`:

| Method        | Behaviour                                                                                                                                                                                                                                                                                     |
| ------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `PUT`         | Store the body at `<path>`. A new path answers `201`, an identical replay `204`, and different bytes or a different media type at an existing path `409`.                                                                                                                                     |
| `GET`, `HEAD` | Return the bytes with `ETag: "sha256:<hex>"`, `X-Checksum-Sha256`, the stored `Content-Type` and `Content-Disposition: attachment`. Supports `If-Match`, `If-None-Match`, `If-Range`, and single, suffix, open-ended and multipart ranges (`416` when unsatisfiable). `HEAD` ignores `Range`. |
| `DELETE`      | Remove the path (`204`). Gated by the core delete permission and the feed's delete policy, audited, and tombstoned for replication. The bytes are reclaimed later by garbage collection.                                                                                                      |
| `OPTIONS`     | WebDAV discovery (`DAV: 1`).                                                                                                                                                                                                                                                                  |
| `PROPFIND`    | Existence, size and `getlastmodified` of one file or one virtual collection.                                                                                                                                                                                                                  |
| `MKCOL`       | Checks that you may publish under the path and answers `201`. Creates nothing.                                                                                                                                                                                                                |

## Limits

- A path (after the `raw/` prefix) can be at most 256 bytes, and the feed slug at most 128. These are rejected with `400`: traversal (`..`), encoded separators, control characters, empty segments, and a trailing slash on `PUT`/`GET`.
- No server-side size ceiling applies to a single object: disk and quota are an operator concern. Without both `X-Checksum-Sha256` and a known `Content-Length`, the upload is spooled to a private temporary file first (`$TMPDIR`, or `<storage dir>/spool`). Chunked uploads of unknown length work.
- Concurrent uploads per process are bounded by `ANT_UPLOAD_SLOTS`. Further uploads wait for a free slot.
- Paths are immutable. There is no overwrite: delete the path first, if policy allows. Conditional writes (`If-Match`, `If-None-Match`, `If-Unmodified-Since`), partial writes (`Content-Range`) and encoded bodies (`Content-Encoding`) on `PUT` answer `501`.
- There is no directory listing or search over HTTP.
- Responses carry `Cache-Control: private, no-store` and `X-Content-Type-Options: nosniff`.

## Create the feed

In the console, open **Feeds**, choose **New feed** and pick format `raw`. For a build cache, choose purpose **cache** (see [Feed purposes](../concepts/purposes.md)). Attach a local source and make it the publish target, or attach a remote source for a proxy.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: raw-local-1' \
  -d '{"name":"raw-local","kind":"local","format":"raw"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: raw-feed-1' -d @- <<JSON
{"slug":"files","format":"raw","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

Each path is its own ownership unit. With several sources attached, the first source in read order that holds a path serves it. See [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md) (publish rules can route path prefixes to different local sources).

## Remote upstreams

A remote raw source maps `/feeds/<slug>/raw/<path>` to `<origin>/<path>`. The origin can include a base path. On a miss, the bytes stream to the client while they are staged, and the cache entry commits only on a clean end of stream with the declared length. Later reads are local hits with ranges and conditionals, and they keep working while the upstream is down. Concurrent misses for one path share one upstream request.

Redirects are followed, at most five hops, only to hosts in `redirect_origins`. The source credential is sent only to the origin, never to a redirect host.

| Upstream                 | `origin`                            | `redirect_origins`                                                                      | Example feed path                                               |
| ------------------------ | ----------------------------------- | --------------------------------------------------------------------------------------- | --------------------------------------------------------------- |
| GitHub raw content       | `https://raw.githubusercontent.com` | none                                                                                    | `golang/go/go1.22.0/LICENSE`                                    |
| GitHub release downloads | `https://github.com`                | `https://objects.githubusercontent.com`, `https://release-assets.githubusercontent.com` | `myorg/myrepo/releases/download/v1.2.3/tool-linux-amd64.tar.gz` |
| A vendor download site   | `https://downloads.example.com/pub` | the CDN host it redirects to, if any                                                    | `tool/1.0/tool-1.0.tar.gz`                                      |

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: raw-github-1' \
  -d '{"name":"github-releases","kind":"remote","format":"raw"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: raw-github-cfg-1' -d '{
  "origin":"https://github.com",
  "redirect_origins":["https://objects.githubusercontent.com","https://release-assets.githubusercontent.com"],
  "allow_private":false,"timeout_seconds":300,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: raw-github-feed-1' -d @- <<JSON
{"slug":"github-releases","format":"raw","source_ids":["$REM"]}
JSON

curl -fsSLO -u token:$ANT_TOKEN \
  https://ant.example.com/feeds/github-releases/raw/myorg/myrepo/releases/download/v1.2.3/tool-linux-amd64.tar.gz
```

::: warning Limits

- Every path is cached as an immutable artifact the first time it is read. A mutable upstream URL, such as `releases/latest/download/...` or a file that changes in place, keeps serving the first bytes Ant cached. Proxy only versioned URLs.
- The upstream publishes no digest for a raw file, so a fill is checked by length only (against the declared `Content-Length`), not against a hash.
- The 256-byte path limit applies to proxied paths too.
- GitHub release downloads use the redirect hosts shown above. The `raw.githubusercontent.com` row ran in the conformance harness. The release-download row reuses the redirect origins the Helm conformance run uses for GitHub-hosted charts, but it has not been run as a raw feed.

:::

## Client configuration

### Credentials

Send an Ant token (service token or personal access token) as `Authorization: Bearer <TOKEN>`, or as the password of HTTP Basic with any username. Reading needs `read`, `PUT` and `MKCOL` need `publish`, and `DELETE` needs `delete`. With anonymous reads enabled on the feed, and `anonymous` allowed by the client auth policy, `GET` and `HEAD` work without credentials. Writes are never anonymous (see [Package client auth](../identity/client-auth.md)).

::: code-group

```sh [curl]
# upload, with an end-to-end checksum
curl -fsS -H "Authorization: Bearer $ANT_TOKEN" -X PUT \
  -H "Content-Type: application/gzip" \
  -H "X-Checksum-Sha256: $(sha256sum dist.tar.gz | cut -d' ' -f1)" \
  --data-binary @dist.tar.gz \
  https://ant.example.com/feeds/files/raw/myapp/1.4.0/dist.tar.gz

# download, resume, inspect
curl -fsSO -H "Authorization: Bearer $ANT_TOKEN" https://ant.example.com/feeds/files/raw/myapp/1.4.0/dist.tar.gz
curl -fsS -C - -O -H "Authorization: Bearer $ANT_TOKEN" https://ant.example.com/feeds/files/raw/myapp/1.4.0/dist.tar.gz
curl -fsSI -u token:$ANT_TOKEN https://ant.example.com/feeds/files/raw/myapp/1.4.0/dist.tar.gz

# delete
curl -fsS -X DELETE -H "Authorization: Bearer $ANT_TOKEN" https://ant.example.com/feeds/files/raw/myapp/1.4.0/dist.tar.gz
```

```sh [wget]
wget --header="Authorization: Bearer $ANT_TOKEN" \
  https://ant.example.com/feeds/files/raw/myapp/1.4.0/dist.tar.gz
```

:::

The `Content-Type` sent on `PUT` is stored and returned on `GET`. When none is sent, `application/octet-stream` is stored. A `PUT` that sends a different media type for the same bytes at an existing path is a conflict (`409`).

### WebDAV

The WebDAV support is a minimal class-1 subset, built so that sccache's WebDAV backend (OpenDAL) works:

- `OPTIONS` answers `DAV: 1` and `Allow: OPTIONS, GET, HEAD, PUT, PROPFIND, MKCOL`.
- `PROPFIND` reports only the requested resource: the `Depth` header is ignored and children are never listed. A file reports `getcontentlength`, `getetag`, `getcontenttype` and `getlastmodified`. A collection is virtual. It exists once a file lives below it, the feed root always exists, and it reports `getlastmodified` because OpenDAL requires it.
- `MKCOL` on a path with a trailing slash checks publish permission and answers `201`. On the feed root, or on a path without a trailing slash, it answers `405`. A request body answers `415`.
- `COPY`, `MOVE`, `PROPPATCH` and `LOCK`/`UNLOCK` are not supported.

Because collections cannot be listed, general-purpose WebDAV clients that browse or sync directories (file managers, `davfs2` mounts, `rclone sync`) see empty directories and cannot work against a raw feed. Use plain HTTP for general file transfer. Use WebDAV for clients that address keys directly, such as sccache.

### sccache over WebDAV

Point sccache's WebDAV backend at a raw feed, ideally a `purpose: cache` feed so that entries are evicted by idle time and size and are never replicated:

```sh
export SCCACHE_WEBDAV_ENDPOINT=https://ant.example.com/feeds/sccache/raw
export SCCACHE_WEBDAV_TOKEN=$ANT_TOKEN          # or SCCACHE_WEBDAV_USERNAME=token + SCCACHE_WEBDAV_PASSWORD=$ANT_TOKEN
export SCCACHE_WEBDAV_KEY_PREFIX=myproject
export RUSTC_WRAPPER=sccache
```

The real sccache 0.17 client has been verified: cold builds write, warm builds hit, and no cache errors occur. A `PUT` of different bytes for an existing key is a `409`, which sccache logs as a non-fatal cache-write failure. The full setup, including the cache feed, policy and CI wiring, is in [sccache over WebDAV](../build-caches/sccache.md). Cache feeds in general are covered in [Build caches](../build-caches/index.md).

### GitHub Actions

Get a short-lived token with the OIDC exchange in [GitHub Actions](../ci/github-actions.md) (trust rules: [GitHub OIDC trust](../identity/github-oidc.md)). Use it as the Bearer token for `curl`, or as `SCCACHE_WEBDAV_TOKEN`.

## Troubleshooting

| Symptom                             | Cause                                                                                                                                                                                                                        |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `400`                               | An invalid path (traversal, encoded `/`, empty segment, trailing slash, longer than 256 bytes), an invalid `X-Checksum-Sha256`, a body that does not match the declared length or checksum, or an unparsable `Content-Type`. |
| `401`                               | No credentials or invalid credentials.                                                                                                                                                                                       |
| `403`                               | The token lacks the permission for the method, or the client auth policy refuses the credential type.                                                                                                                        |
| `404`                               | The path is not in any attached source, and no remote source has it.                                                                                                                                                         |
| `409` on `PUT`                      | The path already exists with different bytes or a different media type. Paths are immutable.                                                                                                                                 |
| `501` on `PUT`                      | The client sent `If-Match`, `If-None-Match`, `If-Unmodified-Since`, `Content-Range` or `Content-Encoding`.                                                                                                                   |
| `503` on a remote path              | An upstream error, or a redirect to a host that is not in `redirect_origins` (refused by policy).                                                                                                                            |
| Truncated download on a remote miss | The upstream connection ended early. The staged bytes are discarded and the next request retries the fill.                                                                                                                   |

For cache feeds, eviction and pressure, see [Build caches](../build-caches/index.md).
