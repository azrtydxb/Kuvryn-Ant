# OCI / Docker

The OCI adapter implements the OCI Distribution API (Docker Registry HTTP API v2)
for container images, multi-platform indexes, Helm charts pushed as OCI artifacts and
any other OCI artifact. It works with docker, podman, buildx/BuildKit, containerd,
crane, oras, skopeo and helm. The same adapter serves both OCI feed formats, `oci`
and `oci-artifacts`, and can also act as an unprefixed pull-through mirror for
dockerd, BuildKit and containerd.

Endpoint and image references:

```text
https://ant.example.com/v2/<feed>/<repository>/...
ant.example.com/<feed>/<repository>:<tag>
ant.example.com/<feed>/<repository>@sha256:<digest>
```

The first path segment is the feed slug, the rest is the repository name, which may
have several segments (`ant.example.com/oci-hosted/myorg/app:1.0` is repository
`myorg/app` in feed `oci-hosted`).

Implementation: [`internal/oci`](https://github.com/azrtydxb/Kuvryn-Ant/tree/main/internal/oci).

## Support matrix

| Operation                             | Supported | Notes                                                                                                                                                                                 |
| ------------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pull (manifests, blobs, tags)         | Yes       | `GET`/`HEAD`; tag list with `n`/`last` pagination.                                                                                                                                    |
| Push (monolithic and chunked uploads) | Yes       | Resumable uploads; cross-repository blob mount (`?mount=`).                                                                                                                           |
| Multi-platform images and indexes     | Yes       | `docker buildx build --platform ... --push`.                                                                                                                                          |
| Referrers API                         | Yes       | `GET /v2/<feed>/<repo>/referrers/<digest>`, with `artifactType` filter; used by `oras discover`, `oras attach`.                                                                       |
| Delete                                | Partly    | Manifest delete by digest (removes the manifest and its tags) or by tag (removes only that tag). Blob `DELETE` returns `405`; unreferenced blobs are reclaimed by garbage collection. |
| Catalog (`/v2/_catalog`)              | No        | Use the management API package listing instead.                                                                                                                                       |
| Remote (pull-through)                 | Yes       | Blobs and manifests by digest are cached; tags go through the metadata cache. Bearer-token handshake supported (Docker Hub, ghcr.io, ...).                                            |
| Mirror mode (unprefixed `/v2/<name>`) | Yes       | Read-only; `ANT_OCI_DEFAULT_FEED`, `ANT_OCI_MIRROR_MAP`.                                                                                                                              |
| Replica                               | Yes       | Through the format-agnostic replication core.                                                                                                                                         |
| Build cache feeds                     | Yes       | `purpose: cache` feeds for `--cache-to type=registry`; see [buildx registry cache](../build-caches/buildx.md).                                                                        |

Real-client conformance (2026-10-19): crane push, pull, retag, multi-arch index,
delete by tag, auth denial and pull-through through `registry-1.docker.io` and
`mirror.gcr.io`; oras push, pull, attach/discover, delete and pull-through from
`ghcr.io`; helm push/pull `oci://`. docker, podman and skopeo use the same API but
were not part of that run.

## Limits

- **Tags are immutable on ordinary feeds.** Pushing an existing tag with a different
  manifest returns `409 DENIED`; pushing the same manifest again succeeds. To move a
  tag, delete it first (`DELETE /v2/<feed>/<repo>/manifests/<tag>` removes only the
  tag, for example with `crane delete ant.example.com/<feed>/<repo>:<tag>`), then push.
  Only feeds with `purpose: cache` move tags on push (build caches); see
  [Feed purposes](../concepts/purposes.md).
- **Manifest size** is limited to 4 MiB. Blob size has no adapter limit.
- Repository names follow the distribution-spec grammar (lowercase,
  `[a-z0-9]+` components separated by `.`, `_`, `__` or `-`, `/`-separated, at most 256
  characters); tags `[A-Za-z0-9_][A-Za-z0-9._-]{0,127}`. Encoded slashes in the path are
  rejected.
- Uploads share a bounded number of transfer slots per process. When all are busy a
  new upload waits up to 15 seconds and then gets `503` with `Retry-After: 1`.
- Mirror routes are read-only (`405 UNSUPPORTED` for writes).
- There is no separate token service: `GET /v2/` challenges with
  `WWW-Authenticate: Basic`, and every request carries the credential.

::: warning Gap

Pull-through through `quay.io`, `registry.k8s.io`, `gcr.io` and `public.ecr.aws` is
documented with the redirect origins below but has no recorded real-client run. Their
CDN hostnames change over time; if a pull fails with a policy error, the redirect
target appears in the access log and must be added to `redirect_origins`.

:::

## Create the feed

### Console

1. **Sources**: **Create source**, kind `local`, format `oci` (or `oci-artifacts`).
2. **Feeds**: **Create feed** with slug (for example `oci-hosted`) and the same
   format. On the feed detail, attach the source and set it as the publish target in
   the publish policy.
3. For a pull-through feed, create a `remote` source instead, configure it on its
   **Upstream** tab (origin, redirect origins, credential), and attach it to a feed
   such as `dockerhub`. Turn on anonymous reads on that feed if dockerd will use it as
   a mirror.

### API

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# hosted feed for pushes
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: oci-local-1' \
  -d '{"name":"oci-local","kind":"local","format":"oci"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: oci-feed-1' -d @- <<JSON
{"slug":"oci-hosted","format":"oci","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON

# Docker Hub pull-through feed, anonymous reads for dockerd mirrors
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-src-1' \
  -d '{"name":"dockerhub","kind":"remote","format":"oci"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: dockerhub-cfg-1' -d '{
  "origin":"https://registry-1.docker.io",
  "redirect_origins":["https://auth.docker.io","https://production.cloudflare.docker.com","https://production.cloudfront.docker.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-feed-1' -d @- <<JSON
{"slug":"dockerhub","format":"oci","anonymous_read":true,"source_ids":["$REM"]}
JSON
```

Do not give a feed a slug equal to a top-level upstream namespace that you mirror
(for example `library`): a prefixed path whose first segment is an existing OCI feed
slug always wins over mirror routing (see [Mirror mode](#mirror-mode)).

## Remote upstreams

The origin is the registry root; Ant requests `v2/<repository>/manifests/...` and
`v2/<repository>/blobs/...` relative to it.

- **Blobs and manifests by digest** are fetched with their digest verified before the
  first byte is served, then cached under the remote source.
- **Tags** are resolved through the metadata cache (`metadata_ttl_seconds`,
  stale-if-error). A tag `HEAD` is answered from the same resolved manifest, so one
  upstream `GET` serves the `HEAD` and the later by-digest reads. Resolved tags are
  recorded as observed tags for package discovery; they are never served from the
  database.
- **Docker Hub official images.** For a source whose origin is
  `registry-1.docker.io` (or `docker.io`, `index.docker.io`,
  `registry.hub.docker.com`), single-segment names get the implicit `library/`
  namespace upstream (`alpine` is fetched as `library/alpine`), unless a component
  with the short name already exists in the feed.
- **Rate limits.** A `429` from the upstream or its token service is returned to the
  client as `429 TOOMANYREQUESTS` with the upstream `Retry-After`; it is not retried
  and does not open the upstream circuit breaker.

### Token handshake

On a `401` with `WWW-Authenticate: Bearer realm=...,service=...,scope=...` Ant
requests a token from the realm and retries with it.

- The realm must be the source origin or one of its `redirect_origins`; any other
  realm is refused (the pull fails with `503` and an authentication error in source
  health). The realm response is never followed through a redirect.
- If the source has a **Basic** credential it is sent to the realm (authenticated
  pulls, higher rate limits). Otherwise the token is requested anonymously. A static
  `Bearer` credential is sent as-is to the origin and is not used for an exchange.
- The obtained token is sent only to the origin, never to a redirected blob or CDN
  host. Tokens are cached per realm, service and scope until `expires_in` (60 s when
  absent); a failed token request is remembered for 5 seconds.
- Docker Hub answers `401`, not `404`, for a repository that does not exist (or that the
  caller may not see), even after a valid token handshake: it accepts the token and
  then answers `401` with `error="insufficient_scope"` or an `UNAUTHORIZED` error body.
  For a repository request after a successful handshake, Ant treats that as `404`
  (`NAME_UNKNOWN`). Resolution falls through to the next source and the source is not
  marked as an authentication failure. A refused token request, or a token the
  registry calls `invalid_token`, stays a `401` and still counts against source health.

### Upstream examples

| Upstream                    | `origin`                       | `redirect_origins`                                                                                               | Status       |
| --------------------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------- | ------------ |
| Docker Hub                  | `https://registry-1.docker.io` | `https://auth.docker.io`, `https://production.cloudflare.docker.com`, `https://production.cloudfront.docker.com` | Verified     |
| Google mirror of Docker Hub | `https://mirror.gcr.io`        | none                                                                                                             | Verified     |
| GitHub Container Registry   | `https://ghcr.io`              | `https://pkg-containers.githubusercontent.com`                                                                   | Verified     |
| Quay                        | `https://quay.io`              | `https://cdn.quay.io`, `https://cdn01.quay.io`, `https://cdn02.quay.io`, `https://cdn03.quay.io`                 | Not verified |
| Kubernetes registry         | `https://registry.k8s.io`      | `https://prod-registry-k8s-io-<region>.s3.dualstack.<region>.amazonaws.com` for the regions you run in           | Not verified |
| Google Container Registry   | `https://gcr.io`               | usually none                                                                                                     | Not verified |
| Amazon ECR Public           | `https://public.ecr.aws`       | its CloudFront host, for example `https://d1uvnnlqvlnzwp.cloudfront.net`                                         | Not verified |

`https://auth.docker.io` is mandatory for Docker Hub: without it the token handshake
is refused by policy.

### Upstream credentials

For Docker Hub (and other registries that issue tokens from a Basic login), store a
Basic credential; it is sent only to the token realm:

```sh
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
BASIC=$(printf '%s' 'myuser:<docker-hub-access-token>' | base64)
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-credential -H "$H" -H "$J" \
  -H "If-Match: $REV" -H 'Idempotency-Key: dockerhub-cred-1' \
  -d "{\"authorization\":\"Basic $BASIC\"}"
```

The credential is encrypted, never returned and never sent to a redirect host.

## Mirror mode

Mirror clients such as dockerd `registry-mirrors` cannot add a feed prefix. Two
settings (environment variables, in the Helm chart through `extraEnv`) serve
unprefixed paths `/v2/<name>/...`:

| Variable               | Meaning                                                                                                                                                                                                                       |
| ---------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ANT_OCI_DEFAULT_FEED` | Feed slug served for unprefixed `/v2/<name>/...` when the client sends no `ns` parameter (dockerd).                                                                                                                           |
| `ANT_OCI_MIRROR_MAP`   | Comma-separated `host=slug` pairs (at most 64), choosing the feed from the `?ns=<upstream host>` parameter that containerd and BuildKit append. Example: `docker.io=dockerhub,ghcr.io=ghcr,quay.io=quay,registry.k8s.io=k8s`. |

Resolution of `/v2/<name>/...`:

1. If the first segment of `<name>` is the slug of an existing OCI feed, the request
   is the prefixed form for that feed. This always wins.
2. Otherwise the whole `<name>` is the upstream repository. With `?ns=<host>` the
   feed comes from `ANT_OCI_MIRROR_MAP`; if the map is set and the host is not in it,
   the request is refused rather than answered from another upstream. Without `ns`
   (or without a map) `ANT_OCI_DEFAULT_FEED` is used.
3. Mirror routes are read-only. Single-segment names (`pause`) are valid.

dockerd never sends credentials to a mirror, so a dockerd mirror feed needs
`anonymous_read`. An anonymous `GET /v2/` returns `200 {}` only when the default feed
(or the `ns`-mapped feed) allows anonymous reads; every other feed still answers
`401`. Ant does not rate-limit anonymous reads; restrict the listener to trusted
networks.

```yaml
# Helm values
extraEnv:
  ANT_OCI_DEFAULT_FEED: dockerhub
  ANT_OCI_MIRROR_MAP: docker.io=dockerhub,ghcr.io=ghcr,quay.io=quay,registry.k8s.io=k8s
```

Node-level configuration for dockerd, containerd, CRI-O, Podman and BuildKit is on
[Host-level caching](../host-caching/index.md).

## Client configuration

### Authentication

`docker login` (and every OCI client) uses Basic authentication against the
registry. The password is a service token (`ant_...`), a personal access token
(`antp_...`), a GitHub Actions OIDC token, or a directory user's password where the
feed's client-auth policy allows it. For tokens the username is ignored (use any
value, for example `x`).

::: code-group

```sh [docker]
echo "$ANT_TOKEN" | docker login ant.example.com -u x --password-stdin
```

```sh [podman]
echo "$ANT_TOKEN" | podman login ant.example.com -u x --password-stdin
```

```sh [crane]
crane auth login ant.example.com -u x -p "$ANT_TOKEN"
```

```sh [oras]
echo "$ANT_TOKEN" | oras login ant.example.com -u x --password-stdin
```

```sh [helm]
echo "$ANT_TOKEN" | helm registry login ant.example.com -u x --password-stdin
```

:::

Pushing needs a token whose principal has publish rights on the feed; pulling needs
read. See [Service accounts and tokens](../identity/tokens.md) and
[Package client auth](../identity/client-auth.md).

For a private CA, place the CA certificate in
`/etc/docker/certs.d/ant.example.com/ca.crt` (docker) or
`/etc/containers/certs.d/ant.example.com/ca.crt` (podman, skopeo, buildah); see
[CA trust distribution](../host-caching/ca-trust.md).

### Push and pull

::: code-group

```sh [docker]
docker tag myapp:1.0 ant.example.com/oci-hosted/myorg/myapp:1.0
docker push ant.example.com/oci-hosted/myorg/myapp:1.0
docker pull ant.example.com/oci-hosted/myorg/myapp:1.0

# multi-platform build and push
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t ant.example.com/oci-hosted/myorg/myapp:1.1 .
```

```sh [podman]
podman push localhost/myapp:1.0 ant.example.com/oci-hosted/myorg/myapp:1.0
podman pull ant.example.com/oci-hosted/myorg/myapp:1.0
```

```sh [crane]
crane copy alpine:3 ant.example.com/oci-hosted/myorg/alpine:3
crane manifest ant.example.com/oci-hosted/myorg/alpine:3
crane ls ant.example.com/oci-hosted/myorg/alpine
```

```sh [skopeo]
skopeo copy --dest-creds "x:$ANT_TOKEN" \
  docker://docker.io/library/alpine:3 docker://ant.example.com/oci-hosted/myorg/alpine:3
skopeo inspect --creds "x:$ANT_TOKEN" docker://ant.example.com/oci-hosted/myorg/alpine:3
```

:::

Pull through a remote feed with the feed prefix, whatever the upstream:

```sh
docker pull ant.example.com/dockerhub/library/alpine:3
docker pull ant.example.com/ghcr/myorg/myimage:latest
crane manifest ant.example.com/library/alpine:3          # mirror mode: unprefixed, default feed
```

### Helm charts in OCI

`helm push` stores a chart in an OCI feed; see [Helm](./helm.md#helm-charts-in-oci).

```sh
helm push mychart-0.1.0.tgz oci://ant.example.com/oci-hosted
helm pull oci://ant.example.com/oci-hosted/mychart --version 0.1.0
```

### OCI artifacts feeds

An `oci-artifacts` feed is served by the same adapter and protocol as an `oci` feed.
It exists so that non-image content (SBOMs, signatures, attestations, configuration
bundles) can live in its own feed with its own policies and retention. Create it like
an `oci` feed with `"format":"oci-artifacts"` (sources must have the same format).

```sh
# push a file with an artifact type
oras push ant.example.com/artifacts/myorg/sbom:1.0 \
  --artifact-type application/vnd.example.sbom.v1+json ./sbom.json

# attach a referrer to an image and list referrers (referrers API)
oras attach ant.example.com/artifacts/myorg/app:1.0 \
  --artifact-type application/vnd.example.signature ./app.sig
oras discover ant.example.com/artifacts/myorg/app:1.0

oras pull ant.example.com/artifacts/myorg/sbom:1.0
```

### GitHub Actions

With a stored secret:

```yaml
- uses: docker/login-action@v3
  with:
    registry: ant.example.com
    username: x
    password: ${{ secrets.ANT_TOKEN }}
- uses: docker/build-push-action@v6
  with:
    push: true
    tags: ant.example.com/oci-hosted/myorg/myapp:${{ github.sha }}
```

Without a stored secret, exchange the workflow's OIDC token for a short-lived Ant
token (requires a GitHub OIDC trust rule, see [GitHub OIDC trust](../identity/github-oidc.md)):

```yaml
permissions:
  id-token: write
  contents: read
steps:
  - name: Ant token
    run: |
      JWT=$(curl -sS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://ant.example.com" | jq -r .value)
      TOKEN=$(curl -sS -X POST https://ant.example.com/api/v1/auth/github-oidc/exchange \
        -H 'Content-Type: application/json' -d "{\"token\":\"$JWT\"}" | jq -r .access_token)
      echo "::add-mask::$TOKEN"
      echo "ANT_TOKEN=$TOKEN" >> "$GITHUB_ENV"
  - uses: docker/login-action@v3
    with:
      registry: ant.example.com
      username: x
      password: ${{ env.ANT_TOKEN }}
```

The setup action and more patterns are on [GitHub Actions](../ci/github-actions.md).
Registry build caches (`--cache-to type=registry`) belong in a cache feed, not in the
hosted image feed: see [buildx registry cache](../build-caches/buildx.md).

## Troubleshooting

| Symptom                                         | Cause                                                                                                                                                                                             |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401 UNAUTHORIZED` on every request             | No or invalid credential. `docker login` only probes `/v2/`, which may answer `200` without credentials when a mirror feed is anonymous; the real check happens on the first pull or push.        |
| `403 DENIED`                                    | The principal lacks the role for the operation, or the feed's client-auth policy does not accept that credential type.                                                                            |
| `409 DENIED` on push                            | The tag already points to a different manifest (tags are immutable), or the bytes conflict with existing content.                                                                                 |
| `405 UNSUPPORTED`                               | A write on a mirror route, or a blob `DELETE`.                                                                                                                                                    |
| `404 NAME_UNKNOWN` on an unprefixed name        | No mirror feed applies: `ANT_OCI_DEFAULT_FEED` is unset, or the `ns` host is not in `ANT_OCI_MIRROR_MAP`.                                                                                         |
| `429 TOOMANYREQUESTS`                           | Upstream rate limit (Docker Hub anonymous pulls); add a Basic credential to the source. Also returned when a client exceeds the failed-authentication budget (`authentication attempts limited`). |
| `503` on a pull-through                         | Upstream error, timeout, or a redirect / token realm that is not in `redirect_origins`. Check `GET /api/v1/sources/{id}/health` and the access log.                                               |
| `503 UNKNOWN` with `Retry-After` on upload      | All upload slots busy; the client retries.                                                                                                                                                        |
| `X-Kuvryn-Cache: stale` on a tag                | The tag was served from the metadata cache after its TTL because the upstream failed.                                                                                                             |
| `X-Kuvryn-Cache: miss`                          | Served from upstream on this request and being cached.                                                                                                                                            |
| `alpine` and `library/alpine` listed separately | Docker Hub short names are fetched as `library/<name>` but cached under the name the client used; blobs are deduplicated.                                                                         |
