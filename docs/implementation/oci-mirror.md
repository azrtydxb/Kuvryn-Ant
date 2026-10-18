# OCI pull-through mirror

Ant can replace Nexus Docker proxies (docker.io, ghcr.io, quay.io, gcr.io,
registry.k8s.io, public.ecr.aws) for dockerd `registry-mirrors`, buildkit
mirrors and containerd/k3s `registries.yaml`.

## Model

* One OCI feed per upstream, each with a **remote** source pointing at the
  upstream registry origin (see `pull-through.md`).
* Blobs and manifests are cached by digest under the remote source; tags are
  proxied through the bounded metadata cache (TTL, stale-if-error). A tag HEAD is
  answered from the same resolved manifest, so one upstream GET serves the
  HEAD and every later by-digest read (Docker Hub counts only GETs against its
  pull limit).
* Concurrent pulls of the same missing blob are coalesced into one upstream
  fetch.

### Upstream authentication (token handshake)

On `401` with `WWW-Authenticate: Bearer realm=,service=,scope=` the fetcher
requests a token from the realm and retries with `Authorization: Bearer`.

* Tokens are cached per (realm, service, scope) until `expires_in` (default 60s,
  refreshed slightly early). A token the upstream rejects is dropped and fetched
  once more. Concurrent requests share one token fetch.
* A failed token request is remembered for 5 seconds only (so a fixed credential
  or recovered upstream works immediately after); it is never a negative cache
  entry for the artifact.
* If the source has a configured **Basic** credential it is sent to the realm
  (authenticated pulls avoid anonymous rate limits). Otherwise the token is
  requested anonymously. A static `Bearer` credential is sent as-is and no token
  exchange uses it.
* **The realm must be the source origin or one of the source's
  `redirect_origins`.** The credential is sent only to such a realm, the realm
  response is never followed through a redirect, and the bearer token is sent only
  to the source origin, never to a redirected blob/CDN host.
* Docker Hub: single-segment names get the implicit `library/` namespace.
* `429` from the upstream or its token service is surfaced to the client as
  `429 TOOMANYREQUESTS` with the upstream `Retry-After`; it is not retried and
  does not open the upstream circuit breaker.

Redirect origins to configure per upstream (blob hosts and token realms):

| Upstream | origin | redirect_origins |
|---|---|---|
| docker.io | `https://registry-1.docker.io` | `https://auth.docker.io`, `https://production.cloudflare.docker.com` |
| ghcr.io | `https://ghcr.io` | `https://pkg-containers.githubusercontent.com` |
| quay.io | `https://quay.io` | `https://cdn.quay.io`, `https://cdn01.quay.io`, `https://cdn02.quay.io`, `https://cdn03.quay.io` |
| gcr.io | `https://gcr.io` | none usually |
| registry.k8s.io | `https://registry.k8s.io` | `https://prod-registry-k8s-io-<region>.s3.dualstack.<region>.amazonaws.com` (the regions you run in) |
| public.ecr.aws | `https://public.ecr.aws` | `https://d1uvnnlqvlnzwp.cloudfront.net` and similar |

(CDN hostnames change; the access log shows the redirect target if a pull fails
with a policy error.)

## Routing

Existing prefixed routes keep working: `/v2/<feed-slug>/<repo>/...`.

For mirror clients that cannot add a path prefix, configure:

| Variable | Meaning |
|---|---|
| `ANT_OCI_DEFAULT_FEED` | feed slug served for unprefixed `/v2/<name>/...` |
| `ANT_OCI_MIRROR_MAP` | `host=slug,...` choosing the feed from containerd's `?ns=` parameter |

Resolution order for `/v2/<name>/...`:

1. If the first segment of `<name>` is the slug of an existing OCI feed, the
   request is the **prefixed** form for that feed. This always wins, so do not
   create a feed whose slug equals a top-level upstream namespace you mirror
   (for example a feed called `library` would capture `library/alpine`).
2. Otherwise the whole `<name>` is the upstream repository. With `?ns=host`:
   the feed from `ANT_OCI_MIRROR_MAP[host]`; if the map is non-empty and the host
   is not in it the request is refused (never silently answered from another
   upstream); if no map is configured `ANT_OCI_DEFAULT_FEED` is used. Without
   `ns` the default feed is used (dockerd sends no `ns`).
