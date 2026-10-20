# Swift packages

The Swift adapter implements the Swift Package Registry service, version 1 (SE-0292). SwiftPM resolves `.package(id: "scope.name", ...)` dependencies against it, and packages can be published to it. A feed can also pull through another SE-0292 registry.

Registry URL: `https://ant.example.com/feeds/<slug>/swift` (feed format `swift`).

## Support matrix

| Publish                                                                                                       | Consume                                                             | Remote (pull-through)                                                   | Replica                       | Notes                                                                |
| ------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- | ----------------------------------------------------------------------- | ----------------------------- | -------------------------------------------------------------------- |
| Yes: `PUT <scope>/<name>/<version>` (the SE-0292 publish endpoint that `swift package-registry publish` uses) | Yes: registry API (`swift package resolve` with `id:` dependencies) | Yes: any SE-0292 registry, archive checked against the release checksum | Yes, through core replication | Every request needs `Accept: application/vnd.swift.registry.v1+...`. |

Routes under `/feeds/<slug>/swift/`:

| Method   | Path                                                         | Purpose                                                                                                                                                                           |
| -------- | ------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET`    | `<scope>/<name>` (or `<name>.json`)                          | Release list, `Link: latest-version`                                                                                                                                              |
| `GET`    | `<scope>/<name>/<version>`                                   | Release metadata (resources, checksum, signing info)                                                                                                                              |
| `GET`    | `<scope>/<name>/<version>/Package.swift[?swift-version=X.Y]` | Manifest (alternates announced through `Link`)                                                                                                                                    |
| `GET`    | `<scope>/<name>/<version>.zip`                               | Source archive, with `Digest` and `Content-Disposition`                                                                                                                           |
| `GET`    | `identifiers?url=<repository URL>`                           | Package identifiers for a repository URL (local releases only)                                                                                                                    |
| `PUT`    | `<scope>/<name>/<version>`                                   | Publish: multipart `source-archive`, `metadata`, optional `source-archive-signature` / `metadata-signature` with `X-Swift-Package-Signature-Format`, optional `X-Checksum-Sha256` |
| `DELETE` | `<scope>/<name>/<version>`                                   | Delete a release. The artifact core's delete policy controls whether this is allowed.                                                                                             |

The scope (lowercased) is the namespace used for authorization and publish routing. A [publish policy](../concepts/publish-policies.md) rule can therefore send different scopes to different local sources.

## Limits

- `Accept` must name registry API version 1 (`application/vnd.swift.registry.v1+json`, `+zip` or `+swift`). A missing or invalid header gets 400, another version 415. Errors are `application/problem+json`.
- Publish is synchronous: the response is 201, never 202. A source archive can be at most 128 MiB, a manifest at most 1 MiB. A release is one atomic bundle (archive, release record, manifests) and is immutable: an identical retry gets 201, different bytes 409.
- Signatures are stored and served back, but Ant does not verify them cryptographically. SwiftPM verifies them on the client.
- `identifiers?url=` scans at most 4,096 release records and answers only from local releases.
- There is no `POST /login` endpoint (see the gap below).
- Every request without an `Authorization` header gets `401` with `WWW-Authenticate: Bearer realm="swift"`, unless the client-auth layer admitted it as anonymous.
- `ANT_PUBLIC_URL` must be set: release resources and `Link` headers carry absolute URLs.

::: warning Gap

- The native client steps of the conformance run did not complete. On the `http` target, SwiftPM does not send registry credentials over plain HTTP, so those steps were skipped. On the `https` targets, the Swift 6.4.1 Linux client aborted with a client-side crash (`_MultiHandle deallocated`) during `swift package-registry publish` and `swift package resolve`. Publish over the HTTP API, release listing, metadata, archive download and delete passed.
- `swift package-registry login` calls a `POST <registry>/login` endpoint, which the adapter does not implement (the request gets 405). Use `~/.netrc` for credentials instead.
- Remote pull-through is covered by tests against a simulated registry only. It has not been run against a real upstream registry.
  :::

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `swift`. On **Sources**, use **Create source** to create a `local` source, attach it with **Attach source** and make it the **Publish policy** default.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: swift-local-1' \
  -d '{"name":"swift-local","kind":"local","format":"swift"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: swift-feed-1' -d @- <<JSON
{"slug":"swift","format":"swift","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md).

## Remote upstreams

The origin of a remote Swift source is the **registry root**, the URL a client would pass to `swift package-registry set`.

- The owner of each `scope.name` is decided before any upstream request. A locally published package is never completed from upstream.
- The release list and release resource go through the metadata cache. They are validated (version, id, a `source-archive` resource with a SHA-256 checksum) and re-rendered with feed URLs. While the upstream is reachable, its list replaces the cached view. During an outage, cached releases are listed. Stale responses carry `X-Kuvryn-Cache: stale`.
- The source archive is fetched on the first `GET .../<version>.zip`, checked against the upstream checksum before the first byte is served, and cached with a release record that keeps the upstream signing data. `X-Swift-Package-Signature`, `X-Swift-Package-Signature-Format` and `Digest` are served from that record, so SwiftPM verifies publisher signatures itself. Ant neither signs nor verifies.
- `Package.swift` is cached on first read. Only the default manifest is proxied: `?swift-version=` gets the default manifest, because upstream `Link` headers for alternate manifests are not exposed.
- Release lists are limited by the metadata limit compiled into the binary (24 MiB) and to 4,096 releases.

There is no widely used public SE-0292 registry with a well-known URL, so there is no default upstream. Point the source at your upstream registry's root:

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: swift-remote-1' \
  -d '{"name":"swift-upstream","kind":"remote","format":"swift"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: swift-remote-cfg-1' -d '{
  "origin":"https://registry.example.com/swift","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
# optional upstream credential, stored encrypted and sent only to the origin
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-credential -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: swift-remote-cred-1' -d '{"authorization":"Bearer <upstream-token>"}'
```

