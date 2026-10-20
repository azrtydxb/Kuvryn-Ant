# Conan

The Conan adapter implements the Conan 2 server REST API (`v2/conans/...`, with revisions). `conan upload`, `conan install`, `conan download`, `conan list` and `conan remove` work against it. A feed can hold your own recipes and binary packages, and it can proxy and cache ConanCenter.

Remote URL: `https://ant.example.com/feeds/<slug>/conan` (feed format `conan`).

## Support matrix

| Publish                                                   | Consume                                              | Remote (pull-through)                                                            | Replica                       | Notes                                                                         |
| --------------------------------------------------------- | ---------------------------------------------------- | -------------------------------------------------------------------------------- | ----------------------------- | ----------------------------------------------------------------------------- |
| Yes, native `conan upload` (recipe and package revisions) | Yes, `conan install`, `conan download`, `conan list` | Yes, Conan v2 remotes such as `center2.conan.io`, verified against the manifests | Yes, through core replication | Conan 2 only. Native `conan remove -r` deletes, subject to the delete policy. |

Routes under `/feeds/<slug>/conan/`. A reference is always the four-part path `<name>/<version>/<user>/<channel>`, with `_` for an absent user or channel, which is what Conan 2 sends.

| Method       | Path                                                                                                       | Purpose                                                                                    |
| ------------ | ---------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `GET`        | `v1/ping`                                                                                                  | Capability probe. Returns `X-Conan-Server-Capabilities: revisions`                         |
| `GET`        | `v2/users/authenticate`, `v2/users/check_credentials`                                                      | Login and credential check                                                                 |
| `GET`        | `v2/conans/search?q=<pattern>`                                                                             | Recipe search (local and cached content only)                                              |
| `GET`        | `v2/conans/<ref>/latest`, `.../revisions`, `.../revisions/<rrev>/files`                                    | Recipe revisions and file lists                                                            |
| `GET`        | `.../revisions/<rrev>/packages`, `.../search`, `.../packages/<pid>/latest`, `.../packages/<pid>/revisions` | Binary package metadata                                                                    |
| `GET`, `PUT` | `.../revisions/<rrev>/files/<file>`                                                                        | Recipe files: `conanfile.py`, `conan_export.tgz`, `conan_sources.tgz`, `conanmanifest.txt` |
| `GET`, `PUT` | `.../revisions/<rrev>/packages/<pid>/revisions/<prev>/files/<file>`                                        | Package files: `conaninfo.txt`, `conan_package.tgz`, `conanmanifest.txt`                   |
| `DELETE`     | `v2/conans/<ref>[/revisions/<rrev>[/packages/<pid>[/revisions/<prev>]]]`                                   | Remove a recipe, a revision or packages                                                    |

## Limits

- **Conan 2 only.** The adapter has no `v1/conans/...` routes, so Conan 1.x clients in the classic (non-revisions) mode cannot use it. Conan 1.x with revisions enabled has not been tested.
- Revision files arrive one at a time. They are staged and committed as a single atomic unit only once `conanmanifest.txt` arrives and its MD5 identity matches the revision in the URL. Each other file must match the manifest. Otherwise the stage is discarded and nothing becomes visible. A stage holds at most 8 files and about 130 MiB, and expires after one hour.
- A plain metadata file can be at most 1 MiB. A tarball can be at most 128 MiB expanded, with at most 4,096 manifest members.
- A revision is immutable. Re-uploading the same revision is idempotent. Different bytes can never seal under an existing revision.
- Listings never exceed 4,096 rows. Responses carry `Cache-Control: private, no-store`.
- Uploads beyond the per-format concurrency limit (`ANT_UPLOAD_SLOTS`) are answered with 503.

