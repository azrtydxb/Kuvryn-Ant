# Git LFS

The Git LFS adapter is an LFS server: it implements the Git LFS batch API and basic transfers. Your Git repository stays wherever it is hosted. Git LFS stores and fetches the large objects through an Ant feed instead of the Git host. A feed can also front an upstream LFS server and cache the objects it serves.

Endpoint (`lfs.url`): `https://ant.example.com/feeds/<slug>/git-lfs` (feed format `git-lfs`). The batch API is at `<lfs.url>/objects/batch`.

## Support matrix

| Publish                                    | Consume                                    | Remote (pull-through)                                  | Replica                       | Notes                                 |
| ------------------------------------------ | ------------------------------------------ | ------------------------------------------------------ | ----------------------------- | ------------------------------------- |
| Yes, `git lfs push` (upload via batch API) | Yes, `git lfs pull` / `fetch` / `checkout` | Yes, against an upstream LFS batch API (download only) | Yes, through core replication | Basic transfer only. No file locking. |

Served routes (all under `/feeds/<slug>/git-lfs/`):

| Method        | Path                     | Purpose                                                                                    |
| ------------- | ------------------------ | ------------------------------------------------------------------------------------------ |
| `POST`        | `objects/batch`          | Batch API, operations `upload` and `download`, hash algorithm `sha256`                     |
| `PUT`         | `objects/<oid>/upload`   | Upload action, granted by a batch response                                                 |
| `POST`        | `objects/<oid>/verify`   | Verify action. Needs the exact declared oid and size                                       |
| `GET`, `HEAD` | `objects/<oid>/download` | Download action. Supports single, open-ended and suffix ranges, and strong-ETag `If-Range` |
| `DELETE`      | `objects/<oid>`          | Ant-specific delete, gated by the core delete policy. The batch API has no delete.         |

Every transfer needs two things: a valid Ant credential, and the random `X-LFS-Action` grant that the batch response returned for that object and operation. A grant is bound to the token, principal, feed, oid, size, operation and source, and it expires after five minutes. The batch response carries the client's own `Authorization` header back in each action's `header` map, and action URLs are built from `ANT_PUBLIC_URL`, never from request headers.

## Limits

- A batch request can be at most 1 MiB of JSON and list at most 100 objects (more answers 413). A verify body can be at most 4 KiB. The declared object size can be at most 1 TiB.
- Concurrent transfers per process are bounded by `ANT_UPLOAD_SLOTS` (default: twice the CPU count, between 4 and 32). A transfer beyond that answers 429. Every read and write must make progress within 30 seconds.
- Only the `basic` transfer adapter and the `sha256` hash algorithm are accepted. Anything else answers 422 (transfer) or 409 (hash algorithm).
- LFS file locking (`/locks`), custom transfer adapters, resumable uploads, multipart ranges and per-ref access control are not implemented.
- Grants are held in process memory (at most 4,096) and are not persisted. A restart, or a transfer that lands on a different Ant process than its batch request, needs a fresh batch response.
- Each oid has one owner per feed. Download actions pin that owner and the size, so a later lookup never falls through to a lower source.

## Create the feed

