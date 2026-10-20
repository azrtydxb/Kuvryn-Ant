# Ansible Galaxy

The Ansible adapter implements the Galaxy v3 API for **collections**. `ansible-galaxy collection install` and `ansible-galaxy collection publish` work against it. A feed can hold collections you publish, and it can proxy and cache `galaxy.ansible.com`.

Server URL: `https://ant.example.com/feeds/<slug>/ansible/` (feed format `ansible`; keep the trailing slash).

## Support matrix

| Publish                                                                    | Consume                                                                               | Remote (pull-through)                                                | Replica                       | Notes                                                |
| -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------- | -------------------------------------------------------------------- | ----------------------------- | ---------------------------------------------------- |
| Yes: `ansible-galaxy collection publish` (`POST v3/artifacts/collections`) | Yes: `ansible-galaxy collection install` (ansible-core 2.21.5 in the conformance run) | Yes: Galaxy v3 upstreams, archives checked against `artifact.sha256` | Yes, through core replication | Collections only. Roles (the v1 API) are not served. |

Routes under `/feeds/<slug>/ansible/`:

| Method        | Path                                                | Purpose                                                                                                 |
| ------------- | --------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `GET`, `HEAD` | `/` and `v3/`                                       | API discovery (`available_versions: {"v3": "v3/"}`)                                                     |
| `POST`        | `v3/artifacts/collections/`                         | Publish a built collection (multipart `file` + `sha256`)                                                |
| `GET`         | `v3/imports/<id>/`                                  | Import task state (always `completed` once accepted)                                                    |
| `GET`         | `v3/collections/<ns>/<name>/`                       | Collection detail                                                                                       |
| `GET`         | `v3/collections/<ns>/<name>/versions/`              | Version list                                                                                            |
| `GET`         | `v3/collections/<ns>/<name>/versions/<version>/`    | Version detail: `download_url`, `artifact.sha256`, dependencies                                         |
| `GET`, `HEAD` | `downloads/<ns>/<name>/<version>/collection.tar.gz` | Archive                                                                                                 |
| `DELETE`      | `v3/collections/<ns>/<name>/versions/<version>/`    | Delete a version (Galaxy NG style). The artifact core's delete policy controls whether this is allowed. |

The collection namespace is the authorization and routing namespace. A [publish policy](../concepts/publish-policies.md) rule can send different Galaxy namespaces to different local sources.

## Limits

- An archive can be at most 64 MiB and 256 MiB expanded, with at most 10,000 members, metadata of at most 2 MiB, and a directory depth of at most 32. `MANIFEST.json`, `FILES.json` and every tar member are cross-checked: file checksums must match `FILES.json`, and the `FILES.json` digest must match `MANIFEST.json`. Nothing runs.
- The archive filename must be `<namespace>-<name>-<version>.tar.gz` and must match the identity inside the archive. The `sha256` form field must match the upload. `ansible-galaxy` sends the file part base64 encoded, and the adapter decodes it before checking.
- Versions are immutable. An identical retry returns a completed import without new events.
- Import tasks are kept in memory in the replica that accepted the upload, for one hour, and can be polled only with the same token. A collection version lists at most 4,096 versions.
- Responses carry `Cache-Control: private, no-store`. Publishes beyond `ANT_UPLOAD_SLOTS` get 503.
- `ANT_PUBLIC_URL` must be set: every `href` and `download_url` is absolute. Without it the adapter answers 503.

::: warning Gap

