# CocoaPods

The CocoaPods adapter serves a **CDN-style spec source**, the same layout as `cdn.cocoapods.org`: `CocoaPods-version.yml`, `all_pods_versions_*` shards and `Specs/.../<Pod>.podspec.json`. It also serves the pod source archives you publish. `pod install` resolves against it like any CDN source. A feed can hold your own pods, and it can proxy the public CocoaPods specs.

Source URL: `https://ant.example.com/feeds/<slug>/cocoapods/` (feed format `cocoapods`; keep the trailing slash).

## Support matrix

| Publish                                                                                | Consume                                                                                            | Remote (pull-through)                | Replica                       | Notes                                                                      |
| -------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- | ------------------------------------ | ----------------------------- | -------------------------------------------------------------------------- |
| Adapter-specific: `PUT pods/<Pod>/<version>` (multipart podspec JSON + source archive) | Yes: `pod repo add-cdn`, Podfile `source`, `pod install` (CocoaPods 1.17.0 in the conformance run) | Specs only, from `cdn.cocoapods.org` | Yes, through core replication | No trunk protocol, no git spec repo. A published pod must ship an archive. |

Routes under `/feeds/<slug>/cocoapods/`:

| Method        | Path                                                        | Purpose                                                     |
| ------------- | ----------------------------------------------------------- | ----------------------------------------------------------- |
| `GET`, `HEAD` | `CocoaPods-version.yml`                                     | CDN marker (`prefix_lengths` 1, 1, 1)                       |
| `GET`, `HEAD` | `all_pods_versions_<a>_<b>_<c>.txt`                         | Version shards (MD5 prefix of the pod name)                 |
| `GET`, `HEAD` | `Specs/<a>/<b>/<c>/<Pod>/<version>/<Pod>.podspec.json`      | Podspec                                                     |
| `GET`, `HEAD` | `deprecated_podspecs.txt`                                   | Always empty                                                |
| `GET`, `HEAD` | `archives/<Pod>/<version>/<Pod>-<version>.tar.gz` or `.zip` | Pod source archive (locally published pods)                 |
| `PUT`, `POST` | `pods/<Pod>/<version>`                                      | Publish (multipart: `podspec`, `source`, optional `sha256`) |
| `DELETE`      | `pods/<Pod>/<version>`                                      | Unlist a version (needs delete permission)                  |

## Limits

