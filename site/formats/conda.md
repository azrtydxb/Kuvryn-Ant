# Conda

The Conda adapter serves a conda channel: one `repodata.json` per platform subdirectory (`noarch`, `linux-64`, ...) plus the package files it lists. You can load packages you built yourself into a feed, and the feed can proxy and cache an upstream channel on `conda.anaconda.org`.

Channel URL: `https://ant.example.com/feeds/<slug>/conda` (feed format `conda`). conda, mamba and other clients add `/<subdir>/repodata.json` to this URL.

## Support matrix

| Publish                                                    | Consume                                                     | Remote (pull-through)                                                   | Replica                       | Notes                                                                          |
| ---------------------------------------------------------- | ----------------------------------------------------------- | ----------------------------------------------------------------------- | ----------------------------- | ------------------------------------------------------------------------------ |
| Admin ingestion only: `PUT ingest/<subdir>/<file>.tar.bz2` | Yes: `conda`, `mamba` (both covered by the conformance run) | Yes: `.tar.bz2` and `.conda` files, checked against the upstream sha256 | Yes, through core replication | conda has no native upload protocol. A local upload must be a `.tar.bz2` file. |

Routes under `/feeds/<slug>/conda/`:

| Method        | Path                                                    | Purpose                                                                                         |
| ------------- | ------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| `GET`, `HEAD` | `<subdir>/repodata.json`                                | Channel index for one subdir (`repodata_version: 1`, with `sha256` and `size` for each package) |
| `GET`, `HEAD` | `<subdir>/<name>-<version>-<build>.tar.bz2` or `.conda` | Package download                                                                                |
| `PUT`         | `ingest/<subdir>/<name>-<version>-<build>.tar.bz2`      | Upload a package. You may send `X-Checksum-Sha256`, the lowercase hex SHA-256 of the body.      |
| `DELETE`      | `ingest/<subdir>/<file>`                                | Delete a package. The artifact core's delete policy controls whether this is allowed.           |

Accepted subdirs: `noarch`, `linux-64`, `linux-aarch64`, `linux-ppc64le`, `linux-s390x`, `osx-64`, `osx-arm64`, `win-64`, `win-32`.

## Limits

- An upload can be at most 128 MiB compressed and 512 MiB expanded, with an expansion ratio of at most 200:1. It can contain at most 10,000 tar members, `info/index.json` at most 256 KiB, and `info/` at most 8 MiB in total.
- `info/index.json` must match the URL exactly: `name-version-build.tar.bz2` and the subdir. Nothing in the package runs (no `pre-link` or `post-link` scripts).
- A local upload must be a `.tar.bz2`. A `.conda` (v2) file sent to `ingest/` is rejected with 400. A cached remote channel can still serve `.conda` files.
- A subdir is one ownership unit. If any source attached to the feed holds a package in `linux-64`, that source alone answers `linux-64/repodata.json`. Sources are never merged file by file. In practice, a local package in `noarch` hides the upstream `noarch` of a remote source attached to the same feed. Put local packages and proxied channels in separate feeds and list both feeds in the client's `channels`.
- An index lists at most 4,096 packages per subdir.
- The server does not serve `current_repodata.json`, the `.zst` and `.bz2` variants of repodata, or sharded repodata. Clients fall back to `repodata.json` when these return 404.
- Uploads beyond the per-format concurrency limit (`ANT_UPLOAD_SLOTS`, which by default scales with CPU between 4 and 32) are answered with 503.
- Responses carry `Cache-Control: private, no-store`.

::: warning Gap
A remote repodata document is held in memory and is capped at 24 MiB, the metadata limit compiled into the binary. The `linux-64` and `noarch` repodata of conda-forge (and of other large channels) are bigger than that, so those subdirs cannot be proxied today: the request fails with 503. Small channels and mirrors work. The conformance run proxies `https://conda.anaconda.org/conda-test`.
:::

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `conda`. Then, on **Sources**, use **Create source** to create a `local` source. Attach it to the feed with **Attach source** and make it the default target in the feed's **Publish policy**. For a proxy feed, create a `remote` source instead and set its origin on the source's **Upstream** tab.

