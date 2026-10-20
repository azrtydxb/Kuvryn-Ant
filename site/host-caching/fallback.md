# Fallback behaviour

Two kinds of failure matter for a pull-through mirror: **Ant itself is unreachable**, or **Ant is up but an upstream registry fails**. The first is handled by the client runtime. The second is handled by Ant's caches.

## When Ant is unreachable

Every supported runtime treats the mirror as optional and keeps the upstream as the last resort:

| Runtime                        | Behaviour when the mirror fails                                                                                                                     |
| ------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| containerd (generic)           | Hosts in `hosts.toml` are tried in order, then `server` (the upstream). containerd logs `trying next host`.                                         |
| k3s / RKE2                     | As containerd. The upstream "default endpoint" is always tried last, unless `disable-default-registry-endpoint` is set on releases that support it. |
| CRI-O, Podman, Buildah, Skopeo | `[[registry.mirror]]` entries are tried in order, then the primary `location`                                                                       |
| Docker                         | Falls back to Docker Hub. There is no option to make the mirror mandatory.                                                                          |
| BuildKit                       | Uses containerd's resolver: mirrors first, then the upstream                                                                                        |

The consequences:

- Nodes need egress to the upstreams for fallback to work. During an Ant outage, they pull directly and count against upstream rate limits again.
- To force every pull through Ant (for policy, audit or air-gapped networks), block direct egress at the network level, and on k3s/RKE2 disable the default endpoint. Pulls of images that Ant cannot serve then fail instead of silently bypassing it.
- A fallback is silent from the workload's point of view. Watch the runtime logs or the upstream traffic to notice it.

::: warning Gap
The fallback behaviour above is the runtimes' documented behaviour. It has not been exercised against Ant in this repository. In particular, containerd's handling differs between manifest resolution and blob fetches: blob fetches may fall back only on not-found and fail on other errors. Test an Ant outage on a staging node before relying on fallback.
:::

## When an upstream fails

Ant separates **immutable** content (blobs, manifests by digest, package archives) from **mutable** metadata (tags, indexes, version lists).

### Immutable content

Anything already cached is an ordinary local artifact. It is served from blob storage without contacting the upstream, through any outage and across restarts. **Pulling by digest** (`image@sha256:...`) is therefore fully resilient once the image has been pulled through Ant once.

### Mutable metadata (OCI tags and package indexes)

Tags and indexes go through the remote source's metadata cache:

| Cache state                                      | Upstream fails | Result                                                                                                                                                                        |
| ------------------------------------------------ | -------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Fresh (younger than `metadata_ttl_seconds`)      | Not contacted  | Served normally                                                                                                                                                               |
| Expired, within `stale_if_error_seconds`         | Error          | Served from the stale entry with `X-Kuvryn-Cache: stale` and `Warning: 110 - "Response is Stale"`                                                                             |
| Expired beyond the stale window, or never cached | Error          | **OCI:** `503 UNKNOWN` for a tag. **Other formats:** a degraded listing built from cached artifacts, marked `X-Kuvryn-Cache: offline`, or `503` if nothing is cached locally. |

`X-Kuvryn-Cache: offline` is set by adapters that compose listings from the store, for example Maven, Go, Cargo, Composer, Conda, Conan, CRAN, RubyGems and Ansible. Per-format details are on each [format page](../formats/index.md).

::: warning Metadata caches live in memory
The metadata, stale and negative caches are held in process memory. They are not shared between instances and are **lost when Ant restarts**. If Ant restarts during an upstream outage, a tag such as `alpine:3` can no longer be resolved (`503`) even though every blob of the image is cached. Pulls by digest keep working. Ant records the tags it has resolved as observed tags for discovery, but they are never used to answer a pull.
:::

### Errors versus not-found

| Upstream answer                                       | Ant's behaviour                                                                                                                                                        |
| ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `404` / `410`                                         | Falls through to the next remote source in the feed's read order, and is remembered in the negative cache for `negative_ttl_seconds`                                   |
| `429`                                                 | OCI: passed to the client as `429 TOOMANYREQUESTS` with the upstream `Retry-After`. It does not open the circuit breaker.                                              |
| Connection error, timeout, `5xx`, refused token realm | The search **stops**, and the client gets `503`. A later public source is never used to answer for a failing earlier one. See [read order](../concepts/resolution.md). |

Connection failures and `502`/`503`/`504` responses are retried up to three attempts with a short backoff before Ant reports the error. Repeated failures open the source's circuit breaker, which by default opens after 3 consecutive failures for 30 seconds. While the circuit is open, requests to that upstream fail fast, and stale or offline serving applies. Source health (`status` `healthy`, `degraded`, `failing` or `unknown`, failure count, last error and last status code) is shown by `GET /api/v1/sources/{id}/health` and in the console.

A client that gets a `503` from Ant as a mirror normally moves on to the next host or the upstream, as described above. So an upstream outage that Ant cannot cover usually becomes a direct pull attempt from the node, which fails in the same way.

## Response headers

| Header                                     | Meaning                                                                       |
| ------------------------------------------ | ----------------------------------------------------------------------------- |
| `X-Kuvryn-Cache: miss`                     | First download, streamed from the upstream while it is stored                 |
| `X-Kuvryn-Cache: stale` (+ `Warning: 110`) | Served from expired metadata within the stale-if-error window                 |
| `X-Kuvryn-Cache: offline`                  | Upstream unreachable. The listing was composed from locally cached artifacts. |
| `Retry-After`                              | On `429`, copied from the upstream                                            |

## Recommendations for mirror feeds

- Set a generous `stale_if_error_seconds` on mirror sources (up to 86400) so tags keep resolving through longer outages.
- Pin production images by digest. Digest pulls do not depend on metadata caches at all.
- [Prewarm](../operations/prewarm.md) the images a cluster needs (OCI image lists with platforms), so they are cached before the first node asks.
- Keep `redirect_origins` current. A missing CDN host or token realm looks like an upstream failure (`503`) and shows up as an authentication or policy error in source health.
- Run Ant highly available at the network level (stable DNS name, health-checked ingress), but remember that one instance holds the writer lease. Use [replication](../replication/index.md) for additional sites.
