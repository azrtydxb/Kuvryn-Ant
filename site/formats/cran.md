# CRAN

The CRAN adapter serves an R package repository in the CRAN layout: `src/contrib/PACKAGES[.gz]` and source tarballs. `install.packages()`, and other tools that read `options(repos)`, can install from it. A feed can hold source packages you upload, and it can proxy and cache a CRAN mirror, including its binary package trees.

Repository URL: `https://ant.example.com/feeds/<slug>/cran` (feed format `cran`). R appends `/src/contrib/...` (and `/bin/<platform>/contrib/<rver>/...` for binaries) to it.

## Support matrix

| Publish                                                            | Consume                                                | Remote (pull-through)                          | Replica                       | Notes                                                                    |
| ------------------------------------------------------------------ | ------------------------------------------------------ | ---------------------------------------------- | ----------------------------- | ------------------------------------------------------------------------ |
| Admin ingestion: `PUT src/contrib/<pkg>_<ver>.tar.gz` (feed admin) | Yes, `install.packages` (R 4.6 in the conformance run) | Yes: source tarballs, `Archive/`, binary trees | Yes, through core replication | Local packages are source only. Binaries come only from a remote mirror. |

Routes under `/feeds/<slug>/cran/`:

| Method        | Path                                                                       | Purpose                                                                                                      |
| ------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------ |
| `GET`, `HEAD` | `src/contrib/PACKAGES`, `src/contrib/PACKAGES.gz`                          | Index. A local index is generated; with a remote source it is merged with the upstream index by package name |
| `GET`, `HEAD` | `src/contrib/<pkg>_<ver>.tar.gz`                                           | Source package                                                                                               |
| `PUT`         | `src/contrib/<pkg>_<ver>.tar.gz`                                           | Upload a source package. You may send `X-Checksum-Sha256`                                                    |
| `DELETE`      | `src/contrib/<pkg>_<ver>.tar.gz`                                           | Delete a version. The artifact core's delete policy controls whether this is allowed.                        |
| `GET`, `HEAD` | `src/contrib/Archive/<pkg>/<pkg>_<ver>.tar.gz`                             | Archived source versions (remote)                                                                            |
| `GET`, `HEAD` | `bin/<platform>/contrib/<rver>/PACKAGES[.gz]`, `<pkg>_<ver>.zip` or `.tgz` | Binary indexes and packages (remote, passed through)                                                         |

## Limits

