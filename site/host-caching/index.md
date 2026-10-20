# Host-level caching

Ant can sit between container runtimes and public registries as a pull-through mirror. Nodes pull `docker.io/library/alpine:3` as usual. The runtime asks Ant first, Ant serves the image from its cache or fetches it once from the upstream, and every later pull on any node is a local hit. This reduces upstream rate limits and egress, and it keeps cached images available while an upstream is down.

This section covers runtime configuration on the node. The OCI feed itself (sources, credentials, publishing) is described in [OCI / Docker](../formats/oci.md).

## How it works on the Ant side

- **One OCI feed per upstream**, each with a single **remote** source pointing at the upstream registry origin. Typical slugs: `dockerhub`, `ghcr`, `quay`, `k8s`, `gcr`, `ecr`.
- Blobs and manifests are cached by digest under the remote source. Tags go through the source's bounded metadata cache (TTL and stale-if-error). A tag `HEAD` is answered from the same resolved manifest, so one upstream `GET` serves the `HEAD` and every later read by digest. Docker Hub counts only `GET` requests against its pull limit.
- Concurrent pulls of the same missing blob are coalesced into one upstream fetch.
- On a `401` with a `WWW-Authenticate: Bearer` challenge, Ant performs the registry token handshake. Tokens are cached per realm, service and scope. The realm must be the source origin or one of its `redirect_origins`.
- An upstream `429` is passed to the client as `429 TOOMANYREQUESTS` with the upstream `Retry-After`.

Origins and redirect origins for common upstreams:

| Upstream        | `origin`                       | `redirect_origins`                                                                                               |
| --------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------- |
| docker.io       | `https://registry-1.docker.io` | `https://auth.docker.io`, `https://production.cloudflare.docker.com`, `https://production.cloudfront.docker.com` |
| ghcr.io         | `https://ghcr.io`              | `https://pkg-containers.githubusercontent.com`                                                                   |
| quay.io         | `https://quay.io`              | `https://cdn.quay.io`, `https://cdn01.quay.io`, `https://cdn02.quay.io`, `https://cdn03.quay.io`                 |
| gcr.io          | `https://gcr.io`               | usually none                                                                                                     |
| registry.k8s.io | `https://registry.k8s.io`      | `https://prod-registry-k8s-io-<region>.s3.dualstack.<region>.amazonaws.com` for the regions you run in           |
| public.ecr.aws  | `https://public.ecr.aws`       | `https://d1uvnnlqvlnzwp.cloudfront.net` and similar                                                              |

CDN hostnames change over time. When a pull fails with a policy error, the access log shows the redirect target that needs to be added. A Docker Hub source whose `redirect_origins` omit `https://auth.docker.io` is refused at the token handshake. It surfaces as a `503`, with an authentication error in the source's health.

## Two ways to address a feed

### Prefixed feed path

Every OCI feed is reachable under its slug:

```
https://ant.example.com/v2/<feed>/<repository>/...
ant.example.com/dockerhub/library/alpine:3
```

This always works. A runtime can use it as a mirror if it can add a path prefix to mirrored requests:

| Runtime                        | How the prefix is added                                                                            |
| ------------------------------ | -------------------------------------------------------------------------------------------------- |
| k3s, RKE2                      | `rewrite` rules in `registries.yaml`                                                               |
| generic containerd             | `override_path = true` on a `hosts.toml` host whose URL contains `/v2/<feed>`                      |
| CRI-O, Podman, Buildah, Skopeo | a `[[registry.mirror]]` `location` that includes the path, for example `ant.example.com/dockerhub` |

### Mirror mode (unprefixed)

Some clients cannot add a prefix. dockerd's `registry-mirrors` is the main example. For them, Ant serves unprefixed `/v2/<name>/...` and picks the feed from configuration:

| Variable               | Meaning                                                                                                                  |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| `ANT_OCI_DEFAULT_FEED` | Feed slug served for unprefixed `/v2/<name>/...` when the client sends no `ns`                                           |
| `ANT_OCI_MIRROR_MAP`   | Comma-separated `host=slug` pairs, at most 64. Selects the feed from containerd's `?ns=<upstream host>` query parameter. |

