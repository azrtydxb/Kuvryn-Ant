# Hugging Face

The Hugging Face adapter serves the subset of the Hugging Face Hub API that `huggingface_hub`, the `hf` / `huggingface-cli` command and libraries built on them (`transformers`, `datasets`, `diffusers`, ...) use to download model and dataset files. A feed can hold repositories you publish through an Ant-specific upload API. It can also proxy and cache `huggingface.co`, including gated and private repositories reached with an upstream token.

Endpoint (`HF_ENDPOINT`): `https://ant.example.com/feeds/<slug>/huggingface` (feed format `huggingface`).

## Support matrix

| Publish                                                  | Consume                          | Remote (pull-through)                           | Replica                       | Notes                                            |
| -------------------------------------------------------- | -------------------------------- | ----------------------------------------------- | ----------------------------- | ------------------------------------------------ |
| Yes, through the Ant upload/commit API (not `hf upload`) | Yes, `huggingface_hub` downloads | Yes, `huggingface.co` with CDN redirect origins | Yes, through core replication | Models and datasets. No Spaces, no Xet protocol. |

Served routes (all under `/feeds/<slug>/huggingface/`; `<type>` is `models` or `datasets`):

| Method        | Path                                            | Purpose                                     |
| ------------- | ----------------------------------------------- | ------------------------------------------- |
| `GET`, `HEAD` | `api/<type>/<repo>`                             | Repository info at `main`                   |
| `GET`, `HEAD` | `api/<type>/<repo>/revision/<rev>`              | Repository info at a branch, tag or commit  |
| `GET`, `HEAD` | `api/<type>/<repo>/tree/<rev>[/<path>]`         | File listing                                |
| `GET`         | `api/whoami-v2`                                 | Identity of the calling token               |
| `GET`, `HEAD` | `<repo>/resolve/<rev>/<path>`                   | Model file download                         |
| `GET`, `HEAD` | `datasets/<repo>/resolve/<rev>/<path>`          | Dataset file download                       |
| `PUT`         | `api/<type>/<repo>/ant-upload/<session>/<path>` | Stage one file for a commit                 |
| `POST`        | `api/<type>/<repo>/ant-commit/<session>`        | Commit the staged files, with optional refs |
| `DELETE`      | `api/<type>/<repo>/ant-ref/<name>`              | Tombstone a branch or tag                   |

A `resolve` response carries `ETag` (the sha256), `X-Repo-Commit`, `X-Linked-Etag` and `X-Linked-Size`. It answers single-range requests and `If-Range`, which `huggingface_hub` uses to resume downloads.

## Limits

- A repository id has one or two segments (`bert-base-uncased`, `myorg/mymodel`). Revision names cannot contain `/`, so `refs/pr/1` style revisions are not supported.
- A single file can be at most 1 TiB. A commit can hold at most 10,000 files, and at most 64 upload sessions can be open per process. A staged session expires after one hour.
- Upload sessions are staged on the local disk of one Ant process. All `ant-upload` requests of a session and its `ant-commit` must reach the same process.
- The Hub write APIs (`hf upload`, `upload_file`, `push_to_hub`, `create_repo`) are not implemented. Spaces, search and listing endpoints (`api/models?search=...`), the Xet transfer protocol and commit deletion are not available.
- Responses carry `Cache-Control: private, no-store`.

::: warning Gap
The real-client conformance run on 2026-10-19 (`huggingface_hub` 2.2.0) passed `hf download` of single files, and the remote download of a file from `huggingface.co`. In the same run, `snapshot_download` of a locally published repository failed. A later commit changed the tree entries, but it has not been re-verified with the real client. `transformers` `from_pretrained` has not been run against the adapter.
:::

## Create the feed

