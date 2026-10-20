# Helm (classic and OCI)

Kuvryn Ant serves Helm charts in two ways:

- **Classic chart repository** (`helm` feed format): an `index.yaml` plus chart
  archives, for `helm repo add` / `helm install repo/chart`. Served by the Helm
  adapter.
- **Charts in OCI** (`oci` feed format): `helm push` / `helm pull oci://...`, served by
  the [OCI adapter](./oci.md) like any other OCI artifact.

Classic endpoint:

```text
https://ant.example.com/feeds/<slug>/helm/
https://ant.example.com/feeds/<slug>/helm/index.yaml
```

Implementation: [`internal/helmrepo`](https://github.com/azrtydxb/Kuvryn-Ant/tree/main/internal/helmrepo)
(classic) and [`internal/oci`](https://github.com/azrtydxb/Kuvryn-Ant/tree/main/internal/oci) (OCI).

## Support matrix

| Operation                           | Supported        | Notes                                                                                                                                 |
| ----------------------------------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| Consume classic repository          | Yes              | `helm repo add`, `helm repo update`, `helm search repo`, `helm pull`, `helm install`, dependency update.                              |
| Publish to classic repository       | Yes, adapter API | `POST charts/<name>/<version>`, multipart. Requires the feed **admin** role (see below).                                              |
| `helm cm-push` (ChartMuseum plugin) | No               | The plugin's `POST /api/charts` route does not exist; use the `curl` call below.                                                      |
| Provenance files (`.prov`)          | Stored           | Uploaded with the chart and served at `<chart URL>.prov`. Ant checks the declared chart digest but does **not** verify the signature. |
| Delete                              | Yes              | `DELETE charts/<name>/<version>`, gated by the feed's delete policy.                                                                  |
| `helm push` / `helm pull oci://`    | Yes              | On an `oci` feed.                                                                                                                     |
| Remote (pull-through)               | Yes              | Classic: upstream `index.yaml` merged, charts cached on first download. OCI: through an `oci` remote source.                          |
| Replica                             | Yes              | Through the format-agnostic replication core.                                                                                         |

Real-client conformance (2026-10-19, Helm v4.3.1): package, publish through the
adapter API, `repo add`/`update`/`search`/`pull`/`template`, delete, `helm push` and
`helm pull` over `oci://`, and pull-through of a public chart repository (miss, then
hit with the upstream unreachable).

## Limits

- **Classic publishing needs the feed admin role** in addition to the publish
  permission: the upload is treated as administrative ingestion. A token that can
  only publish gets `403`.
- Chart archive checks: the file name must be `<name>-<version>.tgz` (provenance
  `<name>-<version>.tgz.prov`), the `Chart.yaml` name and version must match the URL,
  and the archive may not contain unsafe paths, duplicate members, links or special
  files, truncated gzip data or trailing archives. Chart versions must be SemVer.
- `Chart.yaml` metadata at most 1 MiB, provenance at most 2 MiB, at most 10,000
  archive members.
- Index: at most 10,000 chart versions and 16 MiB of stored chart metadata per feed.
- At most four concurrent uploads per process; more return `503`.
- Charts are immutable: uploading different bytes for an existing name and version
  returns `409`; an identical retry succeeds.
- Remote: the first remote source that has an `index.yaml` serves it; indexes from
  several remote sources are **not** merged with each other. A chart whose package
  owner is a local source is dropped from the upstream part of the index.
- Remote metadata documents are bounded by the server's metadata limit (24 MiB per
  document in the shipped build). Very large public indexes may not fit.

## Create the feed

### Console

1. **Sources**: **Create source**, kind `local`, format `helm`.
2. **Feeds**: **Create feed** with slug (for example `charts`) and format `helm`.
   Attach the source and make it the default target of the publish policy.
3. For pull-through, create a `remote` source of format `helm`, set its origin and
   redirect origins on the **Upstream** tab, and attach it (after the local source if
   both are in one feed).

For charts in OCI, create an `oci` feed instead; see [OCI / Docker](./oci.md#create-the-feed).

### API

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: helm-local-1' \
  -d '{"name":"helm-local","kind":"local","format":"helm"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: helm-remote-1' \
  -d '{"name":"podinfo","kind":"remote","format":"helm"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: helm-remote-cfg-1' -d '{
  "origin":"https://stefanprodan.github.io/podinfo",
  "redirect_origins":["https://github.com","https://objects.githubusercontent.com","https://release-assets.githubusercontent.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: helm-feed-1' -d @- <<JSON
{"slug":"charts","format":"helm","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

## Remote upstreams

The origin is the chart repository root, the URL you would pass to `helm repo add`;
Ant fetches `index.yaml` relative to it.

- The upstream `index.yaml` goes through the metadata cache. Each entry is validated
  (chart name, SemVer version, `urls`, optional sha256 `digest`) and merged into the
  feed index with every `urls` entry rewritten to a feed route:
  `charts/<name>/<version>/<source>/<digest>/<file>` when upstream gives a digest,
  `charts/<name>/<version>/<file>` otherwise.
- Chart archives are verified against the index digest and cached on first download;
  `.prov` files are cached on demand. Relative chart URLs resolve against the origin.
- Chart URLs on another host (GitHub release assets, a CDN) need that host in
  `redirect_origins`, including every host the download redirects through. The source
  credential is never sent there.

To find the hosts a repository needs, list the hosts in its index:

```sh
curl -fsSL https://stefanprodan.github.io/podinfo/index.yaml \
  | grep -Eo 'https://[^/" ]+' | sort -u
```

| Repository           | `origin`                                             | `redirect_origins`                                                                                            | Status       |
| -------------------- | ---------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- | ------------ |
| podinfo              | `https://stefanprodan.github.io/podinfo`             | `https://github.com`, `https://objects.githubusercontent.com`, `https://release-assets.githubusercontent.com` | Verified     |
| prometheus-community | `https://prometheus-community.github.io/helm-charts` | same GitHub release hosts as podinfo                                                                          | Not verified |
| ingress-nginx        | `https://kubernetes.github.io/ingress-nginx`         | same GitHub release hosts as podinfo                                                                          | Not verified |

Repositories published as GitHub Pages with charts attached to GitHub releases all
follow the podinfo pattern. Repositories that have moved to OCI (for example charts
published under `oci://registry-1.docker.io/...`) are proxied through an `oci` feed
with a Docker Hub remote source instead; see [OCI / Docker](./oci.md#remote-upstreams).

::: warning Gap

Only the podinfo repository is covered by a real-client run. The other rows follow
the same layout but are not verified, and indexes larger than the server's metadata
limit cannot be proxied. Pulling Helm charts from OCI upstreams through an `oci`
remote source uses the generic OCI pull-through, which is verified for images and
ORAS artifacts but not specifically for Helm chart artifacts.

:::

## Client configuration

### Consume the classic repository

Use a service token or personal access token as the password; the username is
ignored for tokens.

```sh
helm repo add ant https://ant.example.com/feeds/charts/helm \
  --username x --password "$ANT_TOKEN" --pass-credentials
helm repo update ant
helm search repo ant/
helm install myrelease ant/mychart --version 0.1.0
```

Chart URLs in the feed index are relative, so they stay on the same host. Add
`--ca-file ca.pem` for a private CA. As a chart dependency:

```yaml
# Chart.yaml
dependencies:
  - name: mychart
    version: 0.1.0
    repository: https://ant.example.com/feeds/charts/helm
```

Register the repository with `helm repo add` first so `helm dependency update` has
the credentials.

### Publish to the classic repository

Package the chart and upload it with multipart field `chart` (and optionally
`provenance`). The token's principal needs the feed **admin** role.

```sh
helm package ./mychart --version 0.1.0       # writes mychart-0.1.0.tgz
curl -fsS -u "x:$ANT_TOKEN" \
  -F "chart=@mychart-0.1.0.tgz" \
  https://ant.example.com/feeds/charts/helm/charts/mychart/0.1.0

# with a provenance file (helm package --sign ...)
curl -fsS -u "x:$ANT_TOKEN" \
  -F "chart=@mychart-0.1.0.tgz" -F "provenance=@mychart-0.1.0.tgz.prov" \
  https://ant.example.com/feeds/charts/helm/charts/mychart/0.1.0
```

A successful upload returns `201` with
`{"name":...,"version":...,"digest":...,"provenanceStored":...,"signatureVerified":false}`.
A Bearer header (`-H "Authorization: Bearer $ANT_TOKEN"`) works too.

Delete a version:

```sh
curl -fsS -X DELETE -u "x:$ANT_TOKEN" \
  https://ant.example.com/feeds/charts/helm/charts/mychart/0.1.0
```

### Helm charts in OCI

Charts in OCI live in an `oci` feed (for example `oci-hosted`). `helm push` takes the
registry and feed; the chart name becomes the repository.

```sh
echo "$ANT_TOKEN" | helm registry login ant.example.com -u x --password-stdin
helm package ./mychart --version 0.2.0
helm push mychart-0.2.0.tgz oci://ant.example.com/oci-hosted
helm pull oci://ant.example.com/oci-hosted/mychart --version 0.2.0
helm install myrelease oci://ant.example.com/oci-hosted/mychart --version 0.2.0
```

OCI tags are immutable on ordinary feeds, so pushing a changed chart under an existing
version returns `409`. Publishing to an OCI feed needs the publish permission only.

### GitHub Actions

```yaml
- uses: azure/setup-helm@v4
- name: Publish chart
  env:
    ANT_TOKEN: ${{ secrets.ANT_TOKEN }}
  run: |
    helm package ./charts/mychart
    echo "$ANT_TOKEN" | helm registry login ant.example.com -u x --password-stdin
    helm push mychart-*.tgz oci://ant.example.com/oci-hosted
```

To avoid a stored secret, exchange the workflow OIDC token for a short-lived Ant token
first; see [GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md). For classic publishing the trust
rule's principal needs the feed admin role.

## Troubleshooting

| Symptom                                    | Cause                                                                                                                                                                                       |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` from `helm repo add`                 | Missing or invalid credential.                                                                                                                                                              |
| `403` on upload                            | The principal has publish but not the feed admin role (classic upload), or lacks publish.                                                                                                   |
| `400` on upload                            | Wrong multipart field name (must be `chart` / `provenance`), file name not `<name>-<version>.tgz`, `Chart.yaml` name or version differs from the URL, or the archive failed a safety check. |
| `409` on upload                            | That name and version already exist with different bytes.                                                                                                                                   |
| `404` from `helm cm-push`                  | The ChartMuseum API is not implemented; use the `curl` upload.                                                                                                                              |
| `503` on upload                            | All upload slots are busy; retry.                                                                                                                                                           |
| `503` on `index.yaml` with a remote source | Upstream error with nothing cached locally, or a chart download redirected to a host not listed in `redirect_origins`. Check `GET /api/v1/sources/{id}/health`.                             |
| `X-Kuvryn-Cache: offline` on `index.yaml`  | The upstream was unreachable; the locally composed index is served.                                                                                                                         |
| `X-Kuvryn-Cache: stale` on `index.yaml`    | The upstream index was served from the metadata cache after its TTL because the upstream failed.                                                                                            |