With the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# local channel for your own packages
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: conda-local-1' \
  -d '{"name":"conda-local","kind":"local","format":"conda"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: conda-feed-1' -d @- <<JSON
{"slug":"conda","format":"conda","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

For a separate proxy feed, see the next section. [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md) explain read order and publish targets.

## Remote upstreams

The origin of a remote conda source is the **channel root**: the URL whose children are `noarch/`, `linux-64/` and so on.

- `<subdir>/repodata.json` goes through the metadata cache (TTL, negative cache, stale-if-error). The server checks every entry: the filename must equal `name-version-build` plus the extension, and `sha256` must be present. Entries that fail the check are dropped. The document is then re-rendered with `info` reduced to `{"subdir": ...}`, because an upstream `base_url` would send clients around the feed.
- On first download, `.tar.bz2` and `.conda` files are fetched from `<subdir>/<file>`, checked against the repodata sha256 before any byte is served, and cached. For `.tar.bz2` files, the verified repodata entry is cached too, so cached packages stay listable while the upstream is down.
- If the upstream fails and the subdir has cached packages, the feed serves its local view with `X-Kuvryn-Cache: offline`. A response from a stale metadata cache entry carries `X-Kuvryn-Cache: stale` and `Warning: 110`.

| Upstream channel               | `origin`                                                                   | `redirect_origins`                                                  |
| ------------------------------ | -------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| conda-forge                    | `https://conda.anaconda.org/conda-forge`                                   | none needed by the code (see the warning above about repodata size) |
| Any small anaconda.org channel | `https://conda.anaconda.org/<channel>`                                     | none                                                                |
| A self-hosted mirror           | its channel root, for example `https://mirror.example.com/conda/mychannel` | the hosts it redirects downloads to, if any                         |

::: warning Unverified
The repository does not record whether `conda.anaconda.org` redirects package downloads to another host. If a download fails with 503 and the source health shows a refused redirect, add that host to `redirect_origins`. The conformance run against `conda-test` needed no redirect origins.
:::

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: conda-remote-1' \
  -d '{"name":"conda-test-remote","kind":"remote","format":"conda"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: conda-remote-cfg-1' -d '{
  "origin":"https://conda.anaconda.org/conda-test","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: conda-remote-feed-1' \
  -d "{\"slug\":\"conda-remote\",\"format\":\"conda\",\"source_ids\":[\"$REM\"]}"
```

A private upstream channel can take a credential: `PUT /api/v1/sources/{id}/remote-credential` with `{"authorization":"Bearer <upstream-token>"}`. The credential is stored encrypted, never returned, and sent only to the origin.

## Client configuration

The adapter accepts a service token or personal access token as `Authorization: Bearer`, or as the password of HTTP Basic auth with any username. Unauthenticated requests get `401` with `WWW-Authenticate: Basic realm="Conda"`. Anonymous reads work only when the feed has `anonymous_read` enabled and the feed's client-auth policy allows it (see [Package client auth](../identity/client-auth.md)).

### Consume

conda expands `${VAR}` in `.condarc`, so you can keep the token out of the file:

::: code-group

```yaml [~/.condarc (conda, mamba)]
channels:
  - https://token:${ANT_TOKEN}@ant.example.com/feeds/conda/conda
  - https://token:${ANT_TOKEN}@ant.example.com/feeds/conda-remote/conda
channel_priority: strict
```

```sh [command line]
conda create -n demo --override-channels \
  -c "https://token:${ANT_TOKEN}@ant.example.com/feeds/conda/conda" mypkg=1.0.0
mamba install --override-channels \
  -c "https://token:${ANT_TOKEN}@ant.example.com/feeds/conda/conda" mypkg
```

```json [~/.mamba/auth/authentication.json (micromamba)]
{
  "ant.example.com": { "type": "BearerToken", "token": "<TOKEN>" }
}
```

```sh [pixi]
pixi auth login ant.example.com --token "$ANT_TOKEN"
# pixi.toml / pyproject [tool.pixi.workspace]:
#   channels = ["https://ant.example.com/feeds/conda/conda"]
```

:::

::: warning Gap
The conformance run covers conda 26.7 and mamba 2.9 with credentials embedded in the channel URL (`https://user:token@...`). The micromamba `authentication.json` and pixi examples use those clients' documented bearer-token mechanisms but have not been run against Ant.

`channel_alias` and `custom_channels` build a channel URL by appending the channel name to a base URL. Ant's channel path always ends in `/conda`, so a short channel name works only if it is `<slug>/conda` (for example `-c conda/conda` with `channel_alias: https://ant.example.com/feeds`). This has not been tested. Use full channel URLs.
:::

### Publish

conda has no upload protocol that Ant implements (no `anaconda upload`). Build the package (for example with `conda build` or `rattler-build`, as `.tar.bz2`), then upload it with `PUT`. The token needs publish permission on the feed:

```sh
F=mypkg-1.0.0-0.tar.bz2
curl -fsS -u "token:$ANT_TOKEN" -X PUT \
  -H "X-Checksum-Sha256: $(sha256sum "$F" | cut -d' ' -f1)" \
  --data-binary @"$F" \
  "https://ant.example.com/feeds/conda/conda/ingest/noarch/$F"
# 201 Created; an identical retry is also 201, different bytes for the same file are 409
```

`rattler-build` writes `.conda` files by default. Ask it for `.tar.bz2` output (`--package-format tar-bz2`), because the ingest route does not accept `.conda`.

### GitHub Actions

Exchange the job's OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)), then use `$ANT_TOKEN` as above:

```yaml
- run: |
    conda create -y -n ci --override-channels \
      -c "https://ci:${ANT_TOKEN}@ant.example.com/feeds/conda/conda" mypkg
```

## Troubleshooting

| Symptom                                     | Cause                                                                                                                                                                |
| ------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` on `repodata.json`                    | No credentials, or the credentials did not reach the server. Check that the token sits in the channel URL or the auth file for exactly this host.                    |
| `403`                                       | The token's role grants or scopes do not cover this feed or subdir, or the feed's client-auth policy does not accept this credential type.                           |
| `400` on `PUT ingest/...`                   | The filename and `info/index.json` disagree (name, version, build or subdir), the file is `.conda`, the archive is malformed, or `X-Checksum-Sha256` does not match. |
| `409` on `PUT`                              | A different file already exists under that name. Versions are immutable. Bump the build number.                                                                      |
| `503` on a proxied `repodata.json`          | The upstream is unreachable and nothing is cached, the upstream repodata exceeds 24 MiB, or all upload slots are busy (on `PUT`).                                    |
| `X-Kuvryn-Cache: offline`                   | The upstream failed and the server answered from cached packages. The index then lists only `.tar.bz2` packages already cached.                                      |
| Upstream packages missing from a mixed feed | A local package exists in the same subdir. The subdir is owned by the local source. Use a separate proxy feed.                                                       |

Adapter internals: [internal/condarepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/condarepo/README.md).