If the upstream serves archives from another host (for example object storage), add that host to `redirect_origins`.

## Client configuration

The adapter accepts `Authorization: Bearer <token>` or HTTP Basic with the token as password. SwiftPM sends registry credentials only over HTTPS.

### Credentials

SwiftPM reads `~/.netrc` for registry hosts:

```sh
cat >> ~/.netrc <<EOF
machine ant.example.com login token password $ANT_TOKEN
EOF
chmod 600 ~/.netrc
```

### Consume

::: code-group

```sh [global]
swift package-registry set --global https://ant.example.com/feeds/swift/swift
```

```sh [per scope]
swift package-registry set --scope myorg https://ant.example.com/feeds/swift/swift
```

```json [.swiftpm/configuration/registries.json]
{
  "registries": {
    "[default]": { "url": "https://ant.example.com/feeds/swift/swift" }
  },
  "version": 1
}
```

:::

```swift
// Package.swift
dependencies: [
    .package(id: "myorg.MyLib", from: "1.0.0")
],
targets: [
    .executableTarget(name: "App", dependencies: [.product(name: "MyLib", package: "myorg.MyLib")])
]
```

```sh
swift package resolve
```

### Publish

::: code-group

```sh [swift package-registry publish]
swift package-registry publish myorg.MyLib 1.0.0 \
  --url https://ant.example.com/feeds/swift/swift
```

```sh [HTTP API]
# MyLib-1.0.0.zip: the package sources under a top-level directory, with Package.swift
curl -fsS -u "token:$ANT_TOKEN" -X PUT \
  -H 'Accept: application/vnd.swift.registry.v1+json' \
  -F 'source-archive=@MyLib-1.0.0.zip;type=application/zip' \
  -F 'metadata={"description":"My library","author":{"name":"myorg"}};type=application/json' \
  https://ant.example.com/feeds/swift/swift/myorg/MyLib/1.0.0
```

:::

The token needs publish permission on the feed. The native `publish` command has not been verified end to end (see the gap above). The HTTP API form is the one covered by the conformance run.

### GitHub Actions

Exchange the OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)) and write it to `~/.netrc`:

```yaml
- run: |
    printf 'machine ant.example.com login ci password %s\n' "$ANT_TOKEN" > ~/.netrc
    chmod 600 ~/.netrc
    swift package-registry set --global https://ant.example.com/feeds/swift/swift
    swift package resolve
```

## Troubleshooting

| Symptom                     | Cause                                                                                                            |
| --------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `400 invalid Accept header` | The request lacks `Accept: application/vnd.swift.registry.v1+json` (relevant for `curl`, not for SwiftPM).       |
| `415`                       | The `Accept` header names a registry API version other than 1.                                                   |
| `401`                       | No credentials: `.netrc` is missing, or the registry URL is plain HTTP (SwiftPM sends no credentials over HTTP). |
| `403`                       | The token lacks read or publish for this feed or scope.                                                          |
| `405` on `login`            | Not implemented. Use `.netrc`.                                                                                   |
| `409` on publish            | The version exists with different bytes.                                                                         |
| `503` on a proxied package  | The upstream failed and nothing is cached, or the release list exceeds the metadata limit.                       |

Adapter internals: [internal/swiftrepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/swiftrepo/README.md).