- There is no `pod trunk push` and no git-based spec repo. Publish is the adapter's own multipart `PUT`. The podspec must be **JSON** (`.podspec.json`). Convert a Ruby podspec with `pod ipc spec MyPod.podspec > MyPod.podspec.json`.
- Publish requires a source archive (tar.gz or zip, at most 256 MiB). The podspec can be at most 1 MiB. The podspec `source` is rewritten to `{"http": <feed archive URL>, "type": ..., "sha256": ...}`. Pods whose source is only a VCS reference cannot be published.
- Archive URLs are written into the podspec at publish time from `ANT_PUBLIC_URL`. If the public URL changes later, previously published podspecs still point to the old one. Without `ANT_PUBLIC_URL`, publish returns 503.
- `DELETE` only **unlists** a version: it disappears from the shards and the bytes stay until GC. An unlisted version cannot be published again (409).
- Package ownership is decided per pod name across the feed's sources. A feed lists at most 50,000 pods.
- Archive downloads need credentials, because the archive URL inside the podspec carries none. Configure `~/.netrc` (see below).

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `cocoapods`. On **Sources**, use **Create source** to create a `local` source, attach it with **Attach source** and make it the **Publish policy** default. To proxy public specs, add a `remote` source with origin `https://cdn.cocoapods.org` and redirect origin `https://cdn.jsdelivr.net` on its **Upstream** tab.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: pods-local-1' \
  -d '{"name":"pods-local","kind":"local","format":"cocoapods"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: pods-remote-1' \
  -d '{"name":"cocoapods-cdn","kind":"remote","format":"cocoapods"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: pods-remote-cfg-1' -d '{
  "origin":"https://cdn.cocoapods.org","redirect_origins":["https://cdn.jsdelivr.net"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: pods-feed-1' -d @- <<JSON
{"slug":"pods","format":"cocoapods","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The origin is the CDN root.

- `all_pods_versions_<a>_<b>_<c>.txt` shards come through the metadata cache and are merged by pod name. A pod owned by a local source keeps only its local versions. For other pods, the upstream list replaces the cached view while the upstream is reachable.
- `Specs/.../<Pod>.podspec.json` is served verbatim and cached on first read.
- **Only specs are proxied.** A public podspec's `source` (git or http) is not rewritten, so clients still download pod sources from their original hosts (usually GitHub). `archives/...` serves only locally published archives.
- `CocoaPods-version.yml` and `deprecated_podspecs.txt` are always answered locally.
- `cdn.cocoapods.org` answers podspec requests with a 301 to `https://cdn.jsdelivr.net`. Redirects to hosts that are not configured are refused, and the client sees 503. `cdn.jsdelivr.net` must therefore be a redirect origin.

| Upstream      | `origin`                    | `redirect_origins`         |
| ------------- | --------------------------- | -------------------------- |
| CocoaPods CDN | `https://cdn.cocoapods.org` | `https://cdn.jsdelivr.net` |

::: warning Gap
The recorded conformance run (2026-10-19) failed the remote scenario (`pod install AFNetworking 4.0.1`) with 503, before `cdn.jsdelivr.net` was added as a redirect origin. The README and the harness now configure it, but no later real-client run is recorded. Pod sources hosted on GitHub are not cached by Ant.
:::

## Client configuration

The adapter accepts a token as `Authorization: Bearer` or as the HTTP Basic password (any username). Unauthenticated requests get `401` with `WWW-Authenticate: Basic realm="CocoaPods"`.

### Credentials

Spec requests and archive downloads both need credentials. CocoaPods downloads `http` sources with `curl --netrc-optional`, so put the token in `~/.netrc`:

```sh
cat >> ~/.netrc <<EOF
machine ant.example.com login token password $ANT_TOKEN
EOF
chmod 600 ~/.netrc
```

The conformance harness also embeds the credentials in the source URL (`https://token:<TOKEN>@ant.example.com/...`). That works, but it stores the token in the repo configuration under `~/.cocoapods/repos`.

### Consume

::: code-group

```sh [register the CDN source]
pod repo add-cdn ant https://ant.example.com/feeds/pods/cocoapods/
pod repo list
```

```ruby [Podfile]
source 'https://ant.example.com/feeds/pods/cocoapods/'

platform :ios, '15.0'
target 'App' do
  pod 'MyPod', '~> 1.0'
end
```

:::

```sh
pod install
```

To resolve public pods through the feed as well, attach the CocoaPods CDN remote source to the same feed and drop `https://cdn.cocoapods.org/` from the Podfile.

### Publish

```sh
pod ipc spec MyPod.podspec > MyPod.podspec.json
tar -czf MyPod-1.0.0.tar.gz Classes LICENSE
curl -fsS -u "token:$ANT_TOKEN" -X PUT \
  -F "podspec=@MyPod.podspec.json;type=application/json" \
  -F "source=@MyPod-1.0.0.tar.gz;type=application/gzip" \
  -F "sha256=$(sha256sum MyPod-1.0.0.tar.gz | cut -d' ' -f1)" \
  https://ant.example.com/feeds/pods/cocoapods/pods/MyPod/1.0.0
```

The pod name and version in the URL identify the version. The token needs publish permission on the feed. `source_files` and the other podspec paths must match the archive layout, because the client unpacks the archive as the pod source.

To unlist a version:

```sh
curl -fsS -u "token:$ANT_TOKEN" -X DELETE \
  https://ant.example.com/feeds/pods/cocoapods/pods/MyPod/1.0.1
```

### GitHub Actions

Exchange the OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)) and write it to `~/.netrc` before `pod install`:

```yaml
- run: |
    printf 'machine ant.example.com login ci password %s\n' "$ANT_TOKEN" > ~/.netrc
    chmod 600 ~/.netrc
    pod install
```

## Troubleshooting

| Symptom                                           | Cause                                                                                                                                                 |
| ------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CDN: ... URL couldn't be downloaded ... 401`     | No credentials for spec requests. Add the `.netrc` entry, or credentials in the source URL.                                                           |
| Specs resolve but the pod download fails with 401 | Archive downloads use `.netrc`, not the source URL credentials.                                                                                       |
| `503` on remote podspecs                          | The upstream redirect to `cdn.jsdelivr.net` was refused (missing redirect origin), or the upstream is unreachable and the spec is not cached.         |
| `X-Kuvryn-Cache: offline` on a shard              | The upstream failed. Only cached and local versions are listed.                                                                                       |
| `409` on publish                                  | The version exists with different content, or it was unlisted (unlisted versions cannot be republished).                                              |
| `400` on publish                                  | The body is not multipart, the podspec is missing or is not JSON, the archive is missing or of an unknown type, or the `sha256` field does not match. |
| Archive URLs point to an old host                 | `ANT_PUBLIC_URL` changed after publishing. Republish under a new version.                                                                             |

Adapter internals: [internal/cocoapods/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/cocoapods/README.md).
