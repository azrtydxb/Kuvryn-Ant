# containerd (k3s, RKE2, generic)

containerd resolves images through per-registry host lists. When it sends a request to a mirror, it appends `?ns=<upstream host>`. This lets one Ant hostname mirror several upstreams through `ANT_OCI_MIRROR_MAP`. Alternatively, k3s and RKE2 can rewrite repository names onto Ant's prefixed feed paths, which needs no Ant-side mirror configuration.

The Ant side (one OCI feed per upstream, `anonymous_read`, mirror variables) is described in the [host-level caching overview](index.md).

## k3s and RKE2: registries.yaml

k3s and RKE2 read `registries.yaml` at startup and generate containerd's host configuration from it:

| Distribution | File                                | Apply with                                                                  |
| ------------ | ----------------------------------- | --------------------------------------------------------------------------- |
| k3s          | `/etc/rancher/k3s/registries.yaml`  | `systemctl restart k3s` (servers) or `systemctl restart k3s-agent` (agents) |
| RKE2         | `/etc/rancher/rke2/registries.yaml` | `systemctl restart rke2-server` or `systemctl restart rke2-agent`           |

The file must exist on every node that pulls images, including agents.

### Option A: mirror map (one endpoint, many upstreams)

On Ant:

```sh
ANT_OCI_MIRROR_MAP=docker.io=dockerhub,ghcr.io=ghcr,quay.io=quay,registry.k8s.io=k8s,gcr.io=gcr,public.ecr.aws=ecr
```

On every node:

```yaml
mirrors:
  docker.io:
    endpoint:
      - "https://ant.example.com"
  ghcr.io:
    endpoint:
      - "https://ant.example.com"
  quay.io:
    endpoint:
      - "https://ant.example.com"
  registry.k8s.io:
    endpoint:
      - "https://ant.example.com"
  gcr.io:
    endpoint:
      - "https://ant.example.com"
  public.ecr.aws:
    endpoint:
      - "https://ant.example.com"
```

containerd requests `https://ant.example.com/v2/library/alpine/manifests/3?ns=docker.io`, and Ant serves it from feed `dockerhub`. A registry that appears here but not in `ANT_OCI_MIRROR_MAP` is refused by Ant. The node then falls back to the upstream, as described in [fallback behaviour](fallback.md).

### Option B: rewrite to prefixed feeds

`rewrite` maps the repository name before the request is sent, so the request uses Ant's prefixed route for a feed. Ant needs no mirror variables, because a prefixed path always wins.

```yaml
mirrors:
  docker.io:
    endpoint:
      - "https://ant.example.com"
    rewrite:
      "^(.*)$": "dockerhub/$1"
  ghcr.io:
    endpoint:
      - "https://ant.example.com"
    rewrite:
      "^(.*)$": "ghcr/$1"
  registry.k8s.io:
    endpoint:
      - "https://ant.example.com"
    rewrite:
      "^(.*)$": "k8s/$1"
```

`library/alpine` becomes `dockerhub/library/alpine`, which is `https://ant.example.com/v2/dockerhub/library/alpine/...`. The rewrite applies only to the mirror endpoint, not to the upstream fallback.

### Credentials and private CA

If the mirror feeds are not `anonymous_read`, or Ant's certificate is signed by a private CA, add a `configs` entry keyed by the Ant host:

```yaml
configs:
  "ant.example.com":
    auth:
      username: x
      password: "<TOKEN>"
    tls:
      ca_file: /etc/ssl/certs/ant-ca.pem
```

Any username works, and the password is an Ant service token or personal access token with `read` on the mirror feeds. `ca_file` must exist on the node. See [CA trust distribution](ca-trust.md). `insecure_skip_verify: true` exists under `tls`, but use it only for short tests.

::: warning
`registries.yaml` stores the token in plain text on every node. Prefer `anonymous_read` on mirror feeds, restricted to cluster networks through the feed's anonymous network groups ([package client auth](../identity/client-auth.md)), over node-wide tokens.
:::

### Default endpoint fallback

k3s and RKE2 always keep the upstream registry (the "default endpoint") as the last host. If Ant is unreachable or refuses the request, nodes pull directly from the upstream. Recent k3s and RKE2 releases have a `--disable-default-registry-endpoint` flag (`disable-default-registry-endpoint: true` in the config file) that removes this fallback. Check that your release has it before relying on it.

## Generic containerd: hosts.toml

Plain containerd (kubeadm clusters, managed node images) uses a host directory. Point the CRI plugin at it in `/etc/containerd/config.toml`:

::: code-group

```toml [containerd 2.x]
version = 3

[plugins."io.containerd.cri.v1.images".registry]
  config_path = "/etc/containerd/certs.d"
```

```toml [containerd 1.6 / 1.7]
version = 2

[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/etc/containerd/certs.d"
```

:::

Restart containerd (`systemctl restart containerd`) after changing `config.toml`. Files under `config_path` are read when a pull starts, so `hosts.toml` changes take effect without a restart.

