# Cargo

The Cargo adapter is a [sparse registry](https://doc.rust-lang.org/cargo/reference/registry-index.html#sparse-protocol)
implementing the parts of the
[registry web API](https://doc.rust-lang.org/cargo/reference/registry-web-api.html)
needed for `cargo publish` and `cargo yank`. It hosts private crates, and with a
remote source it can be a caching proxy for crates.io, including as a replacement
for crates.io itself.

Index URL (as Cargo writes it):

```text
sparse+https://ant.example.com/feeds/<slug>/cargo/
```

The feed's `config.json` advertises `dl` and `api` pointing back at the feed, and
`"auth-required": true`, so Cargo sends the token on every request, downloads
included.

Implementation: [`internal/cargo`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/cargo/README.md).

## Support matrix

| Operation                              | Supported | Notes                                                                                                                                                                           |
| -------------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Consume (`cargo build`, `cargo fetch`) | Yes       | Sparse index files and `api/v1/crates/<name>/<version>/download`.                                                                                                               |
| Publish (`cargo publish`)              | Yes       | `PUT api/v1/crates/new`.                                                                                                                                                        |
| Yank / unyank                          | Yes       | `cargo yank` and `cargo yank --undo`. Implemented as unlist/relist in the artifact core; yanked versions stay downloadable by exact version and are marked yanked in the index. |
| Owners, search, login API              | No        | Return `405`.                                                                                                                                                                   |
| Git index protocol                     | No        | Sparse only.                                                                                                                                                                    |
| Remote (pull-through)                  | Yes       | Index files come through the metadata cache; `.crate` files are checked against `cksum` and cached.                                                                             |
| Replica                                | Yes       | Through the format-agnostic replication core. The adapter README does not claim replication certification.                                                                      |

The real Cargo client (1.99.1) passes publish, consume, yank, unyank, crates.io
pull-through and an offline cached fetch in the repository's conformance suite.

## Limits

- **Crate names for publishing** must be lowercase ASCII letters, digits and hyphens,
  start with a letter, and be at most 64 characters. Uppercase letters and underscores
  are rejected, so that two spellings can never refer to different packages. This is
  stricter than crates.io. **Read** routes (used for remote crates) accept
  crates.io spellings with uppercase letters and underscores.
- **Upload format** is the documented little-endian, length-prefixed JSON plus
  archive. The JSON metadata and `Cargo.toml` are limited to 1 MiB each. Archive
  expansion is limited to 1000 times the compressed size and 100,000 entries. Archives
  are never extracted on the server; path traversal, links, duplicate paths, a missing
  manifest, or a name or version that does not match the metadata are rejected.
- The packaged `Cargo.toml` must carry the name and version itself.
  `cargo publish` normalizes workspace inheritance when packaging, so this only
  affects hand-built uploads.
- **Version uniqueness** ignores build metadata (`1.0.0+a` and `1.0.0+b` are the same
  version). Different bytes for an existing version return `409`.
- **Index size**: at most 10,000 versions and 16 MiB per crate index file.
- Four concurrent uploads per process. Transfers have a 30-second idle deadline.
- The `dl` and `api` URLs in `config.json` are built from `ANT_PUBLIC_URL`. If the
  instance has no public URL, `config.json` returns `503`. See
  [Configuration](../reference/configuration.md).

## Create the feed

In the console:

1. **Sources**, then **Create source**: a `local` source with format `cargo` (for
   example `cargo-local`). To proxy crates.io, also create a `remote` source (for
   example `crates-io`) and set its **Upstream** tab as described in
   [Remote upstreams](#remote-upstreams).
2. **Feeds**, then **Create feed**: slug `cargo`, format `cargo`.
3. Under **Read order**, put the local source first. Under **Publish policy**, choose
   the local source as the default.

Through the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: cargo-local-1' \
  -d '{"name":"cargo-local","kind":"local","format":"cargo"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: crates-io-1' \
  -d '{"name":"crates-io","kind":"remote","format":"cargo"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: crates-io-cfg-1' -d '{
  "origin":"https://index.crates.io",
  "redirect_origins":["https://static.crates.io"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: cargo-feed-1' -d @- <<JSON
{"slug":"cargo","format":"cargo","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The first source in the [read order](../concepts/resolution.md) that has a crate
owns it. Once you publish `mycrate` locally, crates.io is never asked for `mycrate`,
which protects against dependency confusion. Index files from different sources are
never merged. See [Feeds and sources](../concepts/feeds-and-sources.md) and
[Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The remote source origin is the host of the upstream sparse index, the place that
serves `config.json` and index files such as `se/rd/serde`.

- **Index files** are fetched through the metadata cache, validated line by line
  (name, semver version, sha256 `cksum`) and served as received. Index lines carry no
  registry URLs, and the client downloads through the feed because the feed's own
  `config.json` points `dl` at the feed.
- **Crate archives** are fetched from the `dl` template in the upstream
  `config.json` (`{crate}`, `{version}`, `{prefix}`, `{lowerprefix}` and
  `{sha256-checksum}` are supported). The archive is checked against the index
  `cksum` and cached together with its verified index line, so cached versions stay
  listed and downloadable during an upstream outage.
- The crates.io `dl` host is not the index host, so it must be listed in
  `redirect_origins`. The source credential is never sent to it.

| Upstream                | `origin`                           | `redirect_origins`                             | Notes                                                                                     |
| ----------------------- | ---------------------------------- | ---------------------------------------------- | ----------------------------------------------------------------------------------------- |
| crates.io               | `https://index.crates.io`          | `["https://static.crates.io"]`                 | `static.crates.io` serves the `.crate` files. Covered by the conformance suite (`itoa`).  |
| Another sparse registry | the host serving its `config.json` | the host(s) in its `dl` template, if different | Store the upstream token on the source with `PUT /api/v1/sources/{id}/remote-credential`. |

::: warning Gap
Only crates.io has been tested as an upstream. An authenticated third-party sparse
registry as a remote source has not been exercised.
:::

## Client configuration

Cargo sends the registry token as the raw `Authorization` header value (no scheme).
Ant treats that value as a bearer token. Cargo 1.74 or later is required for
registries that set `auth-required`.

Any credential type works as the Cargo token: a service token (`ant_...`, including
tokens from the GitHub OIDC exchange), a personal access token (`antp_...`) or a
GitHub Actions JWT. The scheme-less value goes through the same resolution as
`Bearer`, so scoping, audit, rate limits and the feed's client-auth policy apply. A
scheme-less value is never treated as a password login. See
[Service accounts and tokens](../identity/tokens.md).

### Private registry next to crates.io

`.cargo/config.toml` (in the project or in `$CARGO_HOME`):

```toml
[registries.ant]
index = "sparse+https://ant.example.com/feeds/cargo/cargo/"
credential-provider = "cargo:token"
```

Provide the token in one of three ways: the environment variable
`CARGO_REGISTRIES_ANT_TOKEN`, `cargo login --registry ant` (which stores it in
`$CARGO_HOME/credentials.toml`), or a `token = "..."` entry under
`[registries.ant]` in `credentials.toml`.

```sh
export CARGO_REGISTRIES_ANT_TOKEN=<TOKEN>   # raw token, no "Bearer"
```

Depend on a crate from the feed:

```toml
[dependencies]
mycrate = { version = "0.1", registry = "ant" }
```

### Publish

Restrict a crate to the feed with `publish = ["ant"]` in its `Cargo.toml`, then run:

```sh
cargo publish --registry ant
```

Yank and unyank:

```sh
cargo yank --registry ant mycrate@0.1.1
cargo yank --registry ant --undo mycrate@0.1.1
```

Yanking is allowed for publishers of the crate (delete permission also works),
unless the feed's delete policy is `denied`. Deleting a version is not part of the
Cargo protocol. Use the management API for that (see
[Management API](../reference/api.md)).

### Replace crates.io with the feed

To send all crates.io traffic through a feed that has a crates.io remote source:

```toml
[source.crates-io]
replace-with = "ant"

[source.ant]
registry = "sparse+https://ant.example.com/feeds/cargo/cargo/"

[registries.ant]
index = "sparse+https://ant.example.com/feeds/cargo/cargo/"

[registry]
global-credential-providers = ["cargo:token"]
```

Set `CARGO_REGISTRIES_ANT_TOKEN` as above. For a private CA, add
`[http] cainfo = "/path/to/ca.pem"`.

::: warning Gap
This source-replacement configuration is taken from the repository's client notes
(`docs/clients/cargo.md`). The conformance suite tests the crates.io remote through a
named registry (`registry = "antr"`), not through `[source.crates-io]` replacement.
:::

### GitHub Actions

```yaml
- run: |
    mkdir -p .cargo
    cat > .cargo/config.toml <<'EOF'
    [registries.ant]
    index = "sparse+https://ant.example.com/feeds/cargo/cargo/"
    credential-provider = "cargo:token"
    EOF
    cargo build
  env:
    CARGO_REGISTRIES_ANT_TOKEN: ${{ env.ANT_TOKEN }}
```

`ANT_TOKEN` can come from a repository secret or from the GitHub OIDC exchange, which
returns a service token. See [GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                              | Cause                                                                                                                                                                    |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `401` with `WWW-Authenticate: Cargo` | No token was sent. Check `credential-provider` and that the token variable name matches the registry name (`CARGO_REGISTRIES_<NAME>_TOKEN`, uppercase, `-` becomes `_`). |
| `403 Cargo authentication required`  | A token was sent but rejected: invalid, expired or revoked, or no read permission. The adapter turns `401` into `403` when credentials were present.                     |
| `403` on publish                     | The token lacks publish permission, or the publish policy does not route the crate to a local source.                                                                    |
| `400` on publish                     | Invalid crate name (uppercase, underscore, longer than 64), or a malformed or unsafe archive.                                                                            |
| `409` on publish                     | The version already exists with different content.                                                                                                                       |
| `405 Cargo operation unsupported`    | `cargo owner`, `cargo search` and similar commands are not implemented.                                                                                                  |
| `503` on `config.json`               | `ANT_PUBLIC_URL` is not set.                                                                                                                                             |
| `503 upstream unavailable`           | The upstream failed and nothing is cached for that crate. A crate download that redirects to a host missing from `redirect_origins` also fails.                          |
| `429` with `Retry-After`             | Too many failed authentication attempts from this client.                                                                                                                |
| `X-Kuvryn-Cache: stale` / `offline`  | The index file came from the metadata cache past its TTL, or from locally cached versions, because upstream failed.                                                      |

To see which source owns a crate, call
`GET /api/v1/feeds/{id}/resolve?coordinate=<crate name>`
(see [Read order and resolve explain](../concepts/resolution.md)).
