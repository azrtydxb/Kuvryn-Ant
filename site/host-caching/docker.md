# Docker daemon

dockerd's `registry-mirrors` setting makes the daemon try a mirror before Docker Hub. Two constraints shape the Ant setup:

- **Docker only mirrors Docker Hub.** Pulls from `ghcr.io`, `quay.io` or any other registry never use `registry-mirrors`. For those, use explicit prefixed image names (`ant.example.com/ghcr/myorg/app:1.0`) or a runtime that supports per-registry mirrors ([containerd](containerd.md), [Podman](podman.md)).
- **No path, no credentials.** A mirror URL cannot contain a path, so the feed slug cannot be part of it. dockerd also never sends credentials to a mirror. Ant therefore needs `ANT_OCI_DEFAULT_FEED` pointing at the Docker Hub feed, and that feed needs `anonymous_read`.

## Ant side

```sh
ANT_OCI_DEFAULT_FEED=dockerhub
```

The `dockerhub` feed has a remote source with origin `https://registry-1.docker.io` and `anonymous_read: true`. See the [host-level caching overview](index.md#create-a-mirror-feed). dockerd sends no `ns` parameter, so unprefixed requests always go to the default feed.

## daemon.json

`/etc/docker/daemon.json`:

```json
{
  "registry-mirrors": ["https://ant.example.com"]
}
```

Restart the daemon:

```sh
sudo systemctl restart docker
docker info --format '{{json .RegistryConfig.Mirrors}}'
```

`docker pull alpine:3` now requests `https://ant.example.com/v2/library/alpine/manifests/3`. Docker Desktop has the same setting under Settings, Docker Engine.

## Private CA

Docker reads per-registry CA files from `/etc/docker/certs.d/<host>[:port]/`:

```sh
sudo mkdir -p /etc/docker/certs.d/ant.example.com
sudo cp ant-ca.crt /etc/docker/certs.d/ant.example.com/ca.crt
```

The file must end in `.crt`. No daemon restart is needed for `certs.d` changes. Alternatively, add the CA to the operating system trust store and restart dockerd ([CA trust distribution](ca-trust.md)).

## Testing without TLS

For a throwaway test against an Ant listening on plain HTTP or with an untrusted certificate:

```json
{
  "registry-mirrors": ["http://10.0.0.10:8080"],
  "insecure-registries": ["10.0.0.10:8080"]
}
```

::: danger
`insecure-registries` disables TLS verification for that host. Never use it in production. Anyone on the path can then substitute image content, unless you pull by digest.
:::

## Prefixed pulls and pushes

Everything outside Docker Hub, and all publishing, uses the prefixed form with normal registry authentication:

```sh
echo "$ANT_TOKEN" | docker login ant.example.com -u x --password-stdin
docker pull ant.example.com/ghcr/myorg/app:1.0
docker push ant.example.com/oci-hosted/myorg/app:1.0
```

Mirror routes are read-only. Pushes always go to a prefixed hosted feed. See [OCI / Docker](../formats/oci.md).

## Fallback

If the mirror is unreachable or returns an error, dockerd falls back to Docker Hub (`registry-1.docker.io`). There is no setting to make the mirror mandatory. To force all pulls through Ant, block direct egress to Docker Hub at the network level. Details are in [fallback behaviour](fallback.md).

## Troubleshooting

| Symptom                               | Cause                                                                                                                  |
| ------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| Pulls still hit Docker Hub            | dockerd not restarted, an error from Ant (dockerd fell back silently), or the image is not from Docker Hub             |
| `401` in Ant's access log for `/v2/`  | The default feed is not `anonymous_read`. dockerd will not send credentials to a mirror.                               |
| `404` for every mirrored image        | `ANT_OCI_DEFAULT_FEED` not set, or set to a slug that does not exist                                                   |
| An image resolves from the wrong feed | A feed slug equals the first segment of the image name, for example a feed named `library`. Prefixed paths always win. |
| x509 errors in `journalctl -u docker` | The CA is missing from `/etc/docker/certs.d/ant.example.com/`                                                          |