Create one directory per upstream registry, named after the registry host as it appears in image references:

```
/etc/containerd/certs.d/
├── docker.io/hosts.toml
├── ghcr.io/hosts.toml
├── registry.k8s.io/hosts.toml
└── ant.example.com/ca.crt        # optional private CA
```

### Mirror map

`/etc/containerd/certs.d/docker.io/hosts.toml`:

```toml
server = "https://registry-1.docker.io"

[host."https://ant.example.com"]
  capabilities = ["pull", "resolve"]
  ca = "/etc/containerd/certs.d/ant.example.com/ca.crt"
```

`/etc/containerd/certs.d/ghcr.io/hosts.toml`:

```toml
server = "https://ghcr.io"

[host."https://ant.example.com"]
  capabilities = ["pull", "resolve"]
  ca = "/etc/containerd/certs.d/ant.example.com/ca.crt"
```

| Key              | Meaning                                                                                              |
| ---------------- | ---------------------------------------------------------------------------------------------------- |
| `server`         | The upstream. containerd tries it after all `host` entries.                                          |
| `[host."<url>"]` | A mirror, tried in file order                                                                        |
| `capabilities`   | `pull` and `resolve` for a read-only mirror. Never add `push`, because mirror routes answer `405`.   |
| `ca`             | CA bundle used to verify this host. Omit it when Ant's certificate chains to the system trust store. |

`ghcr.io` and the others only reach the right feed if `ANT_OCI_MIRROR_MAP` maps them, because containerd sends `?ns=ghcr.io`.

### Prefixed feed with override_path

To use a prefixed feed instead of the mirror map, put the feed path in the host URL and set `override_path`:

```toml
server = "https://registry-1.docker.io"

[host."https://ant.example.com/v2/dockerhub"]
  capabilities = ["pull", "resolve"]
  override_path = true
```

With `override_path = true`, containerd uses the URL path as the API root instead of appending `/v2`. A pull of `docker.io/library/alpine:3` then requests `https://ant.example.com/v2/dockerhub/library/alpine/manifests/3`.

::: warning Untested
The `override_path` form follows containerd's documented `hosts.toml` semantics and maps onto Ant's prefixed route. The repository has no test that runs containerd against it. Verify it on one node first.
:::

### Credentials

`hosts.toml` has no username or password fields. A static header can be set per host:

```toml
[host."https://ant.example.com"]
  capabilities = ["pull", "resolve"]
  [host."https://ant.example.com".header]
    Authorization = ["Basic <base64 of x:TOKEN>"]
```

Generate the value with `printf 'x:%s' "$ANT_TOKEN" | base64`. As with `registries.yaml`, it is stored in plain text. Prefer anonymous mirror feeds.

### nerdctl and ctr

`nerdctl` reads `/etc/containerd/certs.d` by default. `ctr` needs the directory passed explicitly:

```sh
ctr images pull --hosts-dir /etc/containerd/certs.d docker.io/library/alpine:3
```

## Verify

```sh
# Through the CRI (what the kubelet does)
crictl pull docker.io/library/alpine:3

# k3s: inspect the generated host configuration
cat /var/lib/rancher/k3s/agent/etc/containerd/certs.d/docker.io/hosts.toml

# On Ant: the image is now cached in the mirror feed
curl -fsS -G "https://ant.example.com/api/v1/feeds/$FEED/resolve" \
  -H "Authorization: Bearer $ANT_TOKEN" --data-urlencode coordinate=library/alpine
```

containerd logs `trying next host` at info level when it skips a mirror. If you see that for Ant, the pull went to the upstream instead.

## Troubleshooting

| Symptom                                              | Cause                                                                                                                                                                        |
| ---------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Pulls work but bypass Ant                            | Ant was unreachable or refused the request, and containerd fell back to `server` or the default endpoint. Check containerd logs for `trying next host` and Ant's access log. |
| `401` from Ant for an anonymous mirror request       | The `ns` host is not in `ANT_OCI_MIRROR_MAP` (the request is refused, and anonymous requests are challenged), or the feed is not `anonymous_read`.                           |
| `404 NAME_UNKNOWN` / `BLOB_UNKNOWN` with credentials | The `ns` host is not in `ANT_OCI_MIRROR_MAP`, or the image does not exist upstream                                                                                           |
| An image resolves from the wrong feed                | A feed slug equals the first path segment of the image (for example a feed named `library`). Prefixed paths always win.                                                      |
| `503` with `UNKNOWN`                                 | The upstream failed, its circuit is open, or the token handshake was refused because a realm is missing from `redirect_origins`                                              |
| `429 TOOMANYREQUESTS`                                | Upstream rate limit, passed through with `Retry-After`. Add an upstream credential to the remote source.                                                                     |
| x509 errors                                          | The node does not trust Ant's CA. See [CA trust distribution](ca-trust.md).                                                                                                  |
