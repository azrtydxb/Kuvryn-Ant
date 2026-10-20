# PyPI (pip, uv, Poetry, twine)

The PyPI adapter serves the Python Simple Repository API (PEP 503 HTML and PEP 691
JSON, with PEP 658/714 core metadata) for pip, uv, Poetry, PDM and other installers,
and accepts uploads through the legacy upload API used by twine, `uv publish` and
`poetry publish`. A feed can combine a local source for internal packages with a
remote source that proxies pypi.org.

Endpoints:

```text
https://ant.example.com/feeds/<slug>/pypi/simple/      # index URL for installers
https://ant.example.com/feeds/<slug>/pypi/             # upload URL (also .../pypi/legacy/)
```

Implementation: [`internal/pypi`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/pypi/README.md).

## Support matrix

| Operation                               | Supported        | Notes                                                                                                       |
| --------------------------------------- | ---------------- | ----------------------------------------------------------------------------------------------------------- |
| Install (pip, uv, Poetry, PDM)          | Yes              | `simple/<project>/` in HTML or JSON by content negotiation; SHA-256 links, `Requires-Python`, yanked flags. |
| Core metadata (PEP 658 / 714)           | Yes, wheels      | `<file URL>.metadata`, verified. Sdist metadata is stored but not advertised.                               |
| Upload (`twine upload`)                 | Yes              | One wheel or `.tar.gz` sdist per request; wheel and sdist of one release can be uploaded separately.        |
| Upload (`uv publish`, `poetry publish`) | Expected to work | Same legacy upload API; not covered by a real-client run.                                                   |
| Yank / unyank (PEP 592)                 | Yes, adapter API | `PUT` / `DELETE files/<project>/<filename>/yanked`.                                                         |
| Delete a file                           | Yes, adapter API | `DELETE files/<project>/<filename>`, gated by the feed's delete policy.                                     |
| Project listing (`simple/`)             | Local only       | The root index lists locally published projects the caller can read, not upstream projects.                 |
| JSON API (`/pypi/<project>/json`)       | No               | Installers use the Simple API.                                                                              |
| Remote (pull-through)                   | Yes              | Project pages rewritten to the feed, files verified against the upstream SHA-256 and cached.                |
| Replica                                 | Yes              | Through the format-agnostic replication core.                                                               |

Real-client conformance (2026-10-19, pip 26.2.1, twine 7.0.0, Python 3.14): twine
upload of wheel and sdist, pip install, upload of a newer version and resolution of
it, yank / restore / delete and pip's handling of them, pull-through from pypi.org
(miss, then cached wheel served with the upstream unreachable) and auth denial.

## Limits

- **Distribution types:** wheels and `.tar.gz` sdists only. Legacy `.zip` sdists,
  eggs and ZIP64 archives are rejected, and so are version spellings outside the
  normalized PEP 440 subset the adapter recognises.
- **Wheel metadata must have a body.** A wheel whose `METADATA` has no blank line after
  the headers (setuptools writes this when the project has no long description or
  readme) is rejected with `400`. Add a `readme` to the project.
- **Validation:** the project and version in the file name must match the metadata;
  ZIP CRCs, wheel `RECORD` hashes and sizes and the `WHEEL` file are checked; unsafe
  or duplicate paths, links and special files are rejected. Nothing is extracted or
  executed.
- **Bounds:** four concurrent uploads per process (more get `503`), 10,000 archive
  entries, 1 MiB per metadata file, 512 MiB expanded archive size, 128 multipart
  parts. Indexes at most 10,000 entries and 16 MiB.
- **Immutability:** re-uploading a file name with different bytes returns `409`; an
  identical retry succeeds.
- **Credentials are mandatory** on every request unless the feed allows anonymous
  reads; a browser session does not count.
- **Remote:** only files the upstream lists with a SHA-256 are served, and only wheel
  and `.tar.gz` files. The upstream must serve project pages at
  `<origin>/simple/<project>/`; indexes with another layout cannot be used as a
  remote origin.

::: warning Gap

Not verified by a real-client run: `uv publish`, `poetry publish`, installs with uv,
Poetry and PDM (they use the same Simple API that pip uses), and whether the project
index of a remote package is still served while the upstream is down (cached files
stay downloadable by their feed URL; the index behaviour in an outage is not
recorded).

:::

## Create the feed

### Console

1. **Sources**: **Create source** twice, format `pypi`: a `local` source
   (`pypi-local`) and a `remote` source (`pypi-org`).