3. Mirror routes are read-only (`405` for writes). Single-segment names
   (`registry.k8s.io/pause`) are valid on mirror routes.

dockerd can mirror only Docker Hub, so it uses `ANT_OCI_DEFAULT_FEED` pointing
at the Docker Hub feed.

## Anonymous reads

dockerd never sends credentials to a mirror. Set `anonymous_read` on the mirror
feed(s). Then `/v2/` and all reads on that feed are served without a `401`
challenge. Anonymous requests run as the internal `anonymous-reader` principal,
which is granted `read` on `anonymous_read` feeds only; any write still gets
`401`. Disable that principal (`principals.disabled`) to turn anonymous reads off
globally. A feed without `anonymous_read` keeps the Basic challenge, so mirror
clients need credentials for it (buildkit/containerd can supply them, dockerd
cannot). Anonymous reads are not rate limited by Ant; restrict the listener to
trusted networks.

## Client configuration

Assume Ant at `https://ant.example.com`.

### dockerd (`/etc/docker/daemon.json`)

```json
{ "registry-mirrors": ["https://ant.example.com"] }
```

With `ANT_OCI_DEFAULT_FEED=dockerhub` and `anonymous_read` on feed `dockerhub`.
Only docker.io pulls use the mirror; other registries need explicit prefixed
names (`ant.example.com/ghcr/org/img`).

### buildkit (`buildkitd.toml`)

```toml
[registry."docker.io"]
  mirrors = ["ant.example.com"]
[registry."ghcr.io"]
  mirrors = ["ant.example.com"]
```

buildkit uses containerd's resolver, which sends `?ns=<upstream host>` to the
mirror, so `ANT_OCI_MIRROR_MAP` selects the right feed per upstream. Add
`http = true` / `insecure = true` only for plain-HTTP or private-CA test setups.

### k3s / containerd (`/etc/rancher/k3s/registries.yaml`)

Namespace map (preferred; one host, many upstreams):

```yaml
mirrors:
  docker.io:
    endpoint: ["https://ant.example.com"]
  ghcr.io:
    endpoint: ["https://ant.example.com"]
  quay.io:
    endpoint: ["https://ant.example.com"]
  registry.k8s.io:
    endpoint: ["https://ant.example.com"]
  gcr.io:
    endpoint: ["https://ant.example.com"]
  public.ecr.aws:
    endpoint: ["https://ant.example.com"]
```

with, on Ant:

```
ANT_OCI_MIRROR_MAP=docker.io=dockerhub,ghcr.io=ghcr,quay.io=quay,registry.k8s.io=k8s,gcr.io=gcr,public.ecr.aws=ecr
```

containerd appends `?ns=<host>` to every request to a mirror endpoint, which
selects the feed. Rewrite approach (no `ns` support needed; uses the prefixed
route):

```yaml
mirrors:
  docker.io:
    endpoint: ["https://ant.example.com"]
    rewrite:
      "^(.*)$": "dockerhub/$1"
  ghcr.io:
    endpoint: ["https://ant.example.com"]
    rewrite:
      "^(.*)$": "ghcr/$1"
```

`rewrite` turns `library/alpine` into `dockerhub/library/alpine`, i.e. the
prefixed route for feed `dockerhub`. Add `configs:` with credentials or a CA if
the mirror is not anonymous or uses a private CA:

```yaml
configs:
  "ant.example.com":
    auth: { username: svc, password: "<ant service token>" }
    tls: { ca_file: /etc/ssl/ant-ca.pem }
```

## Manual verification

```
crane manifest ant.example.com/dockerhub/library/alpine:3      # prefixed
crane manifest ant.example.com/library/alpine:3                # mirror (default feed)
curl -s "https://ant.example.com/v2/library/alpine/manifests/3?ns=docker.io" \
  -H 'Accept: application/vnd.oci.image.index.v1+json'
docker pull alpine:3                                           # with registry-mirrors set
```

## Limitations

* Tag requests are cached for the source's metadata TTL; a tag HEAD costs one
  upstream GET on a cold cache.
* Docker Hub names without a slash are fetched as `library/<name>` upstream but
  cached locally under the name the client used, so `alpine` and
  `library/alpine` are separate (blob-deduplicated) repositories.
* A realm outside the source origin and its redirect origins is refused, not
  followed.
