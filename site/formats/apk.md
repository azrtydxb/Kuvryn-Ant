# APK (Alpine)

The APK adapter serves Alpine APK v2 repositories to `apk`. A feed can hold packages you upload, with an `APKINDEX.tar.gz` signed by a key that belongs to that feed. It can also proxy and cache an upstream mirror such as `dl-cdn.alpinelinux.org`, passing upstream's signed index through unchanged.

Endpoint: `https://ant.example.com/feeds/<slug>/apk/` (feed format `apk`). A repository line in `/etc/apk/repositories` is `<endpoint>/<branch>/<repository>`, for example `.../apk/v3.24/main`.

## Support matrix

| Publish                             | Consume                                | Remote (pull-through)                                  | Replica                       | Notes                                |
| ----------------------------------- | -------------------------------------- | ------------------------------------------------------ | ----------------------------- | ------------------------------------ |
| Yes, `PUT` of a `.apk` (feed admin) | Yes, `apk` (apk-tools v2 index format) | Yes, the upstream `APKINDEX.tar.gz` is served verbatim | Yes, through core replication | The v3 index format is out of scope. |

Served routes (all under `/feeds/<slug>/apk/`):

| Method        | Path                                           | Purpose                                                     |
| ------------- | ---------------------------------------------- | ----------------------------------------------------------- |
| `GET`, `HEAD` | `<branch>/<repo>/<arch>/APKINDEX.tar.gz`       | The signed index (generated per request for local channels) |
| `GET`, `HEAD` | `<branch>/<repo>/<arch>/<name>-<ver>-r<N>.apk` | Download a package                                          |
| `PUT`         | the same package path                          | Upload a package (feed admin)                               |
| `DELETE`      | the same package path                          | Remove a package (feed admin, local sources only)           |
| `GET`, `HEAD` | `keys/ant-<feed8>.rsa.pub`                     | The feed's PEM public key for `/etc/apk/keys`               |

A `noarch` package (`A:noarch`) is served from every arch path of its branch and repository, and from `noarch/`.

## Limits

- One uploaded package can be at most 64 MiB. A local channel can hold at most 4,096 packages, and its index is regenerated on every request.
- On upload, `.PKGINFO` is parsed from the control segment. The name and version must match the file name, and `arch` must equal the arch in the path or be `noarch`. The package bytes and its signature segment are stored unchanged. Upload signatures are kept but not checked, because there is no trusted-key import yet.
- One remote package can be at most 1 GiB.
- Responses carry `Cache-Control: private, no-store`.

::: warning Gap
The real-client conformance run with apk-tools 3.0.8 on 2026-10-19 passed install, upgrade and delete. It failed to install a `noarch` package that was uploaded under an arch path. The code has since gained a `noarch` lookup across arch paths, but no later real-client run has confirmed it. Remote pull-through is covered by integration tests only. It has not been run against the real `dl-cdn.alpinelinux.org`.
:::

## Create the feed

