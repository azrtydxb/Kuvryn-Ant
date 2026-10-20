# CRI-O

CRI-O pulls images through the `containers/image` library, which reads `/etc/containers/registries.conf` and the drop-in directory `/etc/containers/registries.conf.d/`. A registry entry can list mirrors, and a mirror `location` may include a path. This maps directly onto Ant's prefixed feed route, so no Ant-side mirror variables are needed.

`containers/image` does not send containerd's `?ns=` parameter. Use one prefixed feed per upstream (`ant.example.com/dockerhub`, `ant.example.com/ghcr`, and so on) rather than `ANT_OCI_MIRROR_MAP`.

## Drop-in configuration

Create `/etc/containers/registries.conf.d/50-ant-mirror.conf`:

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

[[registry]]
prefix = "registry.k8s.io"
location = "registry.k8s.io"

[[registry.mirror]]
location = "ant.example.com/k8s"
```

A pull of `docker.io/library/alpine:3` is first tried as `ant.example.com/dockerhub/library/alpine:3`, which is `https://ant.example.com/v2/dockerhub/library/alpine/...`. If every mirror fails, the primary `location` (the upstream) is used.

| Key                                    | Meaning                                                                                          |
| -------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `prefix`                               | Which image references the entry applies to                                                      |
| `location`                             | The primary registry, tried after all mirrors                                                    |
| `[[registry.mirror]] location`         | A mirror. Mirrors are tried in order, and a path is allowed.                                     |
| `[[registry.mirror]] insecure`         | `true` allows plain HTTP or an unverified certificate. Use it only for tests.                    |
| `[[registry.mirror]] pull-from-mirror` | `all` (default), `digest-only` or `tag-only`. Restricts which references the mirror is used for. |
| `[[registry]] blocked`                 | `true` refuses pulls for the prefix entirely                                                     |

Drop-in files are read in lexical order, and later files override earlier entries with the same `prefix`. Keep all mirror entries for one prefix in one file.

::: tip
If the node's `registries.conf` sets `short-name-mode = "enforcing"`, short names such as `alpine` are not resolved silently. Kubernetes manifests normally use fully qualified names, so this rarely matters for CRI-O.
:::

## Apply the change

CRI-O reloads its registries configuration on `SIGHUP`:

```sh
systemctl reload crio        # sends SIGHUP
journalctl -u crio -n 20     # look for the configuration reload message
```

If your CRI-O version does not reload registries on `SIGHUP`, restart it (`systemctl restart crio`). Running containers keep running across a CRI-O restart, because each container is supervised by its own `conmon` process. Pods cannot be created or pulled during the restart.

## Private CA and credentials

`containers/image` looks for CA certificates in `/etc/containers/certs.d/<host>[:port]/`:

```
/etc/containers/certs.d/ant.example.com/ca.crt
```

See [CA trust distribution](ca-trust.md) for rolling this out to nodes.

For feeds without `anonymous_read`, CRI-O uses the pod's `imagePullSecrets` or a global auth file set in `/etc/crio/crio.conf` (or a drop-in under `/etc/crio/crio.conf.d/`):

```toml
[crio.image]
global_auth_file = "/etc/crio/auth.json"
```

```json
{ "auths": { "ant.example.com": { "auth": "<base64 of x:TOKEN>" } } }
```

A node-wide token in a plain file is readable by anyone with root on the node. Prefer `anonymous_read` mirror feeds, limited to cluster networks through anonymous network groups ([package client auth](../identity/client-auth.md)).

## Rolling it out with a DaemonSet

On clusters without node configuration management, a privileged DaemonSet can write the drop-in onto every node and reload CRI-O. The manifest below:

1. stores the drop-in (and optionally a CA certificate) in a ConfigMap
2. runs an init container on every node that copies the files into the host's `/etc/containers` through `hostPath` mounts
3. reloads CRI-O only when a file changed, using `nsenter` into the host's PID 1 namespaces
4. keeps a `pause` container running so the pod stays scheduled

::: danger Read before applying
This DaemonSet runs **privileged, in the host PID namespace, with write access to the host's `/etc/containers`**. Anyone who can change its ConfigMap or image can control how every node pulls images, or take over the node. Deploy it in a namespace only cluster administrators can write to.

