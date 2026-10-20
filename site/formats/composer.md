# Composer

The Composer adapter serves a Composer 2 repository (`packages.json` with a
`metadata-url`, `p2/<vendor>/<package>.json` metadata and ZIP dists) for PHP
projects. Private packages are uploaded through an ingestion endpoint, because
Composer has no publish command. With a remote source the adapter proxies and caches
Packagist, serving dists through the feed.

Repository URL (the `url` of a `composer` repository):

```text
https://ant.example.com/feeds/<slug>/composer
```

Routes under that URL:

| Route                                           | Method      | Purpose                                                        |
| ----------------------------------------------- | ----------- | -------------------------------------------------------------- |
| `packages.json`                                 | GET, HEAD   | Repository root; points `metadata-url` at `p2/%package%.json`. |
| `p2/<vendor>/<package>.json`                    | GET, HEAD   | Version metadata, unminified.                                  |
| `p2/<vendor>/<package>~dev.json`                | GET, HEAD   | Dev versions. Always empty for local packages.                 |
| `dist/<vendor>/<package>/<version>/archive.zip` | GET, HEAD   | The ZIP dist.                                                  |
| `ingest/<vendor>/<package>/<version>`           | PUT, DELETE | Upload or delete a version (Kuvryn Ant API).                   |

Dist URLs in the metadata are absolute and built from `ANT_PUBLIC_URL`, never from
the request's `Host` header. Without a public URL every route returns `503`. See
[Configuration](../reference/configuration.md).

Implementation: [`internal/composer`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/composer/README.md).

## Support matrix

| Operation                                   | Supported          | Notes                                                                                                     |
| ------------------------------------------- | ------------------ | --------------------------------------------------------------------------------------------------------- |
| Consume (`composer install/update/require`) | Yes                | Composer 2 metadata and ZIP dists.                                                                        |
| Publish                                     | Yes (admin upload) | `PUT ingest/<vendor>/<package>/<version>` with the raw ZIP body.                                          |
| Delete                                      | Yes                | `DELETE ingest/<vendor>/<package>/<version>`, gated by the feed's delete policy.                          |
| Dev versions, branches, aliases             | No                 | Local packages are stable `major.minor.patch` only.                                                       |
| Remote (pull-through)                       | Yes                | Upstream `p2` metadata through the metadata cache; dists fetched from the upstream `dist.url` and cached. |
| Replica                                     | Yes                | Through the format-agnostic replication core. The adapter README lists replication as not yet verified.   |

The real Composer client (PHP 8.5) passes ingestion, `composer require`, update to a
newly ingested version, delete, Packagist pull-through and a cached dist with the
upstream unreachable in the repository's conformance suite.

## Limits

These apply to locally ingested packages.

- **Names** must be lowercase `vendor/package` (`[a-z0-9]` segments joined by `_`,
  `.` or `-`). **Versions** must be canonical stable `major.minor.patch`
  (for example `1.4.0`). No `v` prefix, prerelease or branch.
- **The ZIP** must contain exactly one `composer.json`, either at the root or inside a
  single wrapper directory that holds the whole archive. Its `name` and `version`
  must match the URL, so **`version` is required in `composer.json`**.