containerd (and BuildKit, which uses containerd's resolver) appends `?ns=<upstream host>` to every request it sends to a mirror endpoint. One Ant hostname can therefore mirror many upstreams.

Resolution of `/v2/<name>/...`:

1. If the first segment of `<name>` is the slug of an existing OCI feed, the request is the **prefixed** form for that feed. This always wins.
2. Otherwise the whole `<name>` is the upstream repository:
   - With `?ns=<host>`, the feed is `ANT_OCI_MIRROR_MAP[host]`. If the map is non-empty and the host is not in it, the request is refused. It is never answered from another upstream. If no map is configured, `ANT_OCI_DEFAULT_FEED` is used.
   - Without `ns`, `ANT_OCI_DEFAULT_FEED` is used. dockerd sends no `ns`.
3. Mirror routes are read-only (`405` for writes). Single-segment names such as `registry.k8s.io/pause` are valid on mirror routes.

::: danger Slug collisions
Because the prefixed form wins, do not create an OCI feed whose slug equals a top-level namespace of an image you mirror. A feed called `library` would capture `library/alpine`, and a feed called `myorg` would capture `ghcr.io/myorg/...` mirror requests.
:::

With the Helm chart, set the variables through `extraEnv`:

```yaml
extraEnv:
  ANT_OCI_DEFAULT_FEED: dockerhub
  ANT_OCI_MIRROR_MAP: "docker.io=dockerhub,ghcr.io=ghcr,quay.io=quay,registry.k8s.io=k8s,gcr.io=gcr,public.ecr.aws=ecr"
```

## Anonymous reads

dockerd never sends credentials to a mirror, and node-level configuration often has no good place for a token. Set `anonymous_read: true` on the mirror feeds. Then `/v2/` and all reads on those feeds are served without a `401` challenge, as the internal `anonymous-reader` principal. That principal has only `read`, and only on `anonymous_read` feeds. Any write still gets `401`.

- Anonymous `GET /v2/` answers `200 {}` only when the default feed (or the feed mapped from `?ns=`) is `anonymous_read`.
- A feed without `anonymous_read` keeps the Basic challenge. Mirror clients then need credentials. containerd, CRI-O, Podman and BuildKit can supply them. dockerd cannot.
- A feed's client-authentication policy can restrict anonymous reads to named network groups (CIDR lists). See [package client auth](../identity/client-auth.md).
- Disabling the `anonymous-reader` principal turns anonymous reads off globally.

::: warning
Ant does not rate-limit anonymous reads. Restrict the listener, or the feed's anonymous network groups, to trusted networks.
:::

## Which runtime, which mode

| Runtime                                | Sends `ns` | Mirrors registries other than Docker Hub | Recommended setup                                          |
| -------------------------------------- | ---------- | ---------------------------------------- | ---------------------------------------------------------- |
| [containerd, k3s, RKE2](containerd.md) | Yes        | Yes                                      | Mirror map on one hostname, or `rewrite` to prefixed feeds |
| [CRI-O](cri-o.md)                      | No         | Yes                                      | Mirror `location` with the feed prefix                     |
| [Docker daemon](docker.md)             | No         | No, Docker Hub only                      | `ANT_OCI_DEFAULT_FEED` pointing at the Docker Hub feed     |
| [Podman, Buildah, Skopeo](podman.md)   | No         | Yes                                      | Mirror `location` with the feed prefix                     |
| [BuildKit / buildx](buildkit.md)       | Yes        | Yes                                      | Mirror map                                                 |

Before rolling out mirrors on many nodes, distribute your CA if Ant uses a private one ([CA trust distribution](ca-trust.md)), and read what happens when Ant is unreachable ([fallback behaviour](fallback.md)).

## Create a mirror feed

Example for Docker Hub, using the [management API](../reference/api.md):

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-remote-1' \
  -d '{"name":"dockerhub-remote","kind":"remote","format":"oci"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: dockerhub-remote-cfg-1' -d '{
  "origin":"https://registry-1.docker.io",
  "redirect_origins":["https://auth.docker.io","https://production.cloudflare.docker.com","https://production.cloudfront.docker.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":86400,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-feed-1' -d @- <<JSON
{"slug":"dockerhub","format":"oci","anonymous_read":true,"source_ids":["$REM"]}
JSON
```

A mirror feed needs no publish policy, because it has no local source and mirror routes are read-only.

Authenticated Docker Hub pulls avoid the anonymous rate limit. Store a Basic credential on the source with `PUT /api/v1/sources/{id}/remote-credential`. Ant sends it only to the token realm (`auth.docker.io`). See [OCI / Docker](../formats/oci.md) for credentials and the other upstreams.

## Verify

```sh
crane manifest ant.example.com/dockerhub/library/alpine:3     # prefixed
crane manifest ant.example.com/library/alpine:3               # mirror mode, default feed
curl -s "https://ant.example.com/v2/library/alpine/manifests/3?ns=docker.io" \
  -H 'Accept: application/vnd.oci.image.index.v1+json'        # mirror mode, ns-mapped
```

::: warning Limits

- Tags are cached for the source's metadata TTL. On a cold cache, a tag `HEAD` costs one upstream `GET`.
- Docker Hub names without a slash (`alpine`) are fetched upstream as `library/alpine`. Reads of the short name are canonicalised to `library/alpine` unless a repository with the short name already exists in the feed.
- A token realm outside the source origin and its redirect origins is refused, not followed.
- Mirror routing is covered by HTTP-level integration tests in the repository (`internal/oci/mirror_test.go`). The runtime configurations in this section follow each runtime's documented syntax, but the repository contains no automated end-to-end test that drives containerd, CRI-O, dockerd, Podman or BuildKit against a mirror. Verify on one node before rolling out.

:::
