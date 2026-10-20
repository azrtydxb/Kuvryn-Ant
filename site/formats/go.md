# Go modules and the checksum database

The Go adapter implements the
[GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol) for private modules you
upload, and as a caching proxy in front of `proxy.golang.org` or any other GOPROXY.
It also mirrors the public checksum database `sum.golang.org`, so checksum checks
stay on when the go command only talks to Ant.

Endpoint (the value of `GOPROXY`):

```text
https://ant.example.com/feeds/<slug>/go
```

Implementation: [`internal/goproxy`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/goproxy/README.md).

## Support matrix

| Operation                                           | Supported          | Notes                                                                                                                                         |
| --------------------------------------------------- | ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- |
| Consume (`go get`, `go mod download`, `go list -m`) | Yes                | `@v/list`, `@v/<version>.info`, `.mod`, `.zip`, `@latest`.                                                                                    |
| Publish                                             | Yes (admin upload) | Go has no publish command. Upload `info`, `mod` and `zip` together with an HTTP PUT; see [Publish private modules](#publish-private-modules). |
| Delete                                              | Yes                | `DELETE /feeds/<slug>/go/<module>/@v/<version>` (Kuvryn Ant extension, gated by the feed's delete policy).                                    |
| Remote (pull-through)                               | Yes                | `.info`, `.mod` and `.zip` are cached on first download. `@v/list` and `@latest` come through the metadata cache.                             |
| Checksum database mirror                            | Yes                | `/sumdb/sum.golang.org/{supported,latest,lookup/...,tile/...}`, available when the feed has a remote source.                                  |
| Replica                                             | Yes                | Through the format-agnostic replication core. The adapter README lists replica flows as unverified for Go.                                    |

## Limits

- **Zip validation** uses `golang.org/x/mod/zip.CheckZip`: canonical paths, no
  collisions, and Go's own size limits (500 MiB zip compressed or uncompressed,
  16 MiB `go.mod`). Every file is decompressed to check its CRC. `.info` is limited
  to 1 MiB.
- The `.mod` must declare the requested module path. If the zip has a root `go.mod`,
  it must be byte-for-byte the same as the uploaded `.mod`.
- The `.info` JSON must contain a `Version` equal to the version in the URL.
- **Versions** must be canonical semantic versions (`v1.2.3`, `v1.2.3-rc.1`,
  pseudo-versions). Module paths and versions must use the escaped form (`!` before
  a lowercase letter stands for an uppercase letter).
- **Immutability.** Uploading identical bytes again returns `200`, a new version
  returns `201`, and different bytes for an existing version return `409`.
- `@v/list` leaves out pseudo-versions. `@latest` prefers the highest release, then
  the highest prerelease, then the newest pseudo-version.
- A version index fails rather than truncating above 10,000 versions.
- Four concurrent uploads per process. Transfers have a 30-second idle deadline.
- Read requests for anything that is not a valid module path or version get `404`,
  so the go command keeps probing path prefixes instead of aborting.

::: warning Gap: anonymous reads
The adapter README says anonymous access is disabled. Later, the package client-auth
middleware gained anonymous reads for feeds with `anonymous_read` enabled, and the Go
adapter does not reject an anonymous principal. Anonymous `go get` has not been tested
against a Go feed. Plan on using a token.
:::

## Create the feed

In the console:

1. **Sources**, then **Create source**: a `local` source with format `go` (for
   example `go-local`), and a `remote` source (for example `goproxy`). On the remote
   source's **Upstream** tab, set the origin and add `https://storage.googleapis.com`
   to **Allowed redirect origins** (see
   [Remote upstreams](#remote-upstreams)).
2. **Feeds**, then **Create feed**: slug `go`, format `go`.
3. Under **Read order**, put the local source first. Under **Publish policy**, choose
   the local source as the default.

Through the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: go-local-1' \
  -d '{"name":"go-local","kind":"local","format":"go"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: goproxy-1' \
  -d '{"name":"goproxy","kind":"remote","format":"go"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: goproxy-cfg-1' -d '{
  "origin":"https://proxy.golang.org",
  "redirect_origins":["https://storage.googleapis.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: go-feed-1' -d @- <<JSON
{"slug":"go","format":"go","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

Each module path has a single owner: the first source in the
[read order](../concepts/resolution.md) that has it. A module uploaded to the local
source is never looked up upstream, and versions from different sources are never
merged. See [Feeds and sources](../concepts/feeds-and-sources.md) and
[Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The remote source origin is a GOPROXY root. A request for `<module>/@v/<file>` on the
feed fetches the same path from the origin.

::: danger proxy.golang.org needs a redirect origin
`proxy.golang.org` answers some `.zip` downloads, mostly large modules such as
`github.com/aws/aws-sdk-go`, with a `302` to
`https://storage.googleapis.com/proxy-golang-org-prod/...`. Ant only follows a
redirect to the source origin or to a configured redirect origin. **Add
`https://storage.googleapis.com` to the source's `redirect_origins`.** Without it,
those downloads fail with the `policy` error class (redirect off origin) while
smaller modules keep working, so the problem can go unnoticed until a large
dependency is added. The source credential, if any, is never sent to a redirect
origin.
:::

| Upstream         | `origin`                   | `redirect_origins`                   | Notes                                                                                                                                          |
| ---------------- | -------------------------- | ------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| Go module mirror | `https://proxy.golang.org` | `["https://storage.googleapis.com"]` | Pull-through of `github.com/google/uuid@v1.6.0`, and a cached re-download with the upstream unreachable, are covered by the conformance suite. |
| Another GOPROXY  | its root URL               | any host its downloads redirect to   | For an authenticated upstream, store the credential on the source.                                                                             |

How remote content is handled:

- `.info`, `.mod` and `.zip` are immutable. They are cached into the store under the
  remote source on first download, with the same media types as local uploads, and
  served locally afterwards, including during an outage.
- `@v/list` and `@latest` are mutable. They go through the metadata cache and are
  marked `X-Kuvryn-Cache: stale` when served past their TTL because upstream failed.
  If upstream is down and nothing is cached, the feed answers from locally cached
  versions with `X-Kuvryn-Cache: offline`.
- A `404` or `410` from upstream moves on to the next source. Any other error stops
  the search with a `503`.

### Checksum database

The go command checks downloaded modules against `sum.golang.org`. With
`GOPROXY` pointing at Ant, it first asks
`<GOPROXY>/sumdb/sum.golang.org/supported`. If that succeeds, it fetches checksum
database tiles and lookups through the proxy instead of contacting
`sum.golang.org` directly. Ant handles this as follows:

- `supported` returns `200` only when the feed has at least one remote source.
  Otherwise it returns `404`, and the go command contacts `sum.golang.org` itself, as
  the GOPROXY protocol specifies.
- `latest`, `lookup/<module>@<version>` and `tile/...` are first requested from the
  remote source under `/sumdb/sum.golang.org/` (`proxy.golang.org` serves them). If
  the origin returns `404` there, Ant fetches them from `https://sum.golang.org`
  directly. That host is always allowed and never receives a credential, so it does
  not need to be a redirect origin.
- Responses go through the metadata cache and can be served stale during an outage.
  They are signed by the checksum database, and the go command verifies the
  signatures, so Ant cannot alter them without detection.
- Only `sum.golang.org` is mirrored. Requests for any other database name return
  `404`.

Private modules are not in `sum.golang.org`. Exclude them with `GONOSUMDB` or
`GOPRIVATE` (see below).

## Client configuration

### Authentication

The go command sends credentials from `~/.netrc`, and only over HTTPS. Go 1.24 and
later read it through `GOAUTH`, whose default is `netrc`. Use any non-empty login and
a service token or personal access token as the password. See
[Service accounts and tokens](../identity/tokens.md) and
[Package client auth](../identity/client-auth.md).

```text
# ~/.netrc  (chmod 600)
machine ant.example.com login token password <TOKEN>
```

### Consume

```sh
export GOPROXY=https://ant.example.com/feeds/go/go
export GOSUMDB=sum.golang.org      # the default; checked through the proxy
# Private CA:
# export SSL_CERT_FILE=/path/to/ca.pem
go mod download
```

To keep all traffic on Ant, leave out `,direct` in `GOPROXY`. With
`GOPROXY=https://ant.example.com/feeds/go/go,direct`, the go command falls back to
version control when the feed answers `404` or `410`.

### Private module paths

```sh
export GOPROXY=https://ant.example.com/feeds/go/go
export GOPRIVATE=example.com/myorg/*
export GONOPROXY=none
```

`GOPRIVATE` sets the defaults for both `GONOSUMDB` and `GONOPROXY`. `GONOSUMDB` is
what you want: private modules are not checked against `sum.golang.org`. The default
`GONOPROXY` would make the go command bypass the proxy and clone from version control,
so set `GONOPROXY=none` to keep fetching private modules from Ant. Setting only
`GONOSUMDB=example.com/myorg/*` has the same effect.

### Publish private modules

Upload a version with a PUT of a multipart bundle to
`/feeds/<slug>/go/<escaped-module>/@v/<escaped-version>` (no file suffix). The
request must contain exactly one part each named `info`, `mod` and `zip`, in any
order. The token needs publish permission on the feed. All three files become visible
atomically.

The simplest way to produce the three files in the exact format the go command
expects is to let the go command download the version from its original location,
then upload the files from the module cache:

```sh
MOD=example.com/myorg/mylib
VER=v1.2.3
# Fetch from version control into the local module cache.
J=$(GOPROXY=direct GOPRIVATE=example.com/myorg/* go mod download -json "$MOD@$VER")
INFO=$(echo "$J" | jq -r .Info); GOMOD=$(echo "$J" | jq -r .GoMod); ZIP=$(echo "$J" | jq -r .Zip)

curl -fsS -X PUT -H "Authorization: Bearer $ANT_TOKEN" \
  -F "info=@$INFO" -F "mod=@$GOMOD" -F "zip=@$ZIP" \
  "https://ant.example.com/feeds/go/go/$MOD/@v/$VER"
```

If the module path contains uppercase letters, use the escaped form in the URL
(`github.com/MyOrg/x` becomes `github.com/!my!org/x`).

You can also build the zip yourself with
`golang.org/x/mod/zip.CreateFromDir`. The `.info` file is JSON such as
`{"Version":"v1.2.3","Time":"2026-01-01T00:00:00Z"}`, and the `.mod` file is the
module's `go.mod`.

To remove a version:

```sh
curl -fsS -X DELETE -H "Authorization: Bearer $ANT_TOKEN" \
  "https://ant.example.com/feeds/go/go/example.com/myorg/mylib/@v/v1.2.3"
```

Module authors would normally retract a version in `go.mod` instead. Retractions
are kept in the served `go.mod` for the client to act on.

### GitHub Actions

Write `.netrc` from a token, which can come from a secret or from the GitHub OIDC
exchange (see [GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md)):

```yaml
- uses: actions/setup-go@v5
  with: { go-version: stable }
- run: |
    printf 'machine ant.example.com login ci password %s\n' "$ANT_TOKEN" > ~/.netrc
    chmod 600 ~/.netrc
    go build ./...
  env:
    ANT_TOKEN: ${{ env.ANT_TOKEN }}
    GOPROXY: https://ant.example.com/feeds/go/go
    GONOSUMDB: example.com/myorg/*
```

## Troubleshooting

| Symptom                                                     | Cause                                                                                                                                                                                                           |
| ----------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401 Unauthorized` or `reading .../@v/list: 401`            | No `.netrc` entry for the host, or the URL is plain HTTP. Go only sends `.netrc` credentials over HTTPS.                                                                                                        |
| `403`                                                       | The token cannot read (or publish to) the feed, or the feed's client-auth policy does not accept that credential type.                                                                                          |
| `404 not found: not a module path or version`               | Expected while the go command probes path prefixes. It is a problem only for the final module path.                                                                                                             |
| `verifying module:` errors reaching `sum.golang.org`        | The feed has no remote source, so `supported` returns 404 and the go command contacts `sum.golang.org` directly. Add a remote source, allow egress to `sum.golang.org`, or set `GONOSUMDB` for private modules. |
| `checksum mismatch` / `SECURITY ERROR` for a private module | The module is not in `sum.golang.org`. Set `GONOSUMDB` or `GOPRIVATE`.                                                                                                                                          |
| `503` for one large module, smaller ones work               | `proxy.golang.org` redirected the zip to `storage.googleapis.com`, which is not in `redirect_origins`.                                                                                                          |
| `503` in general                                            | The upstream returned an error other than 404/410 and nothing is cached.                                                                                                                                        |
| `400` on PUT                                                | Wrong part names, a missing part, a module or version mismatch between URL, `.info`, `.mod` and zip, or an invalid zip.                                                                                         |
| `409` on PUT                                                | The version exists with different bytes. Module versions are immutable.                                                                                                                                         |
| `X-Kuvryn-Cache: stale` / `offline`                         | The version list or checksum database response came from the cache because upstream failed.                                                                                                                     |

`GOFLAGS=-x` or `go mod download -x` prints each proxy request. To see which source
owns a module, call
`GET /api/v1/feeds/{id}/resolve?coordinate=<module path>`
(see [Read order and resolve explain](../concepts/resolution.md)).
