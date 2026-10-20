# BuildKit / buildx

BuildKit pulls base images (`FROM ...`) through containerd's resolver. When it sends a request to a mirror, it appends `?ns=<upstream host>`, so one Ant hostname with `ANT_OCI_MIRROR_MAP` can mirror Docker Hub, `ghcr.io` and other upstreams for builds. Mirrors are configured in `buildkitd.toml`.

This page covers pulling base images through Ant. Storing layer caches in Ant (`--cache-to type=registry`) is a separate topic, described in [buildx registry cache](../build-caches/buildx.md).

## Ant side

```sh
ANT_OCI_MIRROR_MAP=docker.io=dockerhub,ghcr.io=ghcr,quay.io=quay,registry.k8s.io=k8s
```

Each mapped feed is an OCI feed with a remote source and `anonymous_read: true`. See the [overview](index.md).

## buildkitd.toml

```toml
[registry."docker.io"]
  mirrors = ["ant.example.com"]

[registry."ghcr.io"]
  mirrors = ["ant.example.com"]

[registry."quay.io"]
  mirrors = ["ant.example.com"]

# TLS settings for the mirror host itself (only needed for a private CA)
[registry."ant.example.com"]
  ca = ["/etc/buildkit/certs/ant-ca.pem"]
```

| Key                               | Meaning                                                              |
| --------------------------------- | -------------------------------------------------------------------- |
| `[registry."<upstream>"] mirrors` | Mirror hosts tried before the upstream. Give a host name, not a URL. |
| `[registry."<mirror>"] ca`        | CA bundle files used to verify the mirror                            |
| `http = true`                     | Plain HTTP to that host. Only for tests.                             |
| `insecure = true`                 | Skip TLS verification for that host. Only for tests.                 |

Standalone `buildkitd` reads `/etc/buildkit/buildkitd.toml` (rootless: `~/.config/buildkit/buildkitd.toml`). Restart it after changes.

::: warning Untested
The repository documents the host-only mirror form (`mirrors = ["ant.example.com"]`) together with `ANT_OCI_MIRROR_MAP`. Whether BuildKit accepts a mirror with a path (for example `ant.example.com/dockerhub`, to use a prefixed feed without the mirror map) depends on the BuildKit version and has not been verified against Ant.
:::

## docker buildx

The default `docker` driver uses the BuildKit embedded in dockerd. That BuildKit follows dockerd's [`registry-mirrors`](docker.md) and ignores `buildkitd.toml`, so it only mirrors Docker Hub. To use per-registry mirrors, create a builder with the `docker-container` (or `kubernetes`) driver and pass the configuration:

```sh
docker buildx create --name ant --driver docker-container \
  --buildkitd-config ./buildkitd.toml --use
docker buildx inspect --bootstrap
```

Older buildx releases call the flag `--config`. The configuration is copied into the builder when the builder is created. To change it, remove the builder (`docker buildx rm ant`) and create it again. buildx also copies the files named in `ca` into the builder container.

## GitHub Actions

`docker/setup-buildx-action` accepts the configuration inline:

```yaml
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: docker/setup-buildx-action@v3
        with:
          buildkitd-config-inline: |
            [registry."docker.io"]
              mirrors = ["ant.example.com"]
            [registry."ghcr.io"]
              mirrors = ["ant.example.com"]

      - uses: docker/build-push-action@v6
        with:
          context: .
          push: false
          tags: myorg/myrepo:ci
```

For a private CA, write the certificate to a file on the runner in an earlier step and reference it with `[registry."ant.example.com"] ca = ["/path/on/runner/ant-ca.pem"]`. Self-hosted runners in the same network as Ant benefit most. GitHub-hosted runners reach Ant over the internet. Credentials for publishing and for cache feeds come from `docker/login-action` or the GitHub OIDC exchange, as described in [GitHub Actions](../ci/github-actions.md).

## Credentials

Mirror feeds should be `anonymous_read`. BuildKit can authenticate to registries with credentials from the client's Docker configuration (`docker login ant.example.com`).

::: warning Gap
Whether BuildKit presents `ant.example.com` credentials when it reaches Ant as a mirror of another registry has not been verified against Ant. Use anonymous mirror feeds for base-image pulls.
:::

## Fallback

BuildKit uses containerd's resolver: if the mirror fails, the upstream registry is tried next, and builds continue without Ant. See [fallback behaviour](fallback.md).

## Verify

```sh
docker buildx build --builder ant --progress=plain --no-cache - <<'EOF'
FROM docker.io/library/alpine:3
RUN true
EOF
```

Afterwards, `library/alpine` is cached in the `dockerhub` feed. Check it with `GET /api/v1/feeds/{id}/resolve?coordinate=library/alpine` or in the console.
