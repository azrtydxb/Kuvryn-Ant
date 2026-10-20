# Actions Runner Controller

[Actions Runner Controller](https://github.com/actions/actions-runner-controller) (ARC) runs GitHub Actions runners as pods in your Kubernetes cluster. Running Ant in the same cluster (or next to it) turns every runner pod into a client of a nearby cache: container images through a registry mirror, packages through Ant feeds, and build caches through [cache feeds](../build-caches/index.md).

This page describes how to wire ARC runner scale sets to Ant. Ant itself has no ARC-specific code: everything here is ordinary client configuration of containerd, Docker, BuildKit and package managers.

::: warning Verification status
The `azrtydxb/ant-setup-action` self-test workflows run on ARC runner scale sets (both directly on the runner and inside job `container:` images), so the OIDC exchange, package manager configuration, buildx cache and sccache paths described in [GitHub Actions](github-actions.md) work from ARC runners. The runner-template snippets on this page (dind daemon flags, environment variables in the scale set template, CoreDNS rewrites) are standard ARC and Kubernetes configuration and are **not** exercised by the Kuvryn Ant repository's tests. Check them against the ARC version you run.
:::

## Two kinds of traffic

| Traffic                                                                                           | Who pulls                                                                                                          | Where to configure Ant                                                                                                                           |
| ------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| Runner and job images (`ghcr.io/actions/actions-runner`, `container:` images, service containers) | The node's container runtime (containerd or CRI-O) in `kubernetes` mode; the dind sidecar's dockerd in `dind` mode | Node-level registry mirror ([containerd](../host-caching/containerd.md), [CRI-O](../host-caching/cri-o.md)) or dockerd flags in the dind sidecar |
| Packages and build caches inside steps (`npm ci`, `cargo build`, `docker buildx build`)           | Processes inside the runner or job container                                                                       | Environment variables and config files in the job, written by `ant-setup-action` or set in the runner template                                   |

## Container modes

ARC scale sets run jobs in one of two container modes.

### kubernetes mode

The runner pod uses container hooks to start each `container:` job and service container as a separate pod. Those pods are pulled by the **node's** container runtime, so the node's registry mirror configuration applies to them, exactly as for any other workload:

- containerd: `hosts.toml` (or k3s/RKE2 `registries.yaml`) pointing `docker.io`, `ghcr.io` and others at `https://ant.example.com`, with `ANT_OCI_MIRROR_MAP` on the Ant side. See [containerd](../host-caching/containerd.md).
- The mirror feeds should allow anonymous reads (containerd can also send credentials from its config).

Docker is not available inside the runner in this mode, so `docker build` steps need a separate builder (for example a remote BuildKit deployment or a buildx Kubernetes driver). Point that builder's `buildkitd.toml` at Ant as described in [BuildKit / buildx](../host-caching/buildkit.md).

### dind mode

The runner pod gets a `docker:dind` sidecar, and `docker` commands in steps talk to that daemon. Its image pulls do not use the node's containerd mirror. Configure the sidecar's dockerd instead:

- **Mirror.** dockerd can mirror only Docker Hub: start it with `--registry-mirror=https://ant.example.com` and set `ANT_OCI_DEFAULT_FEED` to the Docker Hub feed (with anonymous reads, because dockerd never sends credentials to a mirror). Other registries need prefixed names such as `ant.example.com/ghcr/org/img`. See [Docker daemon](../host-caching/docker.md).
- **buildx.** The default `docker` driver uses the daemon's mirror. A `docker-container` builder (created by `docker/setup-buildx-action`) runs its own BuildKit with its own resolver; give it a `buildkitd.toml` with `[registry."docker.io"] mirrors = ["ant.example.com"]` through the action's `buildkitd-config` or `buildkitd-config-inline` input. See [BuildKit / buildx](../host-caching/buildkit.md).

A sketch of the dind sidecar in the `gha-runner-scale-set` chart values (the exact template layout depends on the chart version; the chart's own default dind container is replaced when you define it yourself):

```yaml
template:
  spec:
    initContainers:
      - name: dind
        image: docker:dind
        restartPolicy: Always
        args:
          - dockerd
          - --host=unix:///var/run/docker.sock
          - --group=$(DOCKER_GROUP_GID)
          - --registry-mirror=https://ant.example.com
        env:
          - name: DOCKER_GROUP_GID
            value: "123"
        securityContext:
          privileged: true
        volumeMounts:
          - name: dind-sock
            mountPath: /var/run
          - name: ant-ca
            mountPath: /etc/docker/certs.d/ant.example.com
            readOnly: true
```

## CA trust

If Ant's certificate is issued by a private CA, every client must trust it:

| Client                                | Where the CA goes                                                                                                       |
| ------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Node containerd                       | `hosts.toml` `ca = "/etc/containerd/certs.d/ant.example.com/ca.crt"` (see [containerd](../host-caching/containerd.md))  |
| dockerd in the dind sidecar           | `/etc/docker/certs.d/ant.example.com/ca.crt` in the sidecar (mounted from a ConfigMap or Secret)                        |
| BuildKit (`docker-container` builder) | `[registry."ant.example.com"] ca = ["/etc/ssl/ant-ca.pem"]` in `buildkitd.toml`, with the file available to the builder |
| npm, pip, cargo, go, curl in steps    | the job image's system trust store, or `NODE_EXTRA_CA_CERTS`, `PIP_CERT`, `CARGO_HTTP_CAINFO`, `SSL_CERT_FILE`          |

Distributing the CA to nodes is covered in [CA trust distribution](../host-caching/ca-trust.md). A publicly trusted certificate avoids all of this and is the simplest choice for runners.

## Package managers in the runner template

The recommended way is [`ant-setup-action`](github-actions.md) in each job: it uses OIDC, needs no stored secret and writes the right files into the job's `$HOME` (also inside a `container:` job, as long as the action runs in that container).

For read-only consumption of feeds that allow anonymous reads, you can instead set environment variables on the runner container so every job inherits them without any workflow change:

```yaml
template:
  spec:
    containers:
      - name: runner
        image: ghcr.io/actions/actions-runner:latest
        command: ["/home/runner/run.sh"]
        env:
          - name: GOPROXY
            value: https://ant.example.com/feeds/go/go|https://proxy.golang.org,direct
          - name: npm_config_registry
            value: https://ant.example.com/feeds/npm/npm/
          - name: PIP_INDEX_URL
            value: https://ant.example.com/feeds/pypi/pypi/simple/
          - name: UV_DEFAULT_INDEX
            value: https://ant.example.com/feeds/pypi/pypi/simple/
```

Notes:

- These URLs work without credentials only on feeds with `anonymous_read`. For authenticated feeds, use the action, or mount a service token from a Kubernetes Secret and reference it in the variables; a token in the runner template is shared by every job of the scale set, including pull requests, so scope it to read-only.
- Variables on the runner container reach steps that run directly on the runner. Whether they reach `container:` jobs depends on the container mode and ARC version; the repository does not verify this. Prefer the action inside job containers.
- Cargo source replacement (`[source.crates-io] replace-with`) is configured in a `config.toml`, not through the variables above. Bake it into the runner image or let the action write it. See [Cargo](../formats/cargo.md).

## Cluster-internal URL versus public URL

Runners in the same cluster can reach Ant through its Service (for example `http://ant.ant.svc.cluster.local:8080`) or through its public URL (`https://ant.example.com`). Use the **public name** wherever possible, and make it resolve to an in-cluster path if you want to keep traffic local.

Reasons:

- **Absolute URLs in metadata.** Several adapters build absolute URLs from `ANT_PUBLIC_URL`: npm tarball URLs in packuments, the Cargo `config.json` `dl` and `api` URLs, and the metadata of NuGet, Composer, pub, Conan, Conda, Ansible, CRAN, APK, APT, RPM, Terraform, Swift, CocoaPods, Hugging Face, P2 and Git LFS. A client that fetches metadata through the internal Service URL is still sent to the public URL for downloads, so it must be able to reach (and trust) that URL anyway.
- **The action requires HTTPS.** `ant-setup-action` takes a host name and always uses `https://<host>`; it also requests the OIDC audience `https://<host>`, which must match the audience configured in Ant.
- **TLS.** The Service is plain HTTP inside the cluster; package managers refuse or need extra flags for non-HTTPS registries, and container runtimes treat it as an insecure registry.

To keep traffic inside the cluster with the public name:

- **Split-horizon DNS.** Resolve `ant.example.com` inside the cluster to the in-cluster ingress or gateway address (for example a CoreDNS `rewrite` or `hosts` entry, or `hostAliases` in the runner template). TLS still terminates at the ingress with the public certificate.
- **Node-level mirror.** containerd on the nodes talks to whatever `ant.example.com` resolves to on the node; the node's resolver must give the same answer.

The OIDC exchange itself does not depend on the URL used to reach Ant: Ant compares the token's audience with its configured audience, not with the request host.

## OIDC from ARC runners

ARC runners are registered with GitHub like any self-hosted runner, so GitHub issues OIDC ID tokens to their jobs (`ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`) when the workflow has `permissions: id-token: write`. Nothing on the runner side is required beyond:

- egress from the runner pod to `token.actions.githubusercontent.com` / the Actions service (which the runner needs anyway) and to Ant;
- the trust rules on Ant described in [GitHub OIDC trust](../identity/github-oidc.md).

Trust rules match the repository, ref, environment and workflow, not the runner, so the same rules cover GitHub-hosted and ARC runners.

## Client addresses and throttling

Ant throttles failed authentication per client address. When runner traffic reaches Ant through an ingress controller or gateway, set `ANT_TRUSTED_PROXIES` (Helm value `trustedProxies`) to the proxy pod CIDRs so Ant reads the client address from `X-Forwarded-For`. Otherwise every runner shares the proxy's address, and one misconfigured job can throttle the others. With a `Service` of `externalTrafficPolicy: Cluster`, clients arrive SNAT'd to a node address; `Local` preserves them. See [Ingress, Gateway API and TLS](../install/ingress-tls.md).

## Build caches from ARC

Ephemeral runner pods start with an empty Docker and Cargo state, which is exactly the case remote caches help with:

- buildx registry cache in an OCI cache feed: [buildx registry cache](../build-caches/buildx.md);
- sccache over WebDAV in a raw cache feed: [sccache over WebDAV](../build-caches/sccache.md).

Both are configured by `ant-setup-action` with `build-cache: "true"`. Reported timings from the project's own ARC runners are in [Measurements](../build-caches/measurements.md).

::: warning Gap

- No ARC runner template, dind configuration or CoreDNS rewrite is part of the Kuvryn Ant repository or tested by it; the snippets above are generic.
- Propagation of runner-container environment variables into `container:` jobs is not verified.
- There is no separate "internal URL" setting in Ant: `ANT_PUBLIC_URL` is the only origin used in generated metadata.
  :::
