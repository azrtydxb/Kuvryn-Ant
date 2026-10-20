# CA trust distribution

If Ant's TLS certificate is issued by a private CA, every client that talks to it must trust that CA: container runtimes on nodes, CI runners, and the package managers inside builds. This page lists where each component looks for CA certificates and how to roll a CA out.

The examples assume the CA certificate is a PEM file named `ant-ca.crt`. TLS for Ant itself (ingress, Gateway API, cert-manager) is configured as described in [ingress, Gateway API and TLS](../install/ingress-tls.md).

::: tip
A certificate from a public CA avoids all of this. Use a private CA only when Ant cannot have a publicly trusted name.
:::

## Operating system trust stores

Most tools fall back to the system store, so start here.

::: code-group

```sh [Debian / Ubuntu]
sudo cp ant-ca.crt /usr/local/share/ca-certificates/ant-ca.crt   # must end in .crt
sudo update-ca-certificates
# bundle: /etc/ssl/certs/ca-certificates.crt
```

```sh [RHEL / Fedora / Rocky]
sudo cp ant-ca.crt /etc/pki/ca-trust/source/anchors/ant-ca.crt
sudo update-ca-trust
# bundle: /etc/pki/tls/certs/ca-bundle.crt
```

```sh [Alpine]
sudo cp ant-ca.crt /usr/local/share/ca-certificates/ant-ca.crt
sudo update-ca-certificates
# bundle: /etc/ssl/certs/ca-certificates.crt (needs the ca-certificates package)
```

```sh [SUSE / SLE Micro]
sudo cp ant-ca.crt /etc/pki/trust/anchors/ant-ca.crt
sudo update-ca-certificates
```

:::

Long-running daemons read the store at startup. Restart containerd, CRI-O or dockerd afterwards.

## Container runtimes

| Runtime                        | Per-registry CA location                                                                                                                  | Notes                                                        |
| ------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------ |
| containerd (generic)           | `ca = "<file>"` on the host in `/etc/containerd/certs.d/<registry>/hosts.toml`, commonly `/etc/containerd/certs.d/ant.example.com/ca.crt` | Also uses the system store. See [containerd](containerd.md). |
| k3s / RKE2                     | `configs."ant.example.com".tls.ca_file` in `registries.yaml`                                                                              | Restart k3s / RKE2 after the change                          |
| CRI-O, Podman, Buildah, Skopeo | `/etc/containers/certs.d/ant.example.com/ca.crt` (rootless: `~/.config/containers/certs.d/...`)                                           | Directory named after `host[:port]`                          |
| Docker                         | `/etc/docker/certs.d/ant.example.com/ca.crt`                                                                                              | No restart needed for `certs.d`                              |
| BuildKit                       | `[registry."ant.example.com"] ca = ["<file>"]` in `buildkitd.toml`                                                                        | See [BuildKit](buildkit.md)                                  |

## Kubernetes nodes

### Node bootstrap (preferred)

Install the CA as part of node provisioning (cloud-init, Ignition, an image build or configuration management), before the kubelet starts pulling images. For example, with cloud-init:

```yaml
#cloud-config
write_files:
  - path: /usr/local/share/ca-certificates/ant-ca.crt
    permissions: "0644"
    content: |
      -----BEGIN CERTIFICATE-----
      <PEM>
      -----END CERTIFICATE-----
runcmd:
  - update-ca-certificates
  - systemctl restart containerd
```

Many managed Kubernetes services also expose a node-level setting for custom CAs. Prefer that setting where it exists.

### DaemonSet

On existing nodes, a privileged DaemonSet can copy the CA into the runtime's per-registry directory. Per-registry directories are re-read on each pull, so this needs no daemon restart. The pattern is the same as the CRI-O mirror DaemonSet in [CRI-O](cri-o.md#rolling-it-out-with-a-daemonset), with these paths:

```yaml
# containerd: mount the host's /etc/containerd/certs.d and copy to
#   /etc/containerd/certs.d/ant.example.com/ca.crt  (referenced by hosts.toml)
# CRI-O:      /etc/containers/certs.d/ant.example.com/ca.crt
# Docker:     /etc/docker/certs.d/ant.example.com/ca.crt
volumes:
  - name: certs-d
    hostPath:
      path: /etc/containerd/certs.d
      type: DirectoryOrCreate
```

