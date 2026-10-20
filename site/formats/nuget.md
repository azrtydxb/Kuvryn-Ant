# NuGet

The NuGet adapter serves the NuGet V3 protocol for `dotnet`, `nuget.exe` and other
V3 clients. It provides the service index, the flat container (`PackageBaseAddress`)
for version lists, `.nupkg` and `.nuspec` downloads, registration indexes and leaves
(`RegistrationsBaseUrl`), and the V2 push endpoint that `dotnet nuget push` uses. With
a remote source it can proxy and cache `api.nuget.org` or another V3 feed.

Service index (the package source URL):

```text
https://ant.example.com/feeds/<slug>/nuget/v3/index.json
```

The service index advertises three resources, all under the feed:

| Resource                     | URL                                                            |
| ---------------------------- | -------------------------------------------------------------- |
| `PackageBaseAddress/3.0.0`   | `https://ant.example.com/feeds/<slug>/nuget/v3/flatcontainer/` |
| `RegistrationsBaseUrl/3.6.0` | `https://ant.example.com/feeds/<slug>/nuget/v3/registration/`  |
| `PackagePublish/2.0.0`       | `https://ant.example.com/feeds/<slug>/nuget/v2/package`        |

These URLs are built from `ANT_PUBLIC_URL`, never from the request's `Host` or
forwarded headers. Without a public URL every route returns `503`. See
[Configuration](../reference/configuration.md).

Implementation: [`internal/nuget`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/nuget/README.md).

## Support matrix

| Operation                           | Supported | Notes                                                                                                                                                                                |
| ----------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Consume (`dotnet restore`)          | Yes       | Flat container, registration, `.nupkg`, `.nuspec`.                                                                                                                                   |
| Publish (`dotnet nuget push`)       | Yes       | `PUT v2/package` (multipart) with `X-NuGet-ApiKey`.                                                                                                                                  |
| Delete (`dotnet nuget delete`)      | Yes       | `DELETE v2/package/<id>/<version>`. If the feed policy only allows unlisting, or the release is immutable, the version is unlisted instead (response header `X-Ant-Action: unlist`). |
| Relist                              | Yes       | `POST v2/package/<id>/<version>`.                                                                                                                                                    |
| Search, autocomplete                | No        | Not advertised. Package browsing in IDEs does not work; restore by ID and version does.                                                                                              |
| V2 OData feed, Chocolatey discovery | No        |                                                                                                                                                                                      |
| Symbol packages (`.snupkg`)         | No        | No symbol publish resource is advertised.                                                                                                                                            |
| Remote (pull-through)               | Yes       | Upstream service index discovery, re-rendered registrations, `.nupkg`/`.nuspec` cached on first download.                                                                            |
| Replica                             | Yes       | Through the format-agnostic replication core. The adapter README lists replication as outside the adapter's own tests.                                                               |

## Limits

- **Package size**: 512 MiB compressed, 2 GiB expanded, 10,000 entries, 8 MiB ZIP
  central directory, 1 MiB `.nuspec`. ZIP64 is rejected. Unsafe paths,
  case-insensitive duplicate names, symlinks, excessive compression ratios, corrupt
  checksums, several root `.nuspec` files, a file name that does not match the ID,
  and XML DTDs or entities are rejected.
- **Version normalization** follows NuGet's rules: IDs and prerelease labels are
  lowercased, leading zeros dropped, missing components become zero, a fourth
  component of zero and build metadata are removed. A nonzero fourth component is
  kept.
- **Immutability.** Pushing identical bytes again succeeds. Different bytes for the
  same normalized ID and version return `409`.
- **Index size**: at most 4,096 versions and 16 MiB per registration document, built
  within 15 seconds. Registration pages hold up to 64 inline leaves. A document over
  budget fails instead of being truncated.
- Four concurrent uploads per process. A push that arrives while four are already
  running gets `503`.
- Remote fills are length-checked but not hash-verified, because NuGet publishes no
  sha256 for packages.

## Create the feed

In the console:

