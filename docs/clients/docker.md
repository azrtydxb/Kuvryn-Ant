# Docker / OCI

Pull-through mirrors (`dockerhub`, `ghcr`, `quay`, `k8s`, ...) are anonymous; configure `registry-mirrors` or containerd `hosts.toml`.
Hosted pushes use the `oci-hosted` feed with a publish-scoped token:

```
echo "$ANT_TOKEN" | docker login ant.example.com -u x --password-stdin
docker buildx build --platform linux/amd64,linux/arm64 --push -t ant.example.com/oci-hosted/team/app:1.0 .
```

Private CA: place it in `/etc/docker/certs.d/ant.example.com/ca.crt`.
GitHub Actions: `docker/login-action` with `registry: ant.example.com`, `username: x`, `password: ${{ secrets.ANT_TOKEN }}`.

Build caches (`--cache-to`/`--cache-from type=registry`) belong in a cache feed, not in `oci-hosted`: see `docs/operations/build-caches.md`.