- R has no native publish protocol. Upload is an adapter-specific `PUT` that needs **Admin** on the feed, plus publish on the selected local source.
- An upload can be at most 32 MiB, and at most 128 MiB expanded, with at most 10,000 tar members. `DESCRIPTION` is parsed as one bounded DCF record (at most 256 KiB). No R code runs. `Package` and `Version` must match the URL. `Title`, `Description` and `License` are mandatory.
- The local index includes `Package`, `Version`, `Depends`, `Imports`, `LinkingTo`, `Suggests`, `Enhances`, `License`, `OS_type`, `Archs`, `MD5sum` and `NeedsCompilation`. The newest version of each package wins. An index read fails once more than 4,096 candidates exist.
- `PACKAGES.rds`, `Meta/archive.rds` and directory listings of `Archive/<pkg>/` are not served. R falls back to `PACKAGES.gz`.
- Versions are immutable. An identical retry returns 201, different bytes 409.
- Responses carry `Cache-Control: private, no-store`. Uploads beyond `ANT_UPLOAD_SLOTS` get 503.

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `cran`. On **Sources**, use **Create source** to create a `local` source, attach it with **Attach source** and make it the **Publish policy** default. To proxy CRAN, add a `remote` source with origin `https://cloud.r-project.org` on its **Upstream** tab and attach it after the local source.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: cran-local-1' \
  -d '{"name":"cran-local","kind":"local","format":"cran"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: cran-remote-1' \
  -d '{"name":"cran-cloud","kind":"remote","format":"cran"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: cran-remote-cfg-1' -d '{
  "origin":"https://cloud.r-project.org","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: cran-feed-1' -d @- <<JSON
{"slug":"cran","format":"cran","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The origin is the **mirror root**, the URL that contains `src/` and `bin/`.

- The upstream `src/contrib/PACKAGES.gz` is read through the metadata cache (it is compressed, so it fits well under the metadata limit). It is validated and merged into the feed's `PACKAGES` and `PACKAGES.gz` by package name. A package name that exists in a local source never takes upstream metadata or archives. Ownership is checked per package name across all versions.
- Source tarballs (`src/contrib/<pkg>_<ver>.tar.gz` and `src/contrib/Archive/<pkg>/...`) share one cache entry per version and are cached on first download.
- Binary indexes (`bin/<platform>/contrib/<rver>/PACKAGES[.gz]`) are passed through verbatim from the metadata cache. Binary packages are cached on first download.
- CRAN publishes only MD5 sums, which the artifact core does not use, so cache fills are checked for length but **not hash-verified**.
- A response from a stale metadata cache entry carries `X-Kuvryn-Cache: stale`. When the upstream is unreachable, the local index is served with `X-Kuvryn-Cache: offline`.

| Upstream                     | `origin`                                         | `redirect_origins`                |
| ---------------------------- | ------------------------------------------------ | --------------------------------- |
| CRAN (cloud mirror)          | `https://cloud.r-project.org`                    | none                              |
| A specific CRAN mirror       | its root, for example `https://cran.example.org` | none, unless the mirror redirects |
| Posit Public Package Manager | not tested                                       |                                   |

::: tip
The conformance run (R 4.6.1) installs `praise` through a feed with origin `https://cloud.r-project.org`, then downloads the cached tarball while the upstream is unreachable.
:::

## Client configuration

The adapter accepts a token as `Authorization: Bearer` or as the HTTP Basic password, with any username. Unauthenticated requests get `401`. R's default `libcurl` download method sends credentials embedded in the repository URL.

### Consume

::: code-group

```r [~/.Rprofile]
local({
  tok <- Sys.getenv("ANT_TOKEN")
  options(repos = c(
    ANT = sprintf("https://token:%s@ant.example.com/feeds/cran/cran", tok)
  ))
})
```

```r [one-off]
options(repos = c(ANT = "https://token:<TOKEN>@ant.example.com/feeds/cran/cran"))
install.packages("mypkg", type = "source")
```

```sh [Rscript in CI]
REPO="https://ci:${ANT_TOKEN}@ant.example.com/feeds/cran/cran" \
  Rscript -e 'options(repos = c(ANT = Sys.getenv("REPO"))); install.packages("mypkg", type = "source")'
```

:::

On macOS and Windows, R asks for binary packages first (`bin/...`). A local-only feed has no binary trees, so pass `type = "source"` for packages you uploaded. With a remote mirror attached, binary indexes and packages come from upstream.

::: warning Gap
The conformance run covers `install.packages` with credentials in the repository URL and `type = "source"`. `pak`, `renv` and `remotes` read the same `repos` option, but they have not been run against Ant. Binary installs through a remote source on macOS or Windows have not been exercised by a recorded real-client run.
:::

### Publish

Build the source tarball with `R CMD build`, then upload it. The token's principal needs **Admin** on the feed:

```sh
R CMD build mypkg
F=mypkg_1.0.0.tar.gz
curl -fsS -u "token:$ANT_TOKEN" -X PUT \
  -H "X-Checksum-Sha256: $(sha256sum "$F" | cut -d' ' -f1)" \
  --data-binary @"$F" \
  "https://ant.example.com/feeds/cran/cran/src/contrib/$F"
```

To remove a version, send `DELETE` to the same URL (this needs delete permission, subject to the delete policy). `PACKAGES` is regenerated immediately.

### GitHub Actions

Exchange the OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)) and put it in the repository URL:

```yaml
- run: |
    Rscript -e 'options(repos = c(ANT = sprintf("https://ci:%s@ant.example.com/feeds/cran/cran", Sys.getenv("ANT_TOKEN")))); install.packages("mypkg", type = "source")'
```

## Troubleshooting

| Symptom                                                           | Cause                                                                                                                           |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------- |
| `unable to access index for repository .../src/contrib`           | `401` or `403`: credentials are missing from the URL, or the token lacks read on the feed.                                      |
| `unable to access index ... /bin/...` warning on macOS or Windows | The feed has no binary tree (local only). Use `type = "source"`.                                                                |
| `403` on `PUT`                                                    | The principal lacks Admin on the feed. Ingestion is a feed-admin operation.                                                     |
| `400` on `PUT`                                                    | `DESCRIPTION` is missing required fields, `Package` or `Version` does not match the file name, or the checksum header is wrong. |
| `409` on `PUT`                                                    | That version already exists with different bytes.                                                                               |
| Upstream package missing                                          | A local source owns that package name. Local names never take upstream versions.                                                |
| `503`                                                             | The upstream is unreachable and the file is not cached, or upload slots are busy.                                               |

Adapter internals: [internal/cran/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/cran/README.md).