1. **Sources**, then **Create source**: a `local` source with format `nuget` (for
   example `nuget-local`), and optionally a `remote` source (for example
   `nuget-org`) configured on its **Upstream** tab as described in
   [Remote upstreams](#remote-upstreams).
2. **Feeds**, then **Create feed**: slug `nuget`, format `nuget`.
3. Under **Read order**, put the local source first. Under **Publish policy**, choose
   the local source as the default.

Through the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: nuget-local-1' \
  -d '{"name":"nuget-local","kind":"local","format":"nuget"}' | jq -r .id)
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: nuget-org-1' \
  -d '{"name":"nuget-org","kind":"remote","format":"nuget"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: nuget-org-cfg-1' -d '{
  "origin":"https://api.nuget.org",
  "redirect_origins":["https://globalcdn.nuget.org"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: nuget-feed-1' -d @- <<JSON
{"slug":"nuget","format":"nuget","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

Owners are chosen per normalized package ID. The first source in the
[read order](../concepts/resolution.md) that has an ID serves all of its versions;
a version missing from that source never falls through to another source. A
locally published `MyCompany.Core` therefore cannot be shadowed by a public package
with the same ID. See [Feeds and sources](../concepts/feeds-and-sources.md) and
[Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The remote source origin is the host that serves `v3/index.json`. Ant reads the
upstream service index through the metadata cache and discovers its
`PackageBaseAddress` and `RegistrationsBaseUrl` resources from it. Clients never see
upstream URLs:

- Version lists and registration documents are re-rendered with `@id`,
  `packageContent` and `registration` rebuilt from the feed's base URL. Registration
  pages that upstream does not inline are fetched and inlined (up to the
  4,096-version limit). Other catalog fields are kept.
- `.nupkg` and `.nuspec` files are cached into the store on first download and served
  locally afterwards, including during an outage.
- A resource host other than the origin, and any host the upstream redirects
  downloads to, must be in `redirect_origins`. The source credential is only ever
  sent to the origin.

| Upstream        | `origin`                                                              | `redirect_origins`                                    | Notes                                                                                                                                  |
| --------------- | --------------------------------------------------------------------- | ----------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| nuget.org       | `https://api.nuget.org`                                               | `["https://globalcdn.nuget.org"]`                     | This is the configuration the conformance suite uses (`Newtonsoft.Json 13.0.3`, plus a cached download with the upstream unreachable). |
| Another V3 feed | the URL that `v3/index.json` sits under (Ant appends `v3/index.json`) | every other host named in its service index resources | For Azure Artifacts, GitHub Packages and similar, store the credential on the source.                                                  |

For an authenticated upstream, set the credential with
`PUT /api/v1/sources/{id}/remote-credential`, for example
`{"authorization":"Basic <base64 user:token>"}` or `{"api_key":"<key>"}` (sent as
`X-Api-Key`), with the source `If-Match` and an `Idempotency-Key`. It is stored
encrypted and never returned.

::: warning Gap
Ant requests `v3/index.json` relative to the origin, so an upstream whose service index
lives under a deeper path (for example
`https://pkgs.dev.azure.com/myorg/_packaging/myfeed/nuget/v3/index.json`) needs
that prefix in the origin (`.../myfeed/nuget`). Only nuget.org has been tested as an
upstream; such path-prefixed origins have not been exercised.
:::

## Client configuration

Reads accept HTTP Basic auth (any non-empty username, and a service token or personal
access token as the password) or a bearer token. NuGet clients first send each
request without credentials, then retry after the `401` with
`WWW-Authenticate: Basic realm="NuGet"`. Those challenged first requests are not
counted as failed logins.

Pushes and deletes authenticate with the `X-NuGet-ApiKey` header, which carries the
token.

The API key accepts any credential type: a service token (`ant_...`, including a token
from the GitHub OIDC exchange), a personal access token (`antp_...`) or a GitHub
Actions JWT. It goes through the same resolution as `Bearer`, so scoping, audit, rate
limits and the feed's client-auth policy apply. On push and delete only the API key is
consulted, and it is never treated as a password login. See
[Service accounts and tokens](../identity/tokens.md).

### nuget.config

```xml
<?xml version="1.0" encoding="utf-8"?>
<configuration>
  <packageSources>
    <clear />
    <add key="ant" value="https://ant.example.com/feeds/nuget/nuget/v3/index.json" protocolVersion="3" />
  </packageSources>
  <packageSourceCredentials>
    <ant>
      <add key="Username" value="token" />
      <add key="ClearTextPassword" value="%ANT_TOKEN%" />
    </ant>
  </packageSourceCredentials>
</configuration>
```

`<clear />` removes nuget.org, so every package resolves through the feed. Add a
remote source to the feed if you need public packages. NuGet expands `%ANT_TOKEN%`
from the environment.

### dotnet CLI

::: code-group

```sh [add source]
dotnet nuget add source https://ant.example.com/feeds/nuget/nuget/v3/index.json \
  --name ant --username token --password "$ANT_TOKEN" --store-password-in-clear-text
```

```sh [restore]
dotnet restore
```

```sh [push]
dotnet pack -c Release -o out
dotnet nuget push out/MyCompany.Core.1.0.0.nupkg --source ant --api-key "$ANT_TOKEN"
```

```sh [delete]
dotnet nuget delete MyCompany.Core 1.0.0 --source ant --api-key "$ANT_TOKEN" --non-interactive
```

:::

`--store-password-in-clear-text` is needed on Linux and macOS, where the encrypted
store is not available. `dotnet nuget push` finds the push URL through the
`PackagePublish/2.0.0` resource in the service index.

### GitHub Actions

```yaml
- uses: actions/setup-dotnet@v4
  with:
    dotnet-version: 10.0.x
- run: |
    dotnet nuget add source https://ant.example.com/feeds/nuget/nuget/v3/index.json \
      --name ant --username ci --password "$ANT_TOKEN" --store-password-in-clear-text
    dotnet restore
    dotnet pack -c Release -o out
    dotnet nuget push "out/*.nupkg" --source ant --api-key "$ANT_TOKEN"
  env:
    ANT_TOKEN: ${{ env.ANT_TOKEN }}
```

`ANT_TOKEN` can come from a repository secret or from the GitHub OIDC exchange, which
returns a service token that also works as an API key. See
[GitHub Actions](../ci/github-actions.md) and
[GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                                       | Cause                                                                                                                                                       |
| --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401` on restore after the client retried     | No credentials for the source in `nuget.config`, or the token is invalid. The source key in `packageSourceCredentials` must match the `packageSources` key. |
| `401 API key required` on push                | `--api-key` was not given, so no `X-NuGet-ApiKey` header was sent.                                                                                          |
| `401` on push with an API key                 | The API key is invalid, expired or revoked.                                                                                                                 |
| `403`                                         | The token lacks read or publish permission on the feed, or the feed's client-auth policy does not accept that credential type.                              |
| `400` on push                                 | The package failed validation (unsafe archive, ID mismatch, ZIP64, oversized `.nuspec`).                                                                    |
| `409` on push                                 | The version exists with different bytes.                                                                                                                    |
| `503` on every request                        | `ANT_PUBLIC_URL` is not set on the instance.                                                                                                                |
| `503` on push                                 | Four uploads are already in progress; retry.                                                                                                                |
| `503` on restore of a public package          | The upstream returned an error and nothing is cached, or a download redirected to a host missing from `redirect_origins`.                                   |
| `429` with `Retry-After`                      | Too many failed authentications from this client address.                                                                                                   |
| Package search in Visual Studio shows nothing | Search is not implemented. Reference packages by ID and version.                                                                                            |
| `X-Kuvryn-Cache: stale` / `offline`           | The version list or registration came from cache because the upstream failed.                                                                               |

To see which source owns a package ID, call
`GET /api/v1/feeds/{id}/resolve?coordinate=<lowercase package id>`
(see [Read order and resolve explain](../concepts/resolution.md)).
