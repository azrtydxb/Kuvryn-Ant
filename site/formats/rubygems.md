# RubyGems

The RubyGems adapter serves a gem source for `gem` and Bundler. It provides the
compact index (`names`, `versions`, `info/<gem>`), the legacy Marshal indexes
(`specs.4.8.gz`, `prerelease_specs.4.8.gz`, `quick/Marshal.4.8/*.gemspec.rz`), gem
downloads, and the `gem push` and `gem yank` API. With a remote source it can proxy
and cache rubygems.org.

Source URL:

```text
https://ant.example.com/feeds/<slug>/rubygems/
```

Implementation: [`internal/rubygems`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/rubygems/README.md).

## Support matrix

| Operation                                                        | Supported | Notes                                                                                                                                                    |
| ---------------------------------------------------------------- | --------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Consume with `gem install`                                       | Yes       | Compact index and legacy indexes; ETags, conditional requests, single byte ranges.                                                                       |
| Consume with Bundler                                             | Yes       | Through the compact index.                                                                                                                               |
| Publish (`gem push`)                                             | Yes       | `POST api/v1/gems` with the raw `.gem`. Returns `201`.                                                                                                   |
| Yank (`gem yank`)                                                | Yes       | `DELETE api/v1/gems/yank`. Removes the version through the artifact core, gated by the feed's delete policy.                                             |
| Dependency API, search, `latest_specs`, owners, API key creation | No        |                                                                                                                                                          |
| Remote (pull-through)                                            | Yes       | `info/<gem>` through the metadata cache; `.gem` checked against the sha256 in the info line and cached. See the limits on [listings](#remote-upstreams). |
| Replica                                                          | Yes       | Through the format-agnostic replication core. The adapter README lists replica closure as not yet verified.                                              |

The real clients (RubyGems 4.0.20, Bundler 4.0.20) pass push, install, Bundler
resolution, yank, rubygems.org pull-through and a cached download with the upstream
unreachable in the repository's conformance suite.

## Limits

- **Gem size and contents**: 256 MiB compressed. Metadata and checksums may expand to
  1 MiB, data to 512 MiB and 20,000 entries. YAML is limited to 20,000 nodes and depth
  24; there may be at most 256 dependencies with 16 requirement clauses each.
  Validation has a 15-second budget.
- **Safety checks.** The gemspec YAML is parsed as inert data. Aliases, duplicate
  keys, merge keys, arbitrary object tags and multiple documents are rejected. No
  gemspec code, hook or extension runs on the server. TAR entries must be clean
  relative paths; links, duplicates and unknown outer members are rejected.
  `metadata.gz` and `data.tar.gz` are required, and an optional `checksums.yaml.gz`
  must be complete and correct.
- **Not accepted:** signed gems, Ruby object-form platform metadata, and versions
  outside a bounded numeric, dot and alphanumeric grammar.
- **Immutability.** Pushing identical bytes returns `201`; different bytes for the
  same name, version and platform return `409`. Names and platforms are
  case-sensitive.
- **Index size**: index reads fail rather than truncate above 4,096 candidate
  releases or 16 MiB of metadata (20-second budget).
- **Downloads** support a single byte range. Multipart ranges return `416`.
- Four concurrent uploads per process. A push that arrives while four are already
  running gets `503`.

::: warning Gap: anonymous reads
The adapter README says anonymous reads are not implemented. The package client-auth
middleware can now pass an anonymous principal to feeds with `anonymous_read`
enabled, and the RubyGems adapter does not reject one. Anonymous `gem install`
against a RubyGems feed has not been tested.
:::

## Create the feed

In the console:

1. **Sources**, then **Create source**: a `local` source with format `rubygems` (for
   example `gems-local`), and optionally a `remote` source (for example
   `rubygems-org`) configured on its **Upstream** tab.
2. **Feeds**, then **Create feed**: slug `gems`, format `rubygems`.
3. Under **Read order**, put the local source first. Under **Publish policy**, choose
   the local source as the default.

Through the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: gems-local-1' \
  -d '{"name":"gems-local","kind":"local","format":"rubygems"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: rubygems-org-1' \
  -d '{"name":"rubygems-org","kind":"remote","format":"rubygems"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: rubygems-org-cfg-1' -d '{
  "origin":"https://rubygems.org","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: gems-feed-1' -d @- <<JSON
{"slug":"gems","format":"rubygems","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The first source in the [read order](../concepts/resolution.md) that has a gem owns
it, even if it lacks the requested version. A gem pushed to the local source is never
fetched from rubygems.org. See [Feeds and sources](../concepts/feeds-and-sources.md)
and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The remote source origin is a compact-index gem server.

| Upstream                                       | `origin`               | `redirect_origins`                 | Notes                                                                                                                                                               |
| ---------------------------------------------- | ---------------------- | ---------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| rubygems.org                                   | `https://rubygems.org` | none needed in the conformance run | `gem install rake -v 13.2.1` through the feed is covered by the conformance suite. If rubygems.org starts redirecting gem downloads to another host, add that host. |
| Private gem server (Gemfury, Artifactory, ...) | its source root URL    | any download host it redirects to  | It must serve the compact index (`info/<gem>`) and `gems/<file>.gem`. Store the credential on the source.                                                           |

How remote content is handled:

- `info/<gem>` is read through the metadata cache, validated line by line and served
  as received. It carries no URLs.
- A `.gem` is fetched from `gems/<file>.gem` on the origin on first download,
  checked against the sha256 `checksum` in its info line, and cached together with a
  metadata record rebuilt from that line. From then on it shows up in the feed's
  local indexes and is still served during an outage.
- Generated gemspecs (`quick/Marshal.4.8/...`) of remote gems are fetched from the
  origin.
- **`versions`, `names`, `specs.4.8.gz` and `prerelease_specs.4.8.gz` are feed-local
  views.** They list local gems and remote gems that have already been cached, not
  the whole upstream registry, which is too large for the metadata limit. Upstream
  listings are never merged with local content.

::: warning Gap: Bundler with a remote source
The conformance suite tests pull-through with `gem install`, and tests Bundler only
against local gems. Bundler reads `versions` before requesting `info/<gem>` files.
Because `versions` on a feed lists only cached remote gems, Bundler's resolution of
gems that have never been cached through the feed has not been verified.
:::

For an authenticated upstream, set the credential with
`PUT /api/v1/sources/{id}/remote-credential`, for example
`{"authorization":"Basic <base64 user:token>"}`. It is only ever sent to the origin.

## Client configuration

The adapter accepts a token in three forms:

- HTTP Basic auth with any non-empty username and a service token or personal access
  token as the password (what `gem` and Bundler send when the source URL or Bundler
  config has credentials);
- `Authorization: Bearer <token>`;
- the raw token as the whole `Authorization` value (what `gem push` and `gem yank`
  send as the API key).

An unauthenticated request gets `401` with `WWW-Authenticate: Basic realm="RubyGems"`.

Any credential type works as the raw API key: a service token (`ant_...`), a personal
access token (`antp_...`) or a GitHub Actions JWT. The raw value goes through the same
resolution as `Bearer`, so scoping, audit, rate limits and the feed's client-auth policy
apply, and it is never treated as a password login. There is no RubyGems.org-style
sign-in or key creation API. Create tokens as described in
[Service accounts and tokens](../identity/tokens.md).

### Consume with gem

```sh
gem sources --add "https://token:$ANT_TOKEN@ant.example.com/feeds/gems/rubygems/"
gem sources --remove https://rubygems.org/
gem install mygem
```

Or for a single command:

```sh
gem install mygem --clear-sources \
  --source "https://token:$ANT_TOKEN@ant.example.com/feeds/gems/rubygems/"
```

### Consume with Bundler

`Gemfile`:

```ruby
source "https://ant.example.com/feeds/gems/rubygems/"

gem "mygem", "~> 0.1"
```

Keep the credential out of the `Gemfile`. Bundler looks up credentials per host:

::: code-group

```sh [bundle config]
bundle config set --global ant.example.com "token:$ANT_TOKEN"
```

```sh [environment]
# Bundler maps the host to BUNDLE_<HOST>: dots become "__", dashes "___", uppercase.
export BUNDLE_ANT__EXAMPLE__COM="token:$ANT_TOKEN"
```

:::

A credential can also be scoped to the full source URL
(`bundle config set --global https://ant.example.com/feeds/gems/rubygems/ "token:$ANT_TOKEN"`)
when several feeds on the same host need different tokens.

### Publish and yank

`gem push` needs `--host` set to the feed URL. The API key is a service token or personal access token.

::: code-group

```sh [environment variable]
GEM_HOST_API_KEY="$ANT_TOKEN" gem push mygem-0.1.0.gem \
  --host https://ant.example.com/feeds/gems/rubygems
```

```yaml [~/.gem/credentials]
---
:ant: <TOKEN>
```

:::

With a named key in the credentials file (mode `0600`), select it with `--key`:

```sh
gem push mygem-0.1.0.gem --key ant --host https://ant.example.com/feeds/gems/rubygems
```

To stop accidental pushes to rubygems.org, set the gemspec's
`allowed_push_host` metadata:

```ruby
spec.metadata["allowed_push_host"] = "https://ant.example.com/feeds/gems/rubygems"
```

Yank a version (it is deleted through the artifact core, subject to the feed's delete
policy):

```sh
GEM_HOST_API_KEY="$ANT_TOKEN" gem yank mygem -v 0.1.1 \
  --host https://ant.example.com/feeds/gems/rubygems
```

A trailing slash in `--host` makes `gem push` send a double slash. The adapter accepts
that.

### GitHub Actions

```yaml
- uses: ruby/setup-ruby@v1
  with:
    ruby-version: "3.4"
- run: |
    bundle install
    gem build mygem.gemspec
    gem push mygem-*.gem --host https://ant.example.com/feeds/gems/rubygems
  env:
    BUNDLE_ANT__EXAMPLE__COM: ci:${{ env.ANT_TOKEN }}
    GEM_HOST_API_KEY: ${{ env.ANT_TOKEN }}
```

`ANT_TOKEN` can come from a repository secret or from the GitHub OIDC exchange, which
returns a service token. See [GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                                                 | Cause                                                                                                                                 |
| ------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `401`                                                   | No credentials for the host, or the token is invalid. For Bundler, check the `BUNDLE_<HOST>` name.                                    |
| `403`                                                   | The token lacks read or publish permission, or the feed's client-auth policy does not accept that credential type.                    |
| `400` on push                                           | The gem failed validation: signed gem, unsupported platform metadata or version, unsafe archive, missing `metadata.gz`/`data.tar.gz`. |
| `409` on push                                           | That name, version and platform exist with different bytes.                                                                           |
| `405`                                                   | Unsupported API call (owners, dependency API, search, ...).                                                                           |
| `416`                                                   | A multipart byte range was requested.                                                                                                 |
| `503` on push                                           | Four uploads are already in progress; retry.                                                                                          |
| `503` on install of a public gem                        | The upstream failed and nothing is cached for that gem.                                                                               |
| `429` with `Retry-After`                                | Too many failed authentications from this client.                                                                                     |
| A public gem is missing from `gem search` or `versions` | Expected: listings are feed-local and include only cached remote gems.                                                                |
| `X-Kuvryn-Cache: stale` / `offline`                     | `info/<gem>` came from cache past its TTL, or from locally cached versions, because upstream failed.                                  |

To see which source owns a gem, call
`GET /api/v1/feeds/{id}/resolve?coordinate=<gem name>`
(see [Read order and resolve explain](../concepts/resolution.md)).