`v2/users/authenticate` hands Conan the token it keeps for later requests. A service token (`ant_...`) is echoed back unchanged. A personal access token is returned as itself, because it is already a reusable credential. A directory (LDAP or Active Directory) username and password is exchanged for a personal access token owned by that user, valid for 12 hours and named `conan remote login`; logging in again replaces the previous one, so tokens do not accumulate. A GitHub OIDC JWT is exchanged for the short-lived service token described below.

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `conan`. On **Sources**, use **Create source** to create a `local` source, then attach it to the feed with **Attach source** and make it the **Publish policy** default. To proxy ConanCenter, add a `remote` source and set its origin on the **Upstream** tab. You can attach it to the same feed, after the local source.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: conan-local-1' \
  -d '{"name":"conan-local","kind":"local","format":"conan"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: conan-remote-1' \
  -d '{"name":"conancenter","kind":"remote","format":"conan"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: conan-remote-cfg-1' -d '{
  "origin":"https://center2.conan.io","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: conan-feed-1' -d @- <<JSON
{"slug":"conan","format":"conan","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The local source comes first, so a reference that you publish locally is never completed from ConanCenter. See [Read order and resolve explain](../concepts/resolution.md).

## Remote upstreams

The origin is the Conan server root. The v2 API carries no absolute URLs, so feed paths map one to one onto upstream paths.

- `latest`, `revisions`, package-id `search` and `files` listings are mutable. They go through the metadata cache and are validated and re-rendered.
- Revision files are immutable and cached on first download. Conan publishes MD5, not SHA-256, so Ant checks `conanmanifest.txt` against the revision in the URL (the revision is the MD5 of the sorted manifest lines). Every other file is checked against the manifest: a plain file by its MD5, a tarball by the MD5 of each member. Bytes are staged and checked before they are served or cached.
- `v2/conans/search` is answered locally, so `conan search` and `conan list "*" -r` show only locally published and already cached recipes. Upstream recipes that nobody has fetched yet do not appear.
- During an upstream outage, metadata for cached references is served from the store with `X-Kuvryn-Cache: offline`. A response from a stale cache entry carries `X-Kuvryn-Cache: stale`.

| Upstream                                                           | `origin`                                                                                  | `redirect_origins` |
| ------------------------------------------------------------------ | ----------------------------------------------------------------------------------------- | ------------------ |
| ConanCenter (Conan 2)                                              | `https://center2.conan.io`                                                                | none               |
| Another Conan 2 server (Artifactory, a self-hosted `conan_server`) | its remote URL, for example `https://conan.example.com/artifactory/api/conan/conan-local` | none               |

A private upstream that needs credentials: `PUT /api/v1/sources/{id}/remote-credential` with `{"authorization":"Bearer <upstream-token>"}` (stored encrypted, sent only to the origin).

::: tip
The conformance run (Conan 2.33.0) downloads `zlib/1.3.1` through a feed whose remote origin is `https://center2.conan.io`, and downloads it again from a fresh client cache while the upstream is unreachable.
:::

## Client configuration

The adapter accepts `Authorization: Bearer <token>` or HTTP Basic with the token as password. An unauthenticated request gets `401` with `WWW-Authenticate: Basic realm="Conan"`. Conan answers that by calling `v2/users/authenticate` with your login.

### Consume and publish

::: code-group

```sh [conan remote login]
conan remote add ant https://ant.example.com/feeds/conan/conan
conan remote login ant ci -p "$ANT_TOKEN"      # any username; password = service token
conan remote disable conancenter               # optional: resolve everything through Ant

conan install --requires=zlib/1.3.1 -r ant
conan create .
conan upload "mylib/1.0" -r ant -c
conan list "mylib/*" -r ant
conan remove "mylib/1.0" -r ant -c
```

```sh [environment variables (CI)]
conan remote add ant https://ant.example.com/feeds/conan/conan --force
export CONAN_LOGIN_USERNAME_ANT=ci
export CONAN_PASSWORD_ANT="$ANT_TOKEN"
conan upload "mylib/1.0" -r ant -c
```

:::

Uploads need publish permission on the feed. `conan remove -r` needs delete permission and is subject to the feed's delete policy.

If your instance uses a private CA, point Conan at it with `core.net.http:cacert_path=/path/to/ca.pem` in `global.conf`.

### GitHub Actions

Exchange the OIDC token for a short-lived Ant service token (see [GitHub Actions](../ci/github-actions.md)). The exchanged token is a service token, so it works with `conan remote login`:

```yaml
- run: |
    conan remote add ant https://ant.example.com/feeds/conan/conan --force
    conan remote login ant ci -p "$ANT_TOKEN"
    conan upload "mylib/*" -r ant -c
```

## Troubleshooting

| Symptom                                          | Cause                                                                                                                                                  |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `ERROR: Wrong user or password` on login         | The token is invalid, revoked or expired, or the feed's client-auth policy does not accept this credential type.                                       |
| Login works but the next command gets `401`      | The credential Conan stored has expired (the directory-login token lasts 12 hours) or was revoked. Run `conan remote login` again.                     |
| `403`                                            | The token lacks read or publish on this feed, or the feed's client-auth policy rejects the credential type.                                            |
| `400` on upload                                  | The reference, revision or package ID is malformed, or the file name is not one of the allowed Conan files.                                            |
| `400` on the `conanmanifest.txt` upload          | The manifest did not match the revision in the URL or the staged files. The stage was discarded and nothing became visible. Upload the revision again. |
| `409`                                            | Conflicting content for an existing revision.                                                                                                          |
| `503` on a proxied reference                     | The upstream errored or is unreachable and nothing is cached. If headers show `X-Kuvryn-Cache: offline`, the answer came from cache.                   |
| `conan search -r ant` misses ConanCenter recipes | Expected: search is local. Fetch a reference once (`conan download <ref> -r ant --only-recipe`) to cache it.                                           |

Adapter internals: [internal/conan/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/conan/README.md).