- **Only these `composer.json` fields are accepted:** `name`, `version`, `type` (only
  `library`), `description`, `require`, `require-dev`, `autoload` (PSR-0 and PSR-4
  maps only) and `source`. **Any other field is rejected rather than stripped**:
  `license`, `authors`, `keywords`, `homepage`, `scripts`, `config`, `extra`,
  `repositories`, a user-supplied `dist`, `classmap`/`files` autoloading, and so on.
  See [Publish](#publish) for a way to produce a compliant manifest.
- **Constraints** in `require` may use numeric or wildcard versions, comparison
  operators, `^`, `~`, comma or space conjunction and `||`. Branch names, stability
  flags (`@dev`), aliases and hyphen ranges are rejected. Dependency keys may be
  package names, `php`, `ext-*` or `lib-*`. Dependencies do not need to exist in the
  feed.
- **`source`**, if present, must be an HTTPS git URL without credentials, query or
  fragment. It is kept as metadata only and never fetched by the server.
- **Archive bounds**: 128 MiB compressed, 512 MiB expanded, 10,000 entries, 8 MiB
  central directory, 1 KiB names, 32 path levels, 1 MiB `composer.json`. ZIP64,
  symlinks, special files, path traversal, case-insensitive duplicates and excessive
  compression ratios are rejected.
- **Metadata listing**: at most 1,024 versions and 8 MiB of metadata per package.
- **Immutability.** An identical retry returns `201`; different content for an
  existing version returns `409`.
- Four concurrent uploads per process. An upload that arrives while four are running
  gets `503`. No conditional requests or byte ranges are supported on reads.

::: warning Gap: anonymous reads
The adapter README says anonymous access is not implemented. The package client-auth
middleware can now pass an anonymous principal to feeds with `anonymous_read`
enabled, and the Composer adapter does not reject one. Anonymous Composer access has
not been tested.
:::

## Create the feed

In the console:

1. **Sources**, then **Create source**: a `local` source with format `composer` (for
   example `composer-local`), and optionally a `remote` source (for example
   `packagist`) configured on its **Upstream** tab as described in
   [Remote upstreams](#remote-upstreams).
2. **Feeds**, then **Create feed**: slug `composer`, format `composer`.
3. Under **Read order**, put the local source first. Under **Publish policy**, choose
   the local source as the default.

Through the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: composer-local-1' \
  -d '{"name":"composer-local","kind":"local","format":"composer"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: packagist-1' \
  -d '{"name":"packagist","kind":"remote","format":"composer"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: packagist-cfg-1' -d '{
  "origin":"https://repo.packagist.org",
  "redirect_origins":["https://api.github.com","https://codeload.github.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: composer-feed-1' -d @- <<JSON
{"slug":"composer","format":"composer","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The vendor part of `vendor/package` is the namespace used for authorization. The
first source in the [read order](../concepts/resolution.md) that has a package owns
it, and a missing version never falls through to another source, so a private
`myorg/mylib` cannot be replaced by a public package of the same name. See
[Feeds and sources](../concepts/feeds-and-sources.md),
[Namespaces and scopes](../concepts/namespaces.md) and
[Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The remote source origin is the root of a Composer 2 repository. Ant reads the
upstream `packages.json` for its `metadata-url` template (default
`/p2/%package%.json`).

- `p2/<vendor>/<package>.json` and the `~dev` file are read through the metadata
  cache, expanded from Composer's minified form, and re-rendered for the client:
  **`source` is removed** (a VCS URL would let clients bypass the feed) and every
  `dist.url` points at the feed's `dist/<vendor>/<package>/<version>/archive.zip`.
- When a client requests that dist, Ant looks up the upstream `dist.url` again in the
  cached metadata (the client never chooses the upstream URL), downloads it, checks
  that it is a well-formed ZIP within the size limit, checks the SHA-1 `shasum` if
  upstream states one, and caches it. Packagist publishes no `shasum`, so for
  Packagist dists only the ZIP structure and size are checked.
- Each cached dist keeps its version entry, so cached versions of a remote package
  stay installable during an upstream outage.
- Dists usually live on another host. **Every host a dist URL points to, or redirects
  to, must be in `redirect_origins`.** The source credential is never sent there.

| Upstream                             | `origin`                     | `redirect_origins`                                                              | Notes                                                                                                                                                    |
| ------------------------------------ | ---------------------------- | ------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Packagist                            | `https://repo.packagist.org` | `["https://api.github.com","https://codeload.github.com"]`                      | GitHub-hosted packages have dist URLs on `api.github.com`, which redirects to `codeload.github.com`. Covered by the conformance suite (`psr/log 3.0.0`). |
| Packagist, packages hosted elsewhere | same                         | add the dist hosts, for example `https://gitlab.com` or `https://bitbucket.org` | Not tested. Look at the `dist.url` values in the package's metadata to find the hosts.                                                                   |
| Private Packagist or Satis           | its repository root          | its dist host(s)                                                                | Store the credential on the source with `PUT /api/v1/sources/{id}/remote-credential`.                                                                    |

::: warning Gap
Because `source` is stripped, a remote package version that has no ZIP `dist` cannot
be installed through the feed. Dist hosts other than GitHub's have not been tested.
:::

## Client configuration

Clients authenticate with HTTP Basic auth (any non-empty username, and a service
token or personal access token as the password) or with a bearer token. An
unauthenticated request gets `401`. See
[Service accounts and tokens](../identity/tokens.md) and
[Package client auth](../identity/client-auth.md).

### composer.json

```json
{
  "repositories": [
    {
      "type": "composer",
      "url": "https://ant.example.com/feeds/composer/composer"
    },
    { "packagist.org": false }
  ],
  "require": {
    "myorg/mylib": "^1.4"
  }
}
```

`{"packagist.org": false}` turns off the built-in Packagist repository, so public
packages also come through the feed (add a Packagist remote source to the feed for
that). Leave it out if the feed should only serve private packages and Composer
should reach Packagist directly.

### Credentials

Put credentials in `auth.json` next to `composer.json`, or in the global
`$COMPOSER_HOME/auth.json`. Do not commit them.

::: code-group

```json [auth.json (http-basic)]
{
  "http-basic": {
    "ant.example.com": {
      "username": "token",
      "password": "<TOKEN>"
    }
  }
}
```

```json [auth.json (bearer)]
{
  "bearer": {
    "ant.example.com": "<TOKEN>"
  }
}
```

```sh [composer config]
composer config --global http-basic.ant.example.com token "$ANT_TOKEN"
```

```sh [environment]
export COMPOSER_AUTH="{\"http-basic\":{\"ant.example.com\":{\"username\":\"token\",\"password\":\"$ANT_TOKEN\"}}}"
```

:::

Composer sends `auth.json` credentials to every URL on the host, including the dist
URLs Ant generates.

### Publish

Upload a version as the raw ZIP request body. The `X-Checksum-Sha256` header is
optional; if present, it must be the lowercase hex SHA-256 of the ZIP. The token needs
publish permission on the feed.

Because only a few `composer.json` fields are accepted, build the archive from a
cleaned copy of the manifest. For example, from a git checkout of version `1.4.0`:

```sh
VER=1.4.0
rm -rf build && mkdir build
git archive HEAD | tar -x -C build
# Keep only accepted fields and add the version (autoload must be PSR-0/PSR-4 only).
jq --arg v "$VER" '{name, version: $v, description, type, require,
  "require-dev": .["require-dev"], autoload} | with_entries(select(.value != null))' \
  composer.json > build/composer.json
(cd build && zip -qr ../mylib-$VER.zip .)

curl -fsS -X PUT --user "token:$ANT_TOKEN" \
  -H "X-Checksum-Sha256: $(sha256sum mylib-$VER.zip | cut -d' ' -f1)" \
  --data-binary @mylib-$VER.zip \
  https://ant.example.com/feeds/composer/composer/ingest/myorg/mylib/$VER
```

`201` means the version was ingested or an identical upload already existed. `409`
means the version exists with different content.

Delete a version:

```sh
curl -fsS -X DELETE --user "token:$ANT_TOKEN" \
  https://ant.example.com/feeds/composer/composer/ingest/myorg/mylib/1.4.0
```

### GitHub Actions

```yaml
- uses: shivammathur/setup-php@v2
  with:
    php-version: "8.4"
    tools: composer:v2
- run: composer install --no-interaction
  env:
    COMPOSER_AUTH: '{"http-basic":{"ant.example.com":{"username":"ci","password":"${{ env.ANT_TOKEN }}"}}}'
```

`ANT_TOKEN` can come from a repository secret or from the GitHub OIDC exchange. See
[GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                                  | Cause                                                                                                                                                                                                                                  |
| ---------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` / Composer prompts for credentials | No `auth.json` entry for `ant.example.com`, or the token is invalid.                                                                                                                                                                   |
| `403`                                    | The token lacks read or publish permission (the vendor namespace is part of authorization), or the feed's client-auth policy does not accept that credential type.                                                                     |
| `404` on `p2/<vendor>/<package>.json`    | No source in the feed has the package, or the name is not lowercase.                                                                                                                                                                   |
| `400` on ingest                          | `composer.json` has a field outside the accepted list, is missing `version`, its name or version does not match the URL, `type` is not `library`, a constraint uses unsupported syntax, or the ZIP is unsafe or has several manifests. |
| `400` with a checksum header             | `X-Checksum-Sha256` does not match the body.                                                                                                                                                                                           |
| `409` on ingest                          | The version exists with different content.                                                                                                                                                                                             |
| `503` on every request                   | `ANT_PUBLIC_URL` is not set on the instance.                                                                                                                                                                                           |
| `503` on a dist download                 | The upstream dist host failed or is not in `redirect_origins`, and the dist is not cached.                                                                                                                                             |
| `429` with `Retry-After`                 | Too many failed authentications from this client.                                                                                                                                                                                      |
| `X-Kuvryn-Cache: stale` / `offline`      | The package metadata came from cache past its TTL, or from locally cached versions, because upstream failed.                                                                                                                           |

`composer -vvv` prints every URL Composer requests. To see which source owns a
package, call
`GET /api/v1/feeds/{id}/resolve?coordinate=<vendor>/<package>&namespace=<vendor>`
(see [Read order and resolve explain](../concepts/resolution.md)).
