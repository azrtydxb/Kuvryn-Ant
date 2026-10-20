# npm, pnpm and Yarn

The npm adapter implements the npm registry protocol used by npm, pnpm, Yarn classic
and Yarn Berry: packuments (full and abbreviated), version manifests, tarballs,
publish, dist-tags, unpublish and the audit endpoints. A feed can combine a local
source for your own packages with a remote source that proxies `registry.npmjs.org`.

Endpoint:

```text
https://ant.example.com/feeds/<slug>/npm/
```

Scoped packages work as `@scope/name` or `@scope%2fname`.

Implementation: [`internal/npmregistry`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/npmregistry/README.md).

## Support matrix

| Operation                                                                                     | Supported                    | Notes                                                                                  |
| --------------------------------------------------------------------------------------------- | ---------------------------- | -------------------------------------------------------------------------------------- |
| Install (`npm install`, `pnpm add`, `yarn add`)                                               | Yes                          | Packument, version manifest and tarball reads; `ETag` / `If-None-Match` on packuments. |
| Publish (`npm publish`, `pnpm publish`, `yarn npm publish`)                                   | Yes, with tag limits         | One version per publish; the publish tag must not exist yet (see [Limits](#limits)).   |
| `npm dist-tag add / rm / ls`                                                                  | Yes                          | Moves or removes tag pointers; requires the publish permission.                        |
| `npm unpublish` (version or whole package)                                                    | Yes                          | Gated by the feed's delete policy.                                                     |
| `npm deprecate`                                                                               | Accepted, no effect          | Returns success; the deprecation message is not stored.                                |
| `npm audit`, `pnpm audit`                                                                     | Yes, through a remote source | Forwarded to the feed's remote npm source.                                             |
| `npm login` / `adduser`, `whoami`, `ping`, `search`, `access`, `org`, `team`, `token`, `hook` | No                           | Configure the token in `.npmrc` instead of `npm login`.                                |
| `npm publish --provenance`                                                                    | No                           | Provenance attachments are not accepted.                                               |
| Remote (pull-through)                                                                         | Yes                          | Packuments rewritten to the feed, tarballs cached on first download.                   |
| Replica                                                                                       | Yes                          | Through the format-agnostic replication core.                                          |

Real-client conformance (2026-10-19): `pnpm publish`, `pnpm add`, publish with
`--tag next`, `npm dist-tag add/ls/rm`, `npm unpublish`, `npm audit` and `pnpm audit`
through a remote feed, pull-through from `registry.npmjs.org` (miss, then tarball hit
with the upstream unreachable) and auth denial. A second publish with the default
`latest` tag is recorded as a documented limitation.

## Limits

- **Publish tags are immutable at publish time.** A publish carries exactly one
  dist-tag (`latest` by default). If that tag already exists on the package, the
  publish returns `409` and nothing becomes visible, so `npm publish` of a second
  version with the default tag fails. Publish with a tag that does not exist yet, then
  move `latest` with `npm dist-tag`:

  ```sh
  npm publish --tag release-1-2-0
  npm dist-tag add @myorg/mypkg@1.2.0 latest
  npm dist-tag rm @myorg/mypkg release-1-2-0
  ```

  Tag names may not be valid SemVer versions and may not start with a digit.

- **One version, one tarball per publish**, strict SemVer, lowercase names. The
  tarball's `package.json` must match the published name and version, and the
  SHA-1 `shasum` and SHA-512 `integrity` must match the bytes.
- **Size:** the publish JSON is capped at 64 MiB, so the tarball must stay below
  roughly 48 MiB after base64 encoding. Archives may expand to at most 256 MiB and
  10,000 entries. Version metadata at most 2 MiB.
- **Concurrency:** two concurrent publishes per process; a third gets
  `503 publish capacity exhausted`.
- **Packuments:** at most 10,000 versions and 16 MiB of stored manifests for a
  locally published package. A remote packument larger than the metadata cache bound
  is streamed from upstream uncached on every request.
- **Immutability:** republishing an existing version with different bytes returns
  `409`; an identical retry succeeds.
- **Public URL:** tarball URLs in packuments are built from `ANT_PUBLIC_URL`; the
  host or forwarding headers of the request are never used. On a non-loopback
  listener the server refuses to start without it.
- **Remote tarballs** are always fetched from `<name>/-/<basename>-<version>.tgz` on
  the source origin, not from the `dist.tarball` URL in the upstream document.
  Upstream registries that host tarballs under a different path or host cannot be
  proxied.

::: warning Gap

Not verified by a real-client run: Yarn classic and Yarn Berry (the configuration
below follows their documented settings and the same protocol), `npm publish` itself
(publishing was exercised with `pnpm publish`), and the packument of a remote package
while the upstream is down (cached tarballs are verified to stay available; whether
the packument is still served from the local cache is not).

:::

## Create the feed

### Console

1. **Sources**: **Create source** twice, format `npm`: a `local` source (for example
   `npm-local`) and a `remote` source (for example `npmjs`).
2. On the remote source's **Upstream** tab set the origin to
   `https://registry.npmjs.org`.
3. **Feeds**: **Create feed** with slug `npm`, format `npm`. Attach `npm-local` first
   and `npmjs` second, and set `npm-local` as the publish policy's default source.

With this order a package published locally is always served from the local source,
and a name that exists locally is never completed from npmjs (owner-first
resolution; see [Read order and resolve explain](../concepts/resolution.md)).

### API

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: npm-local-1' \
  -d '{"name":"npm-local","kind":"local","format":"npm"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: npm-remote-1' \
  -d '{"name":"npmjs","kind":"remote","format":"npm"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: npm-remote-cfg-1' -d '{
  "origin":"https://registry.npmjs.org","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: npm-feed-1' -d @- <<JSON
{"slug":"npm","format":"npm","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

## Remote upstreams

| Upstream            | `origin`                     | `redirect_origins` | Status   |
| ------------------- | ---------------------------- | ------------------ | -------- |
| npm public registry | `https://registry.npmjs.org` | none               | Verified |

- The packument is fetched from `<origin>/<name>` (abbreviated or full, following the
  client's `Accept` header) through the metadata cache and streamed back with every
  `dist.tarball` rewritten to the feed. `dist-tags`, `integrity`, `time` and other
  fields pass through unchanged.
- Rewritten tarball URLs carry a `source` parameter that pins the download to the
  remote source that supplied the packument.
- Tarballs are cached under the remote source on first download.
- For a private upstream registry that needs a token, store it as the source
  credential (`{"authorization":"Bearer <upstream-token>"}`); it is only sent to the
  origin.

### Audit

`npm audit` and `pnpm audit` call
`POST /-/npm/v1/security/advisories/bulk`, `.../audits/quick` or `.../audits`. Ant
forwards these to the feed's remote npm source (request body at most 1 MiB, 20 s
upstream timeout, answers cached in memory for 2 minutes). A feed without a remote
source, or an upstream without the endpoint, answers `404` ("audit unavailable"); an
unreachable upstream answers `502`.

## Client configuration

### Authentication

The npm adapter takes **Bearer** credentials. Configure the token with `_authToken`
(npm, pnpm, Yarn classic) or `npmAuthToken` (Yarn Berry), which send
`Authorization: Bearer`. Service tokens (`ant_...`), personal access tokens
(`antp_...`) and GitHub OIDC tokens all work this way. Do not use the legacy `_auth`
(Basic) setting with a service token: the adapter does not translate Basic for
service tokens. A request without credentials gets `401` with
`WWW-Authenticate: Bearer realm="npm"`, unless the feed allows anonymous reads.

The auth line is keyed by the registry URL without the scheme; keep the trailing
slash identical to the registry URL.

::: code-group

```ini [npm / pnpm (.npmrc)]
registry=https://ant.example.com/feeds/npm/npm/
//ant.example.com/feeds/npm/npm/:_authToken=${ANT_TOKEN}
```

```ini [scoped only (.npmrc)]
# only @myorg packages come from Ant, everything else from the default registry
@myorg:registry=https://ant.example.com/feeds/npm/npm/
//ant.example.com/feeds/npm/npm/:_authToken=${ANT_TOKEN}
```

```ini [Yarn classic (.npmrc)]
# Yarn 1 reads .npmrc for the registry and _authToken
registry=https://ant.example.com/feeds/npm/npm/
//ant.example.com/feeds/npm/npm/:_authToken=${ANT_TOKEN}
always-auth=true
```

```yaml [Yarn Berry (.yarnrc.yml)]
npmRegistryServer: "https://ant.example.com/feeds/npm/npm/"
npmAuthToken: "${ANT_TOKEN}"
npmAlwaysAuth: true
# or per scope:
# npmScopes:
#   myorg:
#     npmRegistryServer: "https://ant.example.com/feeds/npm/npm/"
#     npmAuthToken: "${ANT_TOKEN}"
#     npmAlwaysAuth: true
```

:::

`${ANT_TOKEN}` is expanded from the environment by npm, pnpm and Yarn, so the file can
be committed without the secret. For a private CA set `cafile=/path/ca.pem` in
`.npmrc` or `NODE_EXTRA_CA_CERTS=/path/ca.pem`.

### Install

```sh
npm install lodash
pnpm add @myorg/mypkg
yarn add left-pad
```

### Publish

The package lands in the feed's publish-policy target (a local source). To pin the
target registry in the package itself:

```json
{
  "name": "@myorg/mypkg",
  "version": "1.0.0",
  "publishConfig": {
    "registry": "https://ant.example.com/feeds/npm/npm/"
  }
}
```

::: code-group

```sh [npm]
npm publish                      # first version: creates the latest tag
npm publish --tag next           # later versions: use a tag that does not exist yet
npm dist-tag add @myorg/mypkg@1.1.0 latest
```

```sh [pnpm]
pnpm publish --no-git-checks
pnpm publish --no-git-checks --tag next
```

```sh [Yarn Berry]
yarn npm publish
yarn npm publish --tag next
yarn npm tag add @myorg/mypkg@1.1.0 latest
```

:::

Unpublish:

```sh
npm unpublish @myorg/mypkg@1.1.0
npm unpublish @myorg/mypkg --force   # all versions
```

### GitHub Actions

`actions/setup-node` writes an `.npmrc` with the registry and an `_authToken` line
that reads `NODE_AUTH_TOKEN`:

```yaml
- uses: actions/setup-node@v4
  with:
    node-version: 22
    registry-url: https://ant.example.com/feeds/npm/npm/
    scope: "@myorg" # optional: only route this scope to Ant
- run: npm ci
  env:
    NODE_AUTH_TOKEN: ${{ secrets.ANT_TOKEN }}
- run: npm publish --tag ci-${{ github.run_number }}
  env:
    NODE_AUTH_TOKEN: ${{ secrets.ANT_TOKEN }}
```

To use the workflow's OIDC identity instead of a stored secret, exchange the OIDC
token for a short-lived Ant token in an earlier step and pass it as
`NODE_AUTH_TOKEN`; see [GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                                                    | Cause                                                                                                                                                         |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` with `WWW-Authenticate: Bearer realm="npm"`          | No `Authorization` header: the `_authToken` line does not match the registry URL (check host, path and trailing slash), or `ANT_TOKEN` is empty.              |
| `401` although a token is configured                       | The token is sent as Basic (`_auth`, or a username/password pair) and is a service token; switch to `_authToken`.                                             |
| `403`                                                      | The principal lacks read or publish on the feed, or the feed's client-auth policy does not accept that token type.                                            |
| `409` on publish                                           | The publish tag (by default `latest`) already exists, or the version exists with different bytes. Publish with a new tag and move it with `npm dist-tag`.     |
| `409 package owner changed` on a tarball                   | The package's owning source changed after the packument was fetched; re-run the install.                                                                      |
| `409 package is not owned by a local source` on `dist-tag` | Tags can only be changed on packages published to a local source.                                                                                             |
| `400` on publish                                           | Name, version, `shasum`/`integrity`, archive or `package.json` mismatch, more than one attachment (for example `--provenance`), or a SemVer-looking tag name. |
| `413` on publish                                           | The publish JSON exceeds 64 MiB.                                                                                                                              |
| `503 publish capacity exhausted`                           | Two publishes already running; retry.                                                                                                                         |
| `503` on install                                           | Upstream error on a remote source (errors do not fall through to the next source); check `GET /api/v1/sources/{id}/health`.                                   |
| `404 audit unavailable`                                    | The feed has no remote npm source, or the upstream lacks the audit endpoint.                                                                                  |
| `X-Kuvryn-Cache: stale`                                    | The packument was served from the metadata cache after its TTL because the upstream failed.                                                                   |