In the console, open **Feeds**, choose **New feed** and pick format `git-lfs`. Attach a local source and make it the publish target. For a proxy, attach a remote source whose origin is the upstream `lfs.url`.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: lfs-local-1' \
  -d '{"name":"lfs-local","kind":"local","format":"git-lfs"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: lfs-feed-1' -d @- <<JSON
{"slug":"lfs","format":"git-lfs","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md).

## Remote upstreams

The origin of a remote Git LFS source is the upstream LFS endpoint, the value a client would use as `lfs.url`, for example `https://lfs.example.com/myorg/myrepo.git/info/lfs`. The upstream batch API is `<origin>/objects/batch`. The oid is the SHA-256 of the content, so every byte is checked against the id the client asked for.

- **Batch (download).** When an object is not stored locally, the adapter asks the upstream batch API (`POST objects/batch`, operation `download`, basic transfer, sha256). If upstream has the object at the declared size, the client receives Ant's own download action. An upstream "not found" becomes a per-object 404, a different size a per-object 422, and an upstream failure a 503.
- **The one non-GET request upstream.** This batch query is the only request other than GET that Ant ever sends to an upstream. It is a bounded JSON `POST` to the source origin, with operation `download` only. Uploads, verify and every other operation never go upstream.
- **Download.** On first use, the adapter asks the upstream batch API again for a fresh action, because actions expire. It fetches the object from the action `href` and caches it under the remote source. A plain `GET` streams to the client while the object is staged, and the digest is checked at the end. A mismatch aborts the transfer and caches nothing. A Range request on a miss fills the cache completely first and is then answered from the store, so resumed transfers hit the cache. `HEAD` on a miss is answered from the grant without fetching.
- **Ownership.** An oid held by a local source is never answered from upstream.

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: lfs-upstream-1' \
  -d '{"name":"lfs-upstream","kind":"remote","format":"git-lfs"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: lfs-upstream-cfg-1' -d '{
  "origin":"https://lfs.example.com/myorg/myrepo.git/info/lfs",
  "redirect_origins":["https://objects.example.com"],"allow_private":false,"timeout_seconds":120,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
# credential for the upstream LFS server (sent only to the origin)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-credential -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: lfs-upstream-cred-1' -d '{"authorization":"Basic <base64 user:token>"}'
```

Put every host that the upstream's action `href` values point at, other than the origin itself, in `redirect_origins`. Such hosts must use https, and they never receive the credential.

::: warning Gap

- Upstream action `header` maps are not forwarded. An upstream that needs per-action headers, or an `Authorization` header on a host other than the origin, cannot be proxied. Many hosted LFS services sign their download URLs, which works; services that require an action header do not.
- The batch response is limited to the metadata size limit.
- Locks, custom transfer adapters and refs are not proxied.
- Remote pull-through is covered by integration tests only. It has not been run against a real hosted LFS server.

:::

## Client configuration

### Point Git LFS at the feed

Commit an `.lfsconfig` at the repository root so every clone uses the feed:

```ini
# .lfsconfig
[lfs]
	url = https://ant.example.com/feeds/lfs/git-lfs
	locksverify = false
```

Setting `locksverify = false` stops `git lfs push` from calling the locking API, which the adapter does not implement. The same settings work per clone:

```sh
git config lfs.url https://ant.example.com/feeds/lfs/git-lfs
git config lfs.locksverify false
```

### Credentials

Git LFS asks Git's credential system for the LFS host. Use any username and an Ant token (service token or personal access token) as the password. Reading needs `read`. Pushing needs `publish` plus `read`.

::: code-group

```sh [credential helper]
git config --global credential.helper store
printf 'protocol=https\nhost=ant.example.com\nusername=token\npassword=%s\n\n' "$ANT_TOKEN" \
  | git credential approve
```

```sh [token in lfs.url (CI only)]
git config lfs.url "https://token:$ANT_TOKEN@ant.example.com/feeds/lfs/git-lfs"
```

:::

The server answers unauthenticated requests with `LFS-Authenticate: Basic realm="Git LFS"`, which tells Git LFS to fetch credentials.

### Use it

```sh
git lfs install
git lfs track "*.bin"
git add .gitattributes big.bin
git commit -m "add big.bin"
git push origin main        # objects go to Ant, commits go to the Git host
git clone https://git.example.com/myorg/myrepo.git && cd myrepo && git lfs pull
```

The conformance harness runs the real `git-lfs` client through push, a fresh clone with `git lfs pull`, and byte-for-byte comparison.

### GitHub Actions

Get a short-lived token with the OIDC exchange described in [GitHub Actions](../ci/github-actions.md) (trust rules: [GitHub OIDC trust](../identity/github-oidc.md)). Then register it with the credential helper before `git lfs pull`. `actions/checkout` with `lfs: true` fetches LFS objects with the checkout token for the Git host, so run `git lfs pull` as a separate step after you configure the Ant credential.

## Troubleshooting

| Symptom                                                         | Cause                                                                                                                                                           |
| --------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` on `objects/batch`                                        | No credentials or wrong credentials for `ant.example.com`.                                                                                                      |
| `403`                                                           | The token lacks `read` (download) or `publish` (upload), or the client auth policy refuses the credential type.                                                 |
| `404` per object                                                | The object is in neither the feed nor the upstream.                                                                                                             |
| `409` on batch                                                  | `hash_algo` is not `sha256`.                                                                                                                                    |
| `413` on batch                                                  | More than 100 objects in one request. Lower `lfs.transfer.batchSize`.                                                                                           |
| `422` per object                                                | The size the client declared differs from the stored or upstream size. A 422 for the whole request means a transfer other than `basic` or an unknown operation. |
| Upload or download fails after a while, or after an Ant restart | The five-minute grant expired, or the transfer reached another Ant process. Re-run the command to get a fresh batch response.                                   |
| Warnings about locking                                          | Set `lfs.locksverify false`.                                                                                                                                    |
| `429` on a transfer                                             | All transfer slots of that process are busy (`ANT_UPLOAD_SLOTS`). Git LFS retries.                                                                              |
| `503`                                                           | Upstream failure on a remote feed, or `ANT_PUBLIC_URL` not set.                                                                                                 |

Adapter internals: [internal/gitlfs/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/gitlfs/README.md).