- The recorded conformance run (2026-10-19) failed the native `ansible-galaxy collection publish` with 400, because the base64-encoded file part was not decoded. The adapter now decodes it and has an integration test for it, but no later real-client run is recorded. Publishing the same archive with a plain multipart `POST` passed.
- The same run failed the remote install from `galaxy.ansible.com` with 503, before the redirect origins below were added. It has not been re-run since.
- With several replicas, `ansible-galaxy` may poll `v3/imports/<id>/` on a replica that did not accept the upload and get 404, even though the collection was committed. This was found by reading the code and has not been tested.
  :::

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `ansible`. On **Sources**, use **Create source** to create a `local` source, attach it with **Attach source** and make it the **Publish policy** default. To proxy Galaxy, add a `remote` source with the origin and redirect origins below on its **Upstream** tab.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: ansible-local-1' \
  -d '{"name":"ansible-local","kind":"local","format":"ansible"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: ansible-remote-1' \
  -d '{"name":"galaxy","kind":"remote","format":"ansible"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: ansible-remote-cfg-1' -d '{
  "origin":"https://galaxy.ansible.com/api/",
  "redirect_origins":["https://galaxy.ansible.com",
                      "https://ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: ansible-feed-1' -d @- <<JSON
{"slug":"ansible","format":"ansible","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md).

## Remote upstreams

The origin is the **API root that serves `v3/`**. For galaxy.ansible.com that is `https://galaxy.ansible.com/api/`.

- Collection detail, the version list and version detail are re-rendered with feed `href` and `download_url` values. The server follows all pages of the version list, at most 50: only absolute `links.next` values, or relative ones once an absolute `href` reveals the origin. The rendered list keeps the upstream `data`/`meta`/`links` shape as a single page.
- Archives come from the upstream `download_url`, are checked against `artifact.sha256`, and are cached together with a metadata asset, so cached versions stay listable offline (`X-Kuvryn-Cache: offline`).
- A collection owned by a local source is never answered from upstream.
- galaxy.ansible.com serves `download_url` through a redirect to its S3 bucket, so both hosts must be redirect origins.

| Upstream                           | `origin`                                                                                         | `redirect_origins`                                                                             |
| ---------------------------------- | ------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------- |
| Ansible Galaxy                     | `https://galaxy.ansible.com/api/`                                                                | `https://galaxy.ansible.com`, `https://ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com` |
| Private Automation Hub / Galaxy NG | its API root serving `v3/` (for example `https://hub.example.com/api/galaxy/content/published/`) | the host serving its downloads, if different                                                   |

A private upstream that needs a token: `PUT /api/v1/sources/{id}/remote-credential` with `{"authorization":"Token <upstream-token>"}` or `{"authorization":"Bearer <upstream-token>"}`, whichever the upstream expects. The credential is stored encrypted and sent only to the origin.

::: warning Unverified
The Automation Hub row has not been tested. Its path layout differs between versions. Use the URL your hub shows as the server URL for `ansible-galaxy`, provided it answers `v3/collections/...` underneath.
:::

## Client configuration

`ansible-galaxy` sends `Authorization: Token <token>`, which the adapter accepts like `Bearer`. Unauthenticated requests get `401`.

::: warning Gap
A token sent with the `Token` scheme is checked only as a **service token** (`ant_...`). Personal access tokens, directory passwords and GitHub OIDC JWTs are recognised by Ant's client-auth layer only with the `Bearer` or `Basic` schemes, and `ansible-galaxy` uses neither. Use a service token, or the short-lived service token returned by the GitHub OIDC exchange. This was found by reading the code.
:::

### Consume and publish

::: code-group

```ini [ansible.cfg]
[galaxy]
server_list = ant

[galaxy_server.ant]
url = https://ant.example.com/feeds/ansible/ansible/
# token = <TOKEN>   ; or set ANSIBLE_GALAXY_SERVER_ANT_TOKEN
```

```sh [command line]
ansible-galaxy collection install myorg.mycoll:1.0.0 \
  --server https://ant.example.com/feeds/ansible/ansible/ --token "$ANT_TOKEN"
```

:::

```sh
export ANSIBLE_GALAXY_SERVER_ANT_TOKEN="$ANT_TOKEN"
ansible-galaxy collection install myorg.mycoll
ansible-galaxy collection install -r requirements.yml

ansible-galaxy collection build
ansible-galaxy collection publish myorg-mycoll-1.0.0.tar.gz
```

Publishing needs publish permission on the feed. With Galaxy attached as a remote source, public collections such as `ansible.posix` resolve through the same server entry.

If the native publish fails, upload the built archive directly:

```sh
F=myorg-mycoll-1.0.0.tar.gz
curl -fsS -X POST -H "Authorization: Bearer $ANT_TOKEN" \
  -F "sha256=$(sha256sum "$F" | cut -d' ' -f1)" \
  -F "file=@$F;type=application/octet-stream" \
  https://ant.example.com/feeds/ansible/ansible/v3/artifacts/collections/
```

To delete a version:

```sh
curl -fsS -X DELETE -H "Authorization: Bearer $ANT_TOKEN" \
  https://ant.example.com/feeds/ansible/ansible/v3/collections/myorg/mycoll/versions/1.0.1/
```

### GitHub Actions

Exchange the OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)), then:

```yaml
- run: ansible-galaxy collection publish myorg-mycoll-1.0.0.tar.gz
  env:
    ANSIBLE_GALAXY_SERVER_LIST: ant
    ANSIBLE_GALAXY_SERVER_ANT_URL: https://ant.example.com/feeds/ansible/ansible/
    ANSIBLE_GALAXY_SERVER_ANT_TOKEN: ${{ env.ANT_TOKEN }}
```

## Troubleshooting

| Symptom                                       | Cause                                                                                                                                                 |
| --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `HTTP Error 401`                              | No token, a token for another server entry, or a PAT or JWT sent with the `Token` scheme (see the gap above).                                         |
| `HTTP Error 403`                              | The token lacks read or publish on the feed or the collection namespace.                                                                              |
| `HTTP Code: 400` on publish                   | The filename does not match the archive identity, `sha256` does not match, or `MANIFEST.json`/`FILES.json` checks failed.                             |
| `404` while waiting for the import            | The task expired or is held by another replica. Check whether the version is listed.                                                                  |
| `409`                                         | That version exists with different bytes.                                                                                                             |
| `503` on install from a proxied feed          | A redirect to the S3 bucket was refused (missing redirect origin), the upstream is unreachable and nothing is cached, or `ANT_PUBLIC_URL` is not set. |
| Upstream collection missing from a mixed feed | A local source owns that collection.                                                                                                                  |

Adapter internals: [internal/ansiblerepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/ansiblerepo/README.md).