In the console, open **Feeds**, choose **New feed** and pick format `huggingface`. Attach a local source for your own models and make it the publish target. Attach a remote source for `huggingface.co`, with its origin and redirect origins set on the **Upstream** tab.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: hf-local-1' \
  -d '{"name":"hf-local","kind":"local","format":"huggingface"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: hf-remote-1' \
  -d '{"name":"huggingface-hub","kind":"remote","format":"huggingface"}' | jq -r .id)
# configure $REM as shown under "Remote upstreams", then:
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: hf-feed-1' -d @- <<JSON
{"slug":"hf","format":"huggingface","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

With the local source first in read order, a repository you published locally is never answered from upstream. See [Read order and resolve explain](../concepts/resolution.md).

## Remote upstreams

The origin is the hub endpoint, `https://huggingface.co`. Hub `resolve` URLs for large (LFS) files answer with a redirect to a CDN host. Ant follows a redirect only to a host listed in the source's `redirect_origins`, so those CDN hosts must be listed. Otherwise the download fails as a policy error.

| Setting               | Value                                                                                                                      |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `origin`              | `https://huggingface.co`                                                                                                   |
| `redirect_origins`    | `https://cdn-lfs.huggingface.co`, `https://cdn-lfs-us-1.hf.co`, `https://cdn-lfs.hf.co`, `https://cas-bridge.xethub.hf.co` |
| credential (optional) | `{"authorization":"Bearer hf_..."}` for gated or private repositories                                                      |

```sh
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: hf-remote-cfg-1' -d '{
  "origin":"https://huggingface.co",
  "redirect_origins":["https://cdn-lfs.huggingface.co","https://cdn-lfs-us-1.hf.co",
                      "https://cdn-lfs.hf.co","https://cas-bridge.xethub.hf.co"],
  "allow_private":false,"timeout_seconds":300,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":86400,
           "max_entries":10000,"max_bytes":268435456}}'

# gated or private repositories: an upstream token with access to them
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-credential -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: hf-remote-cred-1' -d '{"authorization":"Bearer <hf-upstream-token>"}'
```

How the proxy behaves:

- **Credentials.** The source credential is attached to hub requests (`huggingface.co`) only, never to a CDN host. The client's own `HF_TOKEN`, which is an Ant token, is never forwarded upstream. To reach a gated model, accept its terms on huggingface.co with the account that owns the upstream token, then store that token as the source credential. Every client of the feed then reads the model with that account's access. Use feed permissions to decide who may read the feed.
- **Caching.** Files are cached per upstream commit under `models/<repo>` or `datasets/<repo>`. A branch or tag is resolved through the metadata cache and remembered as a ref, so cached files stay reachable by branch name during an upstream outage.
- **Verification.** The sha256 and size come from the upstream tree entry (the LFS oid). A miss streams to the client while it is staged, and the digest is checked at the end. A mismatch aborts the connection and caches nothing. Small non-LFS files carry only a git sha1 upstream, so they are checked by length only.
- **HEAD and Range.** `HEAD` is answered from the tree entry without downloading. A Range request on a miss starts a verified fill that keeps running even if the client goes away, and is then answered from the store, so a resumed download hits the cache.
- **Listings.** Tree pages beyond the first are not followed. A recursive tree is a bounded directory walk (64 directories, 10,000 entries).

::: warning Gap
Upstream 401 and 403 responses (gated repository without access, or a missing or invalid source credential) reach the client as 503, not 401 or 403. Check the source credential and the upstream account's access when a gated model returns 503.
:::

## Client configuration

`huggingface_hub` reads the endpoint and token from the environment. `HF_TOKEN` is an Ant token (service token or personal access token), sent as `Authorization: Bearer`. It is not a huggingface.co token. Reading needs the `read` permission on the feed.

::: code-group

```sh [hf CLI]
export HF_ENDPOINT=https://ant.example.com/feeds/hf/huggingface
export HF_TOKEN=$ANT_TOKEN
hf download prajjwal1/bert-tiny config.json --local-dir ./bert-tiny
# older releases name the command huggingface-cli:
huggingface-cli download prajjwal1/bert-tiny config.json --local-dir ./bert-tiny
```

```python [huggingface_hub]
import os
os.environ["HF_ENDPOINT"] = "https://ant.example.com/feeds/hf/huggingface"
os.environ["HF_TOKEN"] = os.environ["ANT_TOKEN"]

from huggingface_hub import hf_hub_download
path = hf_hub_download("prajjwal1/bert-tiny", "config.json")
```

```python [transformers]
# set HF_ENDPOINT and HF_TOKEN in the environment before Python starts
from transformers import AutoModel, AutoTokenizer
tok = AutoTokenizer.from_pretrained("prajjwal1/bert-tiny")
model = AutoModel.from_pretrained("prajjwal1/bert-tiny")
```

:::

Set `HF_ENDPOINT` before `huggingface_hub` is imported, because the library reads it at import time. Datasets use the same endpoint: `hf download myorg/mydata --repo-type dataset`.

### Publish

Publishing uses Ant's own two-step API, which needs the `publish` permission. Stage each file in a session, then commit the session. The commit id is the sha1 of the sorted file tree. Files are published first and a manifest last, so a revision becomes visible atomically.

```sh
B=https://ant.example.com/feeds/hf/huggingface/api/models/myorg/mymodel
S=upload-$(date +%s)   # session id: 8-64 characters, [A-Za-z0-9_-]
for f in config.json model.safetensors README.md; do
  curl -fsS -u token:$ANT_TOKEN -X PUT \
    -H "X-Checksum-Sha256: $(sha256sum "$f" | cut -d' ' -f1)" \
    --data-binary @"$f" "$B/ant-upload/$S/$f"
done
curl -fsS -u token:$ANT_TOKEN -X POST -H 'Content-Type: application/json' \
  -d '{"refs":["main"]}' "$B/ant-commit/$S"
```

A commit can move at most 16 refs. Refs are append-only versions, and the newest one wins. `DELETE .../ant-ref/<name>` tombstones a ref and needs the `delete` permission. Commits themselves are immutable.

### GitHub Actions

Get a short-lived Ant token with the OIDC exchange in [GitHub Actions](../ci/github-actions.md) (trust rules: [GitHub OIDC trust](../identity/github-oidc.md)), and export it as `HF_TOKEN` along with `HF_ENDPOINT`.

## Troubleshooting

| Symptom                                                         | Cause                                                                                                                           |
| --------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `401`                                                           | `HF_TOKEN` is missing or is a huggingface.co token instead of an Ant token.                                                     |
| `403`                                                           | The token lacks read (or publish) permission on the feed, or the client auth policy refuses the credential type.                |
| `404`                                                           | The repository, revision or file is in neither the local source nor upstream, or the revision name contains `/`.                |
| `404` or `405` from `hf upload`, `create_repo` or `upload_file` | The Hub write APIs are not implemented. Use the Ant upload/commit API.                                                          |
| `429` on `ant-upload`                                           | All upload slots of the process are busy. Retry.                                                                                |
| `503` on a remote file                                          | An upstream error, a missing redirect origin for the CDN host, or a gated or private repository the upstream token cannot read. |
| Connection reset during a remote download                       | The sha256 at the end of the stream did not match the upstream tree entry. Nothing was cached. Retry.                           |
| `X-Kuvryn-Cache: stale` and `Warning: 110`                      | Repository info came from the metadata cache after its TTL because upstream was unreachable.                                    |

Adapter internals: [internal/huggingface/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/huggingface/README.md).