2. On the remote source's **Upstream** tab set origin `https://pypi.org` and redirect
   origin `https://files.pythonhosted.org`.
3. **Feeds**: **Create feed**, slug `pypi`, format `pypi`. Attach `pypi-local` first,
   `pypi-org` second, and make `pypi-local` the publish policy's default source.

A single feed with both sources protects against dependency confusion: a project name
that exists in the local source is never completed from pypi.org. Prefer it over
`--extra-index-url`, which lets pip pick a public package with the same name.

### API

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: pypi-local-1' \
  -d '{"name":"pypi-local","kind":"local","format":"pypi"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: pypi-remote-1' \
  -d '{"name":"pypi-org","kind":"remote","format":"pypi"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: pypi-remote-cfg-1' -d '{
  "origin":"https://pypi.org","redirect_origins":["https://files.pythonhosted.org"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: pypi-feed-1' -d @- <<JSON
{"slug":"pypi","format":"pypi","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

## Remote upstreams

| Upstream                                                  | `origin`                                                                                 | `redirect_origins`                              | Status       |
| --------------------------------------------------------- | ---------------------------------------------------------------------------------------- | ----------------------------------------------- | ------------ |
| pypi.org                                                  | `https://pypi.org`                                                                       | `https://files.pythonhosted.org`                | Verified     |
| Another Simple API index (devpi, Nexus, another Ant feed) | The URL above `simple/`, for example `https://pypi.internal.example.com/repository/pypi` | Hosts its file URLs point to, if not the origin | Not verified |

- The project page is fetched from `<origin>/simple/<project>/`, preferring PEP 691
  JSON and accepting PEP 503 HTML, through the metadata cache.
- It is reduced to the files the adapter can serve (see [Limits](#limits)) and
  re-rendered in the representation the client asked for, with every file URL pointing
  at the feed. File links embed the serving source and the file's SHA-256, so the
  download is bound to the metadata the client saw.
- The upstream file URL (on pypi.org, `files.pythonhosted.org`) is looked up server
  side when the file is requested; clients never supply it. The file is fully staged
  and verified against the SHA-256 before the first byte is served, then cached.
- For an upstream that needs credentials, store a source credential such as
  `{"authorization":"Basic <base64 of user:password>"}`; it is sent to the origin
  only, never to a redirect origin.

## Client configuration

### Authentication

Use HTTP Basic with any non-empty username (for example `__token__`) and a service
token (`ant_...`), personal access token (`antp_...`) or GitHub OIDC token as the
password; a Bearer header works too. Directory users can use their own username and
password where the feed's client-auth policy allows it. An empty username or password
is rejected. Tokens are URL-safe, so they can be embedded in an index URL.

### Install

::: code-group

```ini [pip (pip.conf)]
# ~/.config/pip/pip.conf (Linux), ~/Library/Application Support/pip/pip.conf (macOS),
# %APPDATA%\pip\pip.ini (Windows), or /etc/pip.conf
[global]
index-url = https://__token__:<TOKEN>@ant.example.com/feeds/pypi/pypi/simple/
```

```sh [pip (environment)]
export PIP_INDEX_URL="https://__token__:${ANT_TOKEN}@ant.example.com/feeds/pypi/pypi/simple/"
pip install requests
```

```text [pip (.netrc)]
# ~/.netrc, chmod 600; then index-url without credentials
machine ant.example.com
login __token__
password <TOKEN>
```

```toml [uv (pyproject.toml)]
[[tool.uv.index]]
name = "ant"
url = "https://ant.example.com/feeds/pypi/pypi/simple/"
publish-url = "https://ant.example.com/feeds/pypi/pypi/"
default = true
```

```toml [uv (uv.toml)]
[[index]]
name = "ant"
url = "https://ant.example.com/feeds/pypi/pypi/simple/"
publish-url = "https://ant.example.com/feeds/pypi/pypi/"
default = true
```

```sh [Poetry]
poetry source add --priority=primary ant https://ant.example.com/feeds/pypi/pypi/simple/
poetry config http-basic.ant __token__ "$ANT_TOKEN"
poetry install
```

:::

uv reads the credentials of a named index from the environment, so they stay out of
the file:

```sh
export UV_INDEX_ANT_USERNAME=__token__
export UV_INDEX_ANT_PASSWORD="$ANT_TOKEN"
uv sync
```

Poetry equivalently reads `POETRY_HTTP_BASIC_ANT_USERNAME` and
`POETRY_HTTP_BASIC_ANT_PASSWORD`. For a private CA, point pip at it with
`cert = /path/ca.pem` in `pip.conf` (or `PIP_CERT`), uv with `SSL_CERT_FILE`, and
Poetry with `poetry config certificates.ant.cert /path/ca.pem`.

### Publish

The upload URL is the feed's `pypi/` root (not `simple/`).

::: code-group

```ini [twine (~/.pypirc)]
[distutils]
index-servers = ant

[ant]
repository = https://ant.example.com/feeds/pypi/pypi/
username = __token__
password = <TOKEN>
```

```sh [twine (command line)]
python -m build
twine upload --repository-url https://ant.example.com/feeds/pypi/pypi/ \
  -u __token__ -p "$ANT_TOKEN" dist/*
# with ~/.pypirc:  twine upload -r ant dist/*
```

```sh [uv]
uv build
uv publish --publish-url https://ant.example.com/feeds/pypi/pypi/ \
  --username __token__ --password "$ANT_TOKEN"
# or, with publish-url in [[tool.uv.index]]:  uv publish --index ant
```

```sh [Poetry]
poetry config repositories.ant-upload https://ant.example.com/feeds/pypi/pypi/
poetry config http-basic.ant-upload __token__ "$ANT_TOKEN"
poetry publish --build -r ant-upload
```

:::

Yank, restore and delete files (no Python client has these commands):

```sh
F=https://ant.example.com/feeds/pypi/pypi/files/mypkg
curl -fsS -X PUT    -u "__token__:$ANT_TOKEN" $F/mypkg-1.1.0-py3-none-any.whl/yanked   # yank
curl -fsS -X DELETE -u "__token__:$ANT_TOKEN" $F/mypkg-1.1.0-py3-none-any.whl/yanked   # restore
curl -fsS -X DELETE -u "__token__:$ANT_TOKEN" $F/mypkg-1.1.0-py3-none-any.whl          # delete
```

Each returns `204`. Yanked files are marked as yanked in the index (PEP 592), and pip
skips them when it resolves a requirement.

### GitHub Actions

```yaml
- uses: actions/setup-python@v5
  with:
    python-version: "3.13"
- name: Install
  env:
    PIP_INDEX_URL: https://__token__:${{ secrets.ANT_TOKEN }}@ant.example.com/feeds/pypi/pypi/simple/
  run: pip install -r requirements.txt
- name: Publish
  env:
    TWINE_REPOSITORY_URL: https://ant.example.com/feeds/pypi/pypi/
    TWINE_USERNAME: __token__
    TWINE_PASSWORD: ${{ secrets.ANT_TOKEN }}
  run: |
    python -m pip install build twine
    python -m build
    twine upload dist/*
```

With uv, set `UV_INDEX_ANT_USERNAME` / `UV_INDEX_ANT_PASSWORD` for installs and
`UV_PUBLISH_USERNAME` / `UV_PUBLISH_PASSWORD` for `uv publish`. To avoid a stored
secret, exchange the workflow's OIDC token for a short-lived Ant token first; see
[GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                                                                          | Cause                                                                                                                                                             |
| -------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| pip prompts for a username, or `401` with `WWW-Authenticate: Basic realm="pypi"` | No credentials sent: the index URL or `.netrc` entry is missing them, or the username or password is empty.                                                       |
| `403`                                                                            | The principal lacks read (install) or publish (upload) on the feed, or the feed's client-auth policy does not accept that credential type.                        |
| `301` from `simple/<Name>`                                                       | Project URLs are normalized (lowercase, runs of `-_.` collapsed to `-`) and need a trailing slash; clients follow the redirect.                                   |
| `404` for a project that exists upstream                                         | No remote source on the feed, the upstream has no SHA-256 for its files, or only file types the adapter skips (zip sdists, eggs).                                 |
| `400` on upload                                                                  | Invalid or mismatched metadata, a wheel `METADATA` without a body (add a readme), an unsupported version spelling or archive format, or a failed integrity check. |
| `409` on upload                                                                  | A file with that name already exists with different bytes.                                                                                                        |
| `503` on upload                                                                  | Four uploads already running; retry.                                                                                                                              |
| `503` on install from a remote feed                                              | Upstream error (errors do not fall through to the next source), or a file URL on a host not in `redirect_origins`. Check `GET /api/v1/sources/{id}/health`.       |
| `X-Kuvryn-Cache: stale` on a project page                                        | The upstream page was served from the metadata cache after its TTL because the upstream failed.                                                                   |
