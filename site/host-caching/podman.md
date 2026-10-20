# Podman, Buildah and Skopeo

Podman, Buildah and Skopeo share the `containers/image` library and its `registries.conf` configuration with [CRI-O](cri-o.md). A registry entry lists mirrors, and a mirror `location` may include a path, so each upstream maps onto one prefixed Ant feed. These tools do not send containerd's `?ns=` parameter, so `ANT_OCI_MIRROR_MAP` is not involved.

## Configuration files

| Scope                           | Files                                                                                                       |
| ------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| System-wide (root and rootless) | `/etc/containers/registries.conf`, drop-ins in `/etc/containers/registries.conf.d/*.conf`                   |
| Per user (rootless)             | `$HOME/.config/containers/registries.conf`, drop-ins in `$HOME/.config/containers/registries.conf.d/*.conf` |

If a per-user `registries.conf` exists, it replaces the system `registries.conf` for that user. It is not merged. Drop-in files are applied on top of the main file in lexical order, so adding a drop-in is the least disruptive change. The `registries` section of `podman info` shows which entries are actually in effect for a user. The `CONTAINERS_REGISTRIES_CONF` environment variable points a single command at another file.

## Mirror drop-in

`/etc/containers/registries.conf.d/50-ant-mirror.conf`, or `~/.config/containers/registries.conf.d/50-ant-mirror.conf` for one user:

```toml
[[registry]]
prefix = "docker.io"
location = "docker.io"

[[registry.mirror]]
location = "ant.example.com/dockerhub"

[[registry]]
prefix = "ghcr.io"
location = "ghcr.io"

[[registry.mirror]]
location = "ant.example.com/ghcr"

[[registry]]
prefix = "quay.io"
location = "quay.io"

[[registry.mirror]]
location = "ant.example.com/quay"
```

No daemon is involved. The next `podman pull` reads the new configuration.

```sh
podman pull docker.io/library/alpine:3
podman --log-level=debug pull docker.io/library/alpine:3 2>&1 | grep -i 'trying to access'
```

The debug output lists each location tried, with the Ant mirror first.

## Short names

`podman pull alpine` uses short-name resolution (`unqualified-search-registries` and short-name aliases), and in `enforcing` mode may prompt for a registry. The mirror applies after a short name has been resolved to a fully qualified name such as `docker.io/library/alpine`. Use fully qualified names in scripts and CI.

## Private CA

Place the CA under the certificate directory for the Ant host:

| Scope         | Path                                                      |
| ------------- | --------------------------------------------------------- |
| System        | `/etc/containers/certs.d/ant.example.com/ca.crt`          |
| Rootless user | `$HOME/.config/containers/certs.d/ant.example.com/ca.crt` |

See [CA trust distribution](ca-trust.md).

## Credentials

If the mirror feeds are not `anonymous_read`, log in. Podman stores credentials in `${XDG_RUNTIME_DIR}/containers/auth.json` by default. Use `--authfile` or `REGISTRY_AUTH_FILE` for a persistent location.

```sh
echo "$ANT_TOKEN" | podman login ant.example.com -u x --password-stdin
```

Credentials keyed by `ant.example.com` apply to every path under that host, including `ant.example.com/dockerhub`. Skopeo and Buildah use the same auth file.

## Buildah and Skopeo

Both honour the same `registries.conf` mirrors:

```sh
buildah from docker.io/library/alpine:3          # base image pulled via the mirror
skopeo inspect docker://docker.io/library/alpine:3
```

`skopeo copy` to a destination does not use mirrors, because mirrors apply to pulls only. To copy images into a hosted Ant feed, address the feed directly:

```sh
skopeo copy docker://docker.io/library/alpine:3 docker://ant.example.com/oci-hosted/base/alpine:3
```

## Fallback

When every mirror fails, the primary `location` is used, so pulls continue directly from the upstream while Ant is unreachable. See [fallback behaviour](fallback.md).