A broken `registries.conf` drop-in can stop image pulls on every node at once. Roll out to one node first (uncomment the `nodeSelector`), check `crictl pull`, then widen.

On OpenShift, do not use this. Configure mirrors with `ImageTagMirrorSet` / `ImageDigestMirrorSet` resources, which the Machine Config Operator applies safely.

The manifest has not been tested against a live CRI-O cluster as part of this project.
:::

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: ant-node-config
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: ant-registries
  namespace: ant-node-config
data:
  50-ant-mirror.conf: |
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
  # Optional: private CA for ant.example.com. Leave empty to skip.
  ca.crt: ""
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ant-registries
  namespace: ant-node-config
  labels:
    app: ant-registries
spec:
  selector:
    matchLabels:
      app: ant-registries
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
  template:
    metadata:
      labels:
        app: ant-registries
      annotations:
        # Change this value (for example to a hash of the ConfigMap) to roll out
        # a new configuration; the init container runs again on every node.
        ant.example.com/config-revision: "1"
    spec:
      hostPID: true
      priorityClassName: system-node-critical
      tolerations:
        - operator: Exists
      # nodeSelector:
      #   kubernetes.io/hostname: <first-node>
      initContainers:
        - name: install
          image: docker.io/library/debian:bookworm-slim # any image with sh, cmp and nsenter
          securityContext:
            privileged: true
          command:
            - /bin/sh
            - -c
            - |
              set -eu
              changed=0
              src=/config/50-ant-mirror.conf
              dst=/host/registries.conf.d/50-ant-mirror.conf
              if ! cmp -s "$src" "$dst"; then
                cp "$src" "$dst.tmp" && mv "$dst.tmp" "$dst"
                changed=1
              fi
              if [ -s /config/ca.crt ]; then
                mkdir -p /host/certs.d/ant.example.com
                if ! cmp -s /config/ca.crt /host/certs.d/ant.example.com/ca.crt; then
                  cp /config/ca.crt /host/certs.d/ant.example.com/ca.crt
                  changed=1
                fi
              fi
              if [ "$changed" = 1 ]; then
                echo "configuration changed, reloading CRI-O"
                nsenter --target 1 --mount --uts --ipc --net --pid -- systemctl reload crio
              else
                echo "configuration unchanged"
              fi
          volumeMounts:
            - name: config
              mountPath: /config
              readOnly: true
            - name: registries-d
              mountPath: /host/registries.conf.d
            - name: certs-d
              mountPath: /host/certs.d
      containers:
        - name: pause
          image: registry.k8s.io/pause:3.10
          resources:
            requests:
              cpu: 1m
              memory: 4Mi
            limits:
              memory: 16Mi
      volumes:
        - name: config
          configMap:
            name: ant-registries
        - name: registries-d
          hostPath:
            path: /etc/containers/registries.conf.d
            type: DirectoryOrCreate
        - name: certs-d
          hostPath:
            path: /etc/containers/certs.d
            type: DirectoryOrCreate
```

Notes:

- The DaemonSet's own images (`debian`, `pause`) are pulled before the mirror is configured. Keep them pullable directly, or pre-load them.
- Updating the ConfigMap alone does nothing. Bump `ant.example.com/config-revision` (or let your deployment tool set a checksum annotation) so the pods are recreated and the init container runs again.
- Deleting the DaemonSet does **not** remove the files from the nodes. Remove `/etc/containers/registries.conf.d/50-ant-mirror.conf` and reload CRI-O to undo the change.
- Some immutable node operating systems manage `/etc/containers` themselves and may revert or reject the change. Use the vendor's node configuration mechanism there.

## Verify

```sh
crictl pull docker.io/library/alpine:3
journalctl -u crio | grep -i 'ant.example.com'   # with log_level = "debug", CRI-O logs each mirror attempt
```

On Ant, the image now appears in the `dockerhub` feed. Check it with `GET /api/v1/feeds/{id}/resolve?coordinate=library/alpine`.

## Fallback

If `ant.example.com` is unreachable or answers with an error, `containers/image` tries the next mirror and finally the primary location. Nodes keep pulling from the upstream while Ant is down. See [fallback behaviour](fallback.md).