::: warning
Such a DaemonSet writes to every node's filesystem with elevated privileges. The warnings on the [CRI-O page](cri-o.md#rolling-it-out-with-a-daemonset) apply. Writing to the OS trust store from a pod also requires restarting the runtime. Do that through node provisioning instead.
:::

### Pods that call Ant

Workloads that call Ant directly (package installs in init containers, CI jobs running in pods) need the CA inside the container. Mount it from a ConfigMap and point the runtime at it:

```yaml
volumes:
  - name: ant-ca
    configMap:
      name: ant-ca # key: ca.crt
containers:
  - name: build
    volumeMounts:
      - name: ant-ca
        mountPath: /etc/ant-ca
        readOnly: true
    env:
      - name: SSL_CERT_FILE
        value: /etc/ant-ca/ca.crt
```

`SSL_CERT_FILE` replaces the default bundle for OpenSSL-based tools, so the file must also contain any public CAs the job needs. Concatenate the system bundle and the Ant CA into one ConfigMap key if the job also talks to public hosts. Tools such as trust-manager can maintain such bundles in the cluster.

## Language runtimes and package managers

Many clients do not use the system store, or replace it entirely when configured:

| Client                                                     | Setting                                                                                     | Replaces or adds              |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------------------- | ----------------------------- |
| OpenSSL-based tools (curl, git, Ruby, PHP and many others) | `SSL_CERT_FILE=/path/bundle.pem` (or `SSL_CERT_DIR`)                                        | Replaces                      |
| curl                                                       | `CURL_CA_BUNDLE=/path/bundle.pem`, or `--cacert`                                            | Replaces                      |
| git                                                        | `git config --global http.sslCAInfo /path/bundle.pem`, or `GIT_SSL_CAINFO`                  | Replaces                      |
| Node.js, npm, pnpm, Yarn                                   | `NODE_EXTRA_CA_CERTS=/path/ant-ca.crt`                                                      | Adds to Node's built-in store |
| npm                                                        | `npm config set cafile /path/bundle.pem` (`cafile=` in `.npmrc`)                            | Replaces                      |
| Python requests (and tools built on it)                    | `REQUESTS_CA_BUNDLE=/path/bundle.pem`                                                       | Replaces                      |
| pip                                                        | `PIP_CERT=/path/bundle.pem` or `cert = /path/bundle.pem` in `pip.conf`                      | Replaces                      |
| uv                                                         | `SSL_CERT_FILE`, or `UV_NATIVE_TLS=true` to use the system store                            | Replaces / system             |
| Poetry                                                     | `poetry config certificates.<repo>.cert /path/bundle.pem`                                   | Per repository                |
| Go (modules, `GOPROXY`)                                    | System store on Linux; `SSL_CERT_FILE` is honoured                                          | System or replaces            |
| Cargo                                                      | System store by default; `CARGO_HTTP_CAINFO=/path/bundle.pem`                               | Replaces                      |
| .NET / NuGet                                               | System store on Linux (`update-ca-certificates`)                                            | System                        |
| Helm                                                       | `--ca-file` on `helm repo add` / `helm pull`, or the system store                           | Per command                   |
| Java (Maven, Gradle, sbt)                                  | Import into the JVM trust store with `keytool` (below), or `-Djavax.net.ssl.trustStore=...` | Adds or replaces              |

Java example:

```sh
# JDK 9+: the default trust store is $JAVA_HOME/lib/security/cacerts, password "changeit"
sudo keytool -importcert -noprompt -cacerts -storepass changeit \
  -alias ant-ca -file ant-ca.crt

# Or a separate store for one build:
keytool -importcert -noprompt -keystore ant-truststore.jks -storepass changeit \
  -alias ant-ca -file ant-ca.crt
export MAVEN_OPTS="-Djavax.net.ssl.trustStore=$PWD/ant-truststore.jks -Djavax.net.ssl.trustStorePassword=changeit"
```

On Debian, Ubuntu and RHEL, the distribution JDK packages read the system trust store, so `update-ca-certificates` / `update-ca-trust` also covers Java.

## Upstreams with private CAs

The other direction, Ant trusting a private upstream registry, is configured on the Ant side. The operator lists CA IDs in `ANT_UPSTREAM_CA_MAP_FILE`, and a remote source references one with `ca_ref`. Requests can never supply a CA path. See [feeds and sources](../concepts/feeds-and-sources.md#remote-source-configuration) and [configuration](../reference/configuration.md).