In the console, open **Feeds**, choose **New feed** and pick format `apk`. Attach a local source (for uploads, with a publish policy) or a remote source with its origin set on the **Upstream** tab.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: apk-local-1' \
  -d '{"name":"apk-local","kind":"local","format":"apk"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: apk-feed-1' -d @- <<JSON
{"slug":"alpine-internal","format":"apk","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The origin of a remote APK source is the repository root, the directory that holds `<branch>/<repository>/<arch>/`. The trust model matches APT and RPM:

- The adapter serves `APKINDEX.tar.gz` from upstream byte for byte, through the metadata cache. It never rewrites, merges or re-signs the index, so `apk` checks upstream's signature with the upstream keys in `/etc/apk/keys`. The feed's own key signs only locally published channels.
- A package is served only if the upstream index for its channel lists it. Before the first byte is served, the adapter checks its control segment against the index checksum (`C:Q1...`, the SHA-1 identity `apk` itself uses), its size against `S:`, and its name, version and arch against the file name. It is then cached. A mismatch answers 503 and nothing is cached.
- A channel has one owner. A channel with locally published packages is never extended from upstream.

| Upstream     | `origin`                                | `redirect_origins` |
| ------------ | --------------------------------------- | ------------------ |
| Alpine Linux | `https://dl-cdn.alpinelinux.org/alpine` | none               |

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: apk-alpine-1' \
  -d '{"name":"alpine","kind":"remote","format":"apk"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: apk-alpine-cfg-1' -d '{
  "origin":"https://dl-cdn.alpinelinux.org/alpine","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":86400,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: apk-alpine-feed-1' -d @- <<JSON
{"slug":"alpine","format":"apk","source_ids":["$REM"]}
JSON
```

Limits of the proxy:

- Indexes, and therefore the answer to "which packages exist", come from the in-process metadata cache. Once its stale-if-error window ends, `apk update` fails during an upstream outage, although cached packages stay downloadable.
- Compressed indexes larger than the metadata limit cannot be proxied. Alpine `main` and `community` are well below it.
- Ant does not hold the upstream signing keys and does not check the index signature itself.
- Cached remote packages are not added to any locally generated index.

## Client configuration

### Credentials

`apk` has no separate credentials file. Put HTTP Basic credentials in the repository URL, with any username and an Ant token (service token or personal access token) as the password:

```text
https://token:<TOKEN>@ant.example.com/feeds/alpine/apk/v3.24/main
```

Reading needs the `read` permission on the feed. Uploading and deleting need the `admin` action on the feed. If the feed has anonymous reads enabled and the client auth policy allows `anonymous`, you can drop the credentials from the URL (see [Package client auth](../identity/client-auth.md)).

### Local feed: install the feed key

The feed key is an RSA-2048 key, generated the first time it is needed and stored encrypted on the server. The index signature entry and the key file are both named `ant-<feed8>.rsa.pub`, where `<feed8>` is the first eight hex characters of the feed's UUID. Look the ID up first:

```sh
FEED=$(curl -fsS "$ANT/api/v1/feeds?prefix=alpine-internal" -H "$H" | jq -r '.items[]|select(.slug=="alpine-internal").id')
KEY="ant-${FEED%%-*}.rsa.pub"
wget -q -O "/etc/apk/keys/$KEY" --header "Authorization: Bearer $ANT_TOKEN" \
  "https://ant.example.com/feeds/alpine-internal/apk/keys/$KEY"
```

Any other key name answers 404.

::: code-group

```sh [local feed]
echo "https://token:$ANT_TOKEN@ant.example.com/feeds/alpine-internal/apk/v3.24/main" >> /etc/apk/repositories
apk update
apk add hello-ant
```

```sh [remote feed (Alpine mirror)]
# the upstream keys come with alpine-keys and are already in /etc/apk/keys
cat > /etc/apk/repositories <<EOF
https://token:$ANT_TOKEN@ant.example.com/feeds/alpine/apk/v3.24/main
https://token:$ANT_TOKEN@ant.example.com/feeds/alpine/apk/v3.24/community
EOF
apk update
```

:::

### Publish

```sh
curl -fsS -u token:$ANT_TOKEN -X PUT --data-binary @hello-ant-1.0-r0.apk \
  https://ant.example.com/feeds/alpine-internal/apk/v3.24/main/x86_64/hello-ant-1.0-r0.apk
```

The package and its index entry are published as one atomic bundle. Build packages with `abuild` as usual: the adapter re-signs only the index, not the package.

### GitHub Actions

Get a short-lived token with the OIDC exchange described in [GitHub Actions](../ci/github-actions.md), with trust rules from [GitHub OIDC trust](../identity/github-oidc.md). Then write it into `/etc/apk/repositories` as shown above. Avoid echoing the URL in logs.

## Troubleshooting

| Symptom                                    | Cause                                                                                                                                                               |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `UNTRUSTED signature`                      | For a local feed, the `ant-<feed8>.rsa.pub` key is missing from `/etc/apk/keys`. For a remote feed, the upstream key is missing (install `alpine-keys`).            |
| `401` / `HTTP 401`                         | No credentials or wrong credentials in the repository URL.                                                                                                          |
| `403`                                      | The token lacks read permission, the client auth policy refuses the credential type, or you uploaded or deleted without feed admin.                                 |
| `400` on `PUT`                             | The name or version in `.PKGINFO` does not match the file name, or the arch does not match the path.                                                                |
| `404` on the key                           | The key name does not match the feed's UUID prefix.                                                                                                                 |
| `503` on index or key                      | The server has no secret key ring, or an upstream error left nothing cached. On a remote package, a 503 can also mean the checksum or size did not match the index. |
| `X-Kuvryn-Cache: stale` and `Warning: 110` | The upstream index came from the metadata cache after its TTL because upstream was unreachable.                                                                     |

Adapter internals: [internal/apkrepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/apkrepo/README.md).
