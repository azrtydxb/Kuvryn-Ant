# GitHub Actions

[`azrtydxb/ant-setup-action`](https://github.com/azrtydxb/ant-setup-action) is a composite action that exchanges the job's GitHub OIDC token for a short-lived Kuvryn Ant token and points the job's package managers at your Ant feeds. No secret is stored in GitHub. When OIDC is not available (Dependabot, pull requests from forks, a missing `id-token: write` permission) or the exchange is refused, the action logs a warning and leaves the public upstream defaults in place, unless you set `required: "true"`.

This page documents the action from its source (`action.yml`, `setup.sh`, `lib.sh`, `build-cache.sh`, `install-sccache.sh` and the `cache-cleanup` and `cache-stats` sub-actions). A fallback that uses a stored token secret instead of the action is at the end.

::: tip Pin the action
The examples use `azrtydxb/ant-setup-action@<sha> # v1.1.0`. Replace `<sha>` with the full commit SHA of the release you have reviewed. The tags `v1` and `v1.1.0` currently point at the same commit, which includes the build-cache options and both sub-actions. Pin sub-actions to the same SHA: `azrtydxb/ant-setup-action/cache-stats@<sha>`.
:::

## What the action does

1. **Reachability probe.** It sends an unauthenticated `GET https://<host>/v2/` (with `curl`, `wget` or `node` 18+, whichever exists in the job image). Any non-2xx answer counts as "Ant is not answering" and triggers the fallback.
2. **OIDC token.** It requests a GitHub OIDC ID token with the audience `https://<host>` from `ACTIONS_ID_TOKEN_REQUEST_URL`.
3. **Exchange.** It posts the JWT to `POST https://<host>/api/v1/auth/github-oidc/exchange` (`{"token":"<jwt>"}`) and reads `access_token` from the answer. The token is masked with `::add-mask::` and never printed.
4. **Configuration.** It writes the token and the feed URLs into the job: environment variables through `$GITHUB_ENV` and config files in `$HOME`. It also exports `ANT_HOST` and `ANT_TOKEN` for later steps.
5. **Build caches** (opt-in): buildx registry cache references and sccache over WebDAV.

The token lives for the `ttl_seconds` of the trust rule that matched (60 to 3600 seconds, default 900). Steps that run after it expires fail authentication, so set the rule's TTL to cover your longest job.

::: warning Ant-side requirement for the probe
Ant answers an unauthenticated `GET /v2/` with `200` only when the mirror default feed (`ANT_OCI_DEFAULT_FEED`) names an OCI feed with anonymous reads. Otherwise it answers `401` with a Basic challenge, and the action treats that as "not answering" and falls back to the public registries (or fails with `required: "true"`). Configure `ANT_OCI_DEFAULT_FEED` with an anonymous-read mirror feed (see [OCI / Docker](../formats/oci.md) and [Configuration](../reference/configuration.md)) on any instance the action talks to.
:::

## Ant-side setup: OIDC trust

Full details are in [GitHub OIDC trust](../identity/github-oidc.md). The short version:

1. **Audience.** Set the GitHub OIDC audience to exactly `https://ant.example.com` (the action always requests `https://<host>`; the value must match byte for byte):

   ```sh
   curl -fsS -X PUT $ANT/api/v1/auth/github-oidc/config -H "$H" -H "$J" \
     -H 'Idempotency-Key: gh-oidc-audience-1' -d '{"audience":"https://ant.example.com"}'
   ```

   An empty audience disables the feature. The issuer is fixed to `https://token.actions.githubusercontent.com`.

2. **Service principal.** Create a service principal and give its role the grants the workflows need (read on the language feeds, publish on the OCI feed you push to, read, publish and delete on the cache feeds). A trust rule can only narrow what the principal holds, never widen it. See [Service accounts and tokens](../identity/tokens.md) and [Roles and grants](../identity/roles.md).

3. **Trust rules.** A rule matches `org` (repository owner, case-insensitive), `repo` (glob over the name without owner), `ref` (glob over the full ref; a value that does not start with `refs/` is a branch glob, so `main` means `refs/heads/main`), `environment` and `workflow` (globs, default `*`). The oldest enabled matching rule wins. Its `scopes` (feed, optional namespace, actions `read`, `publish`, `delete`) narrow the issued token.

   ```sh
   # Default branch and tags may publish; every other ref is read only.
   curl -fsS -X POST $ANT/api/v1/auth/github-oidc/trust-rules -H "$H" -H "$J" \
     -H 'Idempotency-Key: gh-rule-publish-1' -d @- <<JSON
   {"name":"myorg main","org":"myorg","repo":"*","ref":"main","principal_id":"$PUBLISH_PRINCIPAL",
    "ttl_seconds":3600,
    "scopes":[{"feed_id":"$NPM_FEED","actions":["read"]},
              {"feed_id":"$OCI_HOSTED_FEED","actions":["read","publish"]},
              {"feed_id":"$BUILDCACHE_FEED","actions":["read","publish","delete"]},
              {"feed_id":"$SCCACHE_FEED","actions":["read","publish","delete"]}]}
   JSON
   ```

   Because the oldest matching rule wins, create the narrow rules (`main`, `refs/tags/*`) before a catch-all `ref: "*"` read-only rule.

4. **Workflow permission.** Every job (or the workflow) that runs the action needs:

   ```yaml
   permissions:
     id-token: write
     contents: read
   ```

## Ecosystems it configures

All values below use the default feed slugs; each slug is an input. `<host>` is the `host` input.

| Tool       | Default                                           | What is written                                                                                                                                                                                                                                                                                   |
| ---------- | ------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Go         | on (`go`)                                         | `~/.netrc` entry `machine <host> login ci password <token>` (mode 600), `NETRC`, `GOPROXY=https://<host>/feeds/go/go\|https://proxy.golang.org,direct`, and `GOSUMDB=sum.golang.org https://sum.golang.org` unless `GOSUMDB` is already set                                                       |
| npm / pnpm | on (`npm`)                                        | `~/.npmrc` with `registry=https://<host>/feeds/npm/npm/` and `//<host>/feeds/npm/npm/:_authToken=<token>`; env `NPM_CONFIG_REGISTRY`, `npm_config_registry`, `NODE_AUTH_TOKEN`, `npm_config_fetch_retries=6`, `npm_config_fetch_retry_mintimeout=5000`, `npm_config_fetch_retry_maxtimeout=60000` |
| Cargo      | on (`cargo`)                                      | appends to `$CARGO_HOME/config.toml` (once): `[source.crates-io] replace-with = "ant"`, `[source.ant] registry = "sparse+https://<host>/feeds/cargo/cargo/"`, `[registries.ant] index = ...`, `credential-provider = "cargo:token"`; env `CARGO_REGISTRIES_ANT_TOKEN`, `CARGO_NET_RETRY=6`        |
| pip / uv   | on (`pip`)                                        | `PIP_INDEX_URL`, `UV_INDEX_URL` and `UV_DEFAULT_INDEX` set to `https://ci:<token>@<host>/feeds/pypi/pypi/simple/`; `PIP_RETRIES=6`                                                                                                                                                                |
| Maven      | opt-in (`maven`)                                  | `~/.m2/settings.xml` with mirror `ant` of `central` to `https://<host>/feeds/maven/maven/` and server `ant` (username `ci`, the token as password)                                                                                                                                                |
| NuGet      | opt-in (`nuget`)                                  | `~/.nuget/NuGet/NuGet.Config` with `<clear />` and the single source `https://<host>/feeds/nuget/nuget/v3/index.json`, with `Username` `ci` and `ClearTextPassword`                                                                                                                               |
| Composer   | opt-in (`composer`)                               | `$COMPOSER_HOME/auth.json` (`http-basic` for `<host>`) and `$COMPOSER_HOME/config.json` with repository `https://<host>/feeds/composer/composer`; exports `COMPOSER_HOME`                                                                                                                         |
| Docker     | opt-in (`docker-login`, implied by `build-cache`) | `docker login <host> -u ci --password-stdin`, retried with 2, 5, 10 and 20 second waits while Ant answers errors (for example `503` during a restart)                                                                                                                                             |

Notes from the source:

- The Go proxy entry uses `|`, so any Ant error (a `5xx` during a restart, a `400` for a non-module path) falls through to `proxy.golang.org`. Checksums are fetched directly from `sum.golang.org` because, per the action's own comment, Ant's sumdb proxy answers `/supported` but returns `404` for some `/lookup` requests, which `go` treats as fatal.
- A Maven, NuGet or Composer file that the action did not write is copied once to `<file>.pre-ant` before it is replaced.
- Run the action in every job that needs it, after `actions/checkout`, and inside the job's `container:` if it has one: the files go to that container's `$HOME`. The image needs `curl`, `wget` or `node` 18+.
- The pip index URL contains the token as the password. GitHub masks the token in logs, but tools that print their configuration verbatim will expose it to anyone who can read the job's environment.

See the format pages for the protocols behind these settings: [Go](../formats/go.md), [npm](../formats/npm.md), [Cargo](../formats/cargo.md), [PyPI](../formats/pypi.md), [Maven](../formats/maven.md), [NuGet](../formats/nuget.md), [Composer](../formats/composer.md), [OCI](../formats/oci.md).

## Inputs

| Input             | Default                    | Description                                                                                                           |
| ----------------- | -------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| `host`            | the project's own instance | Ant host name, without scheme. Always set it, for example `ant.example.com`.                                          |
| `go`              | `"true"`                   | Configure `GOPROXY` and `~/.netrc` for the go feed.                                                                   |
| `npm`             | `"true"`                   | Configure the npm/pnpm registry and auth token.                                                                       |
| `cargo`           | `"true"`                   | Replace crates.io with the cargo sparse feed.                                                                         |
| `pip`             | `"true"`                   | Set `PIP_INDEX_URL` / `UV_INDEX_URL` / `UV_DEFAULT_INDEX`.                                                            |
| `maven`           | `"false"`                  | Write `~/.m2/settings.xml` mirroring Maven Central to the maven feed.                                                 |
| `nuget`           | `"false"`                  | Write the user `NuGet.Config` with the Ant nuget feed as the only source.                                             |
| `composer`        | `"false"`                  | Write `auth.json` and a repositories entry for the composer feed.                                                     |
| `docker-login`    | `"false"`                  | `docker login` to the host (needed to push). A failed login with this input set triggers the fallback.                |
| `build-cache`     | `"false"`                  | Export buildx registry cache refs (outputs and `ANT_CACHE_*` env), `docker login`, and install and configure sccache. |
| `arch-suffix`     | derived                    | Architecture part of the buildx cache tag: `amd64` or `arm64` from `uname -m`, otherwise the raw `uname -m`.          |
| `cache-name`      | `""`                       | Optional name inserted in the tag (`<branch>-<name>-<arch>`) for a repository that builds several images.             |
| `cache-branch`    | `""`                       | Override the branch part of the tag. Default: PR head branch, or ref name, or the default branch for tag builds.      |
| `cache-write`     | `"true"`                   | `"false"` reads caches only (`cache-to` stays empty).                                                                 |
| `sccache`         | `auto`                     | With `build-cache`: `"true"`, `"false"` or `auto` (install when `cargo`, `rustc` or `Cargo.toml` is present).         |
| `sccache-version` | `"0.17.0"`                 | sccache release to install when it is missing (SHA-256 verified).                                                     |
| `required`        | `"false"`                  | Fail the step instead of falling back when no token can be obtained.                                                  |
| `cache-feed`      | `buildcache`               | Slug of the OCI cache feed.                                                                                           |
| `sccache-feed`    | `sccache`                  | Slug of the raw cache feed.                                                                                           |
| `go-feed`         | `go`                       | Slug of the Go feed.                                                                                                  |
| `npm-feed`        | `npm`                      | Slug of the npm feed.                                                                                                 |
| `cargo-feed`      | `cargo`                    | Slug of the Cargo feed.                                                                                               |
| `pypi-feed`       | `pypi`                     | Slug of the PyPI feed.                                                                                                |
| `maven-feed`      | `maven`                    | Slug of the Maven feed.                                                                                               |
| `nuget-feed`      | `nuget`                    | Slug of the NuGet feed.                                                                                               |
| `composer-feed`   | `composer`                 | Slug of the Composer feed.                                                                                            |

## Outputs

| Output       | Description                                                                                                          |
| ------------ | -------------------------------------------------------------------------------------------------------------------- |
| `configured` | `"true"` when a token was obtained and the tools were configured, `"false"` after a fallback.                        |
| `cache-from` | Multi-line value for `docker/build-push-action` `cache-from`. Empty when `build-cache` is off or Ant is unavailable. |
| `cache-to`   | Value for `cache-to`. Empty when `build-cache` is off, Ant is unavailable or `cache-write` is `"false"`.             |
| `cache-ref`  | Registry repository of the buildx cache, for example `ant.example.com/buildcache/myorg/myrepo`.                      |
| `cache-tag`  | Tag of the cache, `<branch>[-<name>]-<arch>`.                                                                        |
| `sccache`    | `"true"` when sccache is installed and pointed at Ant.                                                               |

Environment variables exported for later steps: `ANT_HOST`, `ANT_TOKEN` and the tool variables in the table above; with `build-cache` also `ANT_CACHE_REPO`, `ANT_CACHE_BRANCH`, `ANT_CACHE_DEFAULT_BRANCH`, `ANT_CACHE_ARCH`, `ANT_CACHE_FROM`, `ANT_CACHE_TO`, and for sccache `RUSTC_WRAPPER=sccache`, `CARGO_INCREMENTAL=0`, `SCCACHE_WEBDAV_ENDPOINT`, `SCCACHE_WEBDAV_TOKEN`, `SCCACHE_WEBDAV_KEY_PREFIX`.

## Build caches

`build-cache: "true"` points the job at two [cache feeds](../build-caches/index.md): an OCI cache feed for buildx layers (default slug `buildcache`) and a raw cache feed for sccache (default slug `sccache`). It uses the same exchanged token.

### buildx registry cache

- **Repository.** `cache-ref` is `<host>/<cache-feed>/<org>/<repo>`, with the repository name lowercased.
- **Tag.** `cache-tag` is `<branch>[-<name>]-<arch>`. Characters outside `[A-Za-z0-9_.-]` become `-`, leading `.` and `-` are removed, the branch part is cut to 80 characters and the name part to 30. The branch is `cache-branch` if set; otherwise the default branch for tag builds (a release shares the default branch's cache instead of creating one cache per tag); otherwise the PR head branch; otherwise the ref name.
- **`cache-from`.** Up to two lines, this branch's tag first, then the default branch's tag as a fallback. The action lists a ref only if its manifest exists (it checks with an authenticated `GET /v2/<feed>/<repo>/manifests/<tag>`), because a missing `cache-from` ref is fatal for `docker/build-push-action` v6 and a warning for v7. On the first build of a repository `cache-from` is empty.
- **`cache-to`.** `type=registry,ref=<cache-ref>:<cache-tag>,mode=max,image-manifest=true,ignore-error=true`. `image-manifest=true` is required by Ant (see [buildx registry cache](../build-caches/buildx.md)); `ignore-error=true` keeps a cache outage or expired token from failing the build.
- If `docker login` fails, the buildx outputs are left empty and the build runs without a remote cache.

A job that builds several images can compose its own refs from the exported variables:

```yaml
cache-to: type=registry,ref=${{ env.ANT_CACHE_REPO }}:${{ env.ANT_CACHE_BRANCH }}-api-${{ env.ANT_CACHE_ARCH }},mode=max,image-manifest=true,ignore-error=true
```

### sccache over WebDAV

When sccache is wanted (`sccache` input) and missing, `install-sccache.sh` downloads `sccache-v<version>-<triple>.tar.gz` from the mozilla/sccache GitHub releases (`x86_64-unknown-linux-musl` or `aarch64-unknown-linux-musl`), verifies it against the published `.sha256`, and installs it to `/usr/local/bin` or, when that is not writable (GitHub-hosted runners), to `~/.local/bin` (added to `$GITHUB_PATH`). Linux only. It then exports:

```sh
RUSTC_WRAPPER=sccache
CARGO_INCREMENTAL=0
SCCACHE_WEBDAV_ENDPOINT=https://<host>/feeds/sccache/raw
SCCACHE_WEBDAV_TOKEN=<token>
SCCACHE_WEBDAV_KEY_PREFIX=<org>/<repo>
```

The key prefix is shared by all branches of a repository, which is the right default because sccache keys are content hashes of the compiler invocation. To isolate branches (for example untrusted pull requests), override it in a later step: `echo "SCCACHE_WEBDAV_KEY_PREFIX=$GITHUB_REPOSITORY/$GITHUB_HEAD_REF" >> "$GITHUB_ENV"`. `build-cache.sh` also reads an `ANT_SCCACHE_PREFIX` environment variable for the prefix; it is not a declared input.

sccache is still installed when Ant is unreachable (it then caches on local disk), so steps that name `RUSTC_WRAPPER` keep working; the `sccache` output is `"false"` in that case. sccache fails soft: judge it by the `cache-stats` step, not by the build passing. See [sccache over WebDAV](../build-caches/sccache.md).

## Sub-action: cache-stats

`azrtydxb/ant-setup-action/cache-stats` prints `sccache --show-stats`, adds `hits` and `misses` to the job summary, and can fail the job when there were too few hits. Run it last with `if: always()`. It does nothing when sccache is not installed or no sccache server is running.

| Input      | Default | Description                                                           |
| ---------- | ------- | --------------------------------------------------------------------- |
| `min-hits` | `"0"`   | Fail when compile cache hits are below this number (`0` never fails). |

| Output   | Description           |
| -------- | --------------------- |
| `hits`   | Compile cache hits.   |
| `misses` | Compile cache misses. |

## Sub-action: cache-cleanup

`azrtydxb/ant-setup-action/cache-cleanup` deletes a branch's caches from the cache feeds with [`DELETE /api/v1/feeds/{id}/cache?prefix=...`](../build-caches/index.md#branch-cleanup), repeating the call while the answer says `truncated`. It exchanges its own OIDC token (needs `id-token: write`) and never fails the workflow when Ant is unreachable unless `required: "true"`.

| Input             | Default                                       | Description                                                                                                      |
| ----------------- | --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `branch`          | `""`                                          | Branch whose caches to delete. Default: the ref of a `delete` event, else the PR head branch, else the ref name. |
| `repository`      | <span v-pre>`${{ github.repository }}`</span> | `org/repo` whose caches to delete.                                                                               |
| `host`            | the project's own instance                    | Ant host name. Always set it.                                                                                    |
| `cache-feed`      | `buildcache`                                  | Slug of the OCI cache feed.                                                                                      |
| `sccache-feed`    | `sccache`                                     | Slug of the raw cache feed.                                                                                      |
| `cache-feed-id`   | a UUID of the project's own instance          | Feed ID used when the OCI cache feed cannot be looked up by slug with the CI token. Set it to your feed's ID.    |
| `sccache-feed-id` | a UUID of the project's own instance          | Same, for the sccache feed.                                                                                      |
| `required`        | `"false"`                                     | Fail the step when the cleanup could not run.                                                                    |

| Output           | Description                                                   |
| ---------------- | ------------------------------------------------------------- |
| `tags-removed`   | Cache tags removed from the OCI cache feed.                   |
| `bytes-released` | Bytes released (metadata; the blobs are swept shortly after). |

Behaviour from the source:

- **Prefixes.** OCI: `<org>/<repo>:<branch>-` (lowercased repository, sanitized branch), which removes `<branch>-<arch>` and `<branch>-<name>-<arch>`. Because it is a prefix, cleaning `feat` also removes the caches of branches named `feat-*`; that is only cache. Raw: `<org>/<repo>/<branch>/`, which removes files only for a repository that put the branch into `SCCACHE_WEBDAV_KEY_PREFIX`; with the default per-repository prefix nothing is removed there.
- **Safety.** A deleted tag is skipped (tag builds share the default branch cache). The default branch is refused unless you pass `branch` explicitly.
- **Feed lookup.** It looks up feed IDs with `GET /api/v1/feeds?prefix=<slug>`. A CI token may be allowed to delete from a cache feed without being allowed to read it through the management API; then it falls back to `cache-feed-id` / `sccache-feed-id`, but only when the slug is the default (`buildcache` / `sccache`). With custom slugs and no readable feed, that feed is skipped with a warning.

::: warning Set the feed-ID inputs
The defaults of `host`, `cache-feed-id` and `sccache-feed-id` refer to the action author's own instance. On any other instance, always set `host`, and set both feed-ID inputs (or grant the CI principal read access to the feeds through the management API).
:::

The trust rule that matches the cleanup workflow needs `delete` on both cache feeds.

## Example workflows

### Consume npm, pip and Go through Ant

```yaml
name: ci
on: [push, pull_request]
permissions:
  id-token: write
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v4
        with: { node-version: 22 }
      - uses: actions/setup-python@v5
        with: { python-version: "3.13" }
      - uses: actions/setup-go@v5
        with: { go-version: stable }
      - id: ant
        uses: azrtydxb/ant-setup-action@<sha> # v1.1.0
        with:
          host: ant.example.com
          cargo: "false"
      - run: npm ci
      - run: pip install -r requirements.txt
      - run: go build ./...
```

Put `actions/setup-node` before the Ant step: it may write its own `.npmrc`, and the action rewrites the `registry=` line afterwards. If `configured` is `"false"` the same steps run against the public registries.

### Docker build with registry cache

```yaml
name: image
on:
  push:
    branches: [main]
  pull_request:
permissions:
  id-token: write
  contents: read
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: docker/setup-buildx-action@v4
      - id: ant
        uses: azrtydxb/ant-setup-action@<sha> # v1.1.0
        with:
          host: ant.example.com
          build-cache: "true"
          go: "false"
          npm: "false"
          cargo: "false"
          pip: "false"
          # Pull requests read the cache but do not write it:
          cache-write: ${{ github.event_name == 'pull_request' && 'false' || 'true' }}
      - uses: docker/build-push-action@v7 # v6 fails on a cache-from ref that does not exist
        with:
          context: .
          push: ${{ github.event_name != 'pull_request' }}
          tags: ant.example.com/oci-hosted/myorg/myrepo:${{ github.sha }}
          cache-from: ${{ steps.ant.outputs.cache-from }}
          cache-to: ${{ steps.ant.outputs.cache-to }}
```

Pushing the image to `oci-hosted` needs `publish` on that feed in the trust rule for this ref. The cache feed needs `read` and `publish`.

### Rust with sccache

```yaml
name: rust
on: [push, pull_request]
permissions:
  id-token: write
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    container: rust:1-bookworm
    steps:
      - uses: actions/checkout@v5
      - id: ant
        uses: azrtydxb/ant-setup-action@<sha> # v1.1.0
        with:
          host: ant.example.com
          build-cache: "true" # sccache: auto installs it because Cargo.toml exists
          go: "false"
          npm: "false"
          pip: "false"
      - run: cargo test --locked
      - uses: azrtydxb/ant-setup-action/cache-stats@<sha> # v1.1.0
        if: always()
        with:
          min-hits: "0"
```

Cargo also fetches crates through the Ant cargo feed (`cargo` defaults to `"true"`).

### Clean up branch caches

```yaml
name: cache-cleanup
on:
  delete:
  pull_request:
    types: [closed]
permissions:
  id-token: write
jobs:
  cleanup:
    if: github.event_name == 'pull_request' || github.event.ref_type == 'branch'
    runs-on: ubuntu-latest
    steps:
      - uses: azrtydxb/ant-setup-action/cache-cleanup@<sha> # v1.1.0
        with:
          host: ant.example.com
          cache-feed-id: <buildcache feed id>
          sccache-feed-id: <sccache feed id>
```

For `pull_request: closed` the branch is the PR head branch. `delete` events run on the default branch's workflow file, so the OIDC token's `ref` is the default branch: the trust rule that matches it must hold `delete` on the cache feeds.

::: warning Fork pull requests
Fork pull requests and Dependabot runs get no OIDC token. The setup action falls back to public registries; the cleanup action skips with a warning. Their branches' caches expire through the feed's idle policy instead.
:::

## Fallback: a stored token secret

Without the action (or for runners where OIDC is not available), store an Ant service token as an encrypted secret and configure the clients yourself. Issue a token for a service principal with only the scopes the workflow needs (see [Service accounts and tokens](../identity/tokens.md)) and save it as the repository or organization secret `ANT_TOKEN`.

```yaml
permissions:
  contents: read
env:
  ANT_HOST: ant.example.com
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - name: Configure package managers
        env:
          ANT_TOKEN: ${{ secrets.ANT_TOKEN }}
        run: |
          # npm / pnpm
          printf 'registry=https://%s/feeds/npm/npm/\n//%s/feeds/npm/npm/:_authToken=%s\n' \
            "$ANT_HOST" "$ANT_HOST" "$ANT_TOKEN" > ~/.npmrc
          # pip / uv
          echo "PIP_INDEX_URL=https://ci:$ANT_TOKEN@$ANT_HOST/feeds/pypi/pypi/simple/" >> "$GITHUB_ENV"
          echo "UV_DEFAULT_INDEX=https://ci:$ANT_TOKEN@$ANT_HOST/feeds/pypi/pypi/simple/" >> "$GITHUB_ENV"
          # Go
          printf 'machine %s login ci password %s\n' "$ANT_HOST" "$ANT_TOKEN" > ~/.netrc
          chmod 600 ~/.netrc
          echo "GOPROXY=https://$ANT_HOST/feeds/go/go|https://proxy.golang.org,direct" >> "$GITHUB_ENV"
      - uses: docker/login-action@v3
        with:
          registry: ant.example.com
          username: ci
          password: ${{ secrets.ANT_TOKEN }}
      - run: npm ci && pip install -r requirements.txt && go build ./...
```

Ant accepts the token as a Bearer token or as the password of HTTP Basic authentication with any username (see [Package client auth](../identity/client-auth.md)). A stored token is long-lived: rotate it as described in [Other CI systems](generic.md#token-rotation).

You can also do the OIDC exchange yourself without the action:

```yaml
- name: Ant token
  run: |
    JWT=$(curl -sS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
      "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://ant.example.com" | jq -r .value)
    ANT=$(curl -sS -X POST https://ant.example.com/api/v1/auth/github-oidc/exchange \
      -H 'Content-Type: application/json' -d "{\"token\":\"$JWT\"}" | jq -r .access_token)
    echo "::add-mask::$ANT"
    echo "ANT_TOKEN=$ANT" >> "$GITHUB_ENV"
```

Ant also accepts the GitHub OIDC JWT itself as a Bearer token or Basic password on package protocols; it exchanges it once per distinct JWT and reuses the issued token until shortly before it expires.

## Troubleshooting

| Warning in the log                                                    | Cause                                                                                                                                                                 |
| --------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `https://<host> is not answering`                                     | The unauthenticated `GET /v2/` did not return 2xx: Ant is down, unreachable from the runner, or has no anonymous-read `ANT_OCI_DEFAULT_FEED` (see the warning above). |
| `no GitHub OIDC token (add 'permissions: id-token: write'; ...)`      | Missing permission, a Dependabot run or a fork pull request.                                                                                                          |
| `OIDC exchange at https://<host> was refused (check the trust rules)` | Audience mismatch (it must be `https://<host>`), no matching enabled rule, or a rule whose scopes exceed the principal's grants.                                      |
| `docker login to <host> failed; the buildx cache will be skipped`     | Login failed after the retries; the build continues without a remote cache.                                                                                           |
| `sccache is not available; Rust builds compile without it`            | Non-Linux runner, unsupported architecture, download or checksum failure.                                                                                             |
| `refusing to clean the default branch`                                | `cache-cleanup` ran for the default branch without an explicit `branch`.                                                                                              |

::: warning Gap

- The action's reachability probe depends on an anonymous `GET /v2/`; Ant answers it with `200` only when `ANT_OCI_DEFAULT_FEED` (or the mirror map for the request) names an anonymous-read OCI feed. The repository does not document this dependency elsewhere.
- The action's `host` and `cache-feed-id` / `sccache-feed-id` defaults point at the author's own instance; there is no neutral default.
- The action comments that Ant's Go sumdb proxy returns `404` for some `/lookup` requests, which is why it sends checksum lookups straight to `sum.golang.org`. That behaviour is not covered by Ant's own documentation.
  :::
