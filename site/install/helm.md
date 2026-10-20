# Helm chart

The chart in [`deploy/chart`](https://github.com/azrtydxb/Kuvryn-Ant/tree/main/deploy/chart) deploys one Kuvryn Ant writer with an internal Service, an optional Ingress or Gateway API `HTTPRoute`, a persistent data volume and optional federation, metrics and NetworkPolicy resources. It consumes an existing PostgreSQL database and existing Secrets; it never generates keys or passwords.

## What the chart does and does not create

| Created by the chart                                                 | Not created by the chart                                |
| -------------------------------------------------------------------- | ------------------------------------------------------- |
| `Deployment` (one replica, `Recreate` strategy)                      | PostgreSQL (see [PostgreSQL](postgresql.md))            |
| `Service` (ClusterIP, port `service.port`)                           | Secrets: DSN, root key, S3 credentials, CA, IdP config  |
| `PersistentVolumeClaim` (unless `persistence.existingClaim` is set)  | Buckets and bucket policies (see [Storage](storage.md)) |
| `Ingress` or `HTTPRoute` (optional)                                  | Ingress controller, Gateway, cert-manager issuers       |
| Federation `Service`, passthrough `Ingress` or `TLSRoute` (optional) | DNS records                                             |
| Headless metrics `Service` (optional)                                | ServiceMonitor or scrape configuration                  |
| `NetworkPolicy` (optional)                                           | Container image (see below)                             |

Chart metadata: name `kuvryn-ant`, version `0.1.0`, `kubeVersion: >=1.30.0-0`. Resource names are `<release>-ant`, so a release called `ant` creates `deployment/ant-ant`.

The pod runs as UID/GID 10001 with a read-only root filesystem, all capabilities dropped, `seccompProfile: RuntimeDefault` and no service-account token. Only `/tmp` (an emptyDir) and the data volume at `/var/lib/ant` are writable.

## Prerequisites

- Kubernetes 1.30 or newer and Helm. The planning baseline is Helm 4.3.1; the chart uses only `apiVersion: v2` features.
- PostgreSQL 17 or 18 reachable from the cluster, preferably over verified TLS.
- A block-storage `StorageClass` (or an existing PVC) with ext4 or XFS. NFS, SMB and FUSE object mounts are not supported for the data volume.
- Optional: an S3-compatible bucket, an ingress controller or Gateway API implementation, cert-manager.
- A container image you built and pushed yourself.

::: warning No published image or chart repository yet
There are no GitHub releases, no published container image and no Helm chart repository at this time. Install the chart from a git checkout and build the image yourself.
:::

### Build and push the image

```sh
git clone https://github.com/azrtydxb/Kuvryn-Ant.git
cd Kuvryn-Ant
docker buildx build --platform linux/amd64,linux/arm64 \
  -t registry.example.com/myorg/kuvryn-ant:0.1.0 --push .
```

The [`Dockerfile`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/Dockerfile) builds the console, embeds it in a static Go binary and produces an Alpine-based image that runs as UID 10001. The `NODE_IMAGE`, `GO_IMAGE` and `RUNTIME_IMAGE` build arguments accept digest-pinned base images. Record the pushed digest and set `image.digest` for production rollouts.

## Create the Secrets

Create Secrets from protected files, never from command-line literals or Helm values. Run these commands in the release namespace.

```sh
kubectl create namespace ant
umask 077

# 1. PostgreSQL DSN (key: dsn). See install/postgresql for the full format.
printf '%s' 'postgres://ant:<password>@db.example.com:5432/ant?sslmode=verify-full&sslrootcert=/run/ant/postgres/ca.crt' > dsn
kubectl -n ant create secret generic ant-postgres --from-file=dsn=./dsn

# Optional: CA certificate the DSN refers to (mounted at /run/ant/postgres).
kubectl -n ant create secret generic ant-postgres-tls --from-file=ca.crt=./postgres-ca.crt

# 2. Root key: exactly 32 random raw bytes (key: root.key). Back it up separately.
head -c 32 /dev/urandom > root.key
kubectl -n ant create secret generic ant-root-key --from-file=root.key=./root.key

# 3. Only for storage.backend=s3: credentials file (key: credentials.json).
cat > credentials.json <<'JSON'
{"access_key_id":"<ACCESS_KEY_ID>","secret_access_key":"<SECRET_ACCESS_KEY>"}
JSON
kubectl -n ant create secret generic ant-s3 --from-file=credentials.json=./credentials.json

# Optional: private CA of the S3 endpoint (key: ca.crt).
kubectl -n ant create secret generic ant-s3-ca --from-file=ca.crt=./s3-ca.crt

shred -u dsn credentials.json 2>/dev/null || rm -f dsn credentials.json
```

Keep `root.key` in your secret manager or offline backup before deleting the local copy; see [Root key](root-key.md). Secret managers and controllers (External Secrets, Sealed Secrets, Vault agents) work as long as they produce Secrets with the same names and keys.

All Secrets are mounted as directory projections without `subPath`, so updated S3 credentials are visible to the running process. Changes to the root key, the DSN or identity-provider files require a pod restart.

## Install

```sh
cd Kuvryn-Ant
helm lint deploy/chart
helm template ant deploy/chart -n ant -f my-values.yaml > /dev/null   # render check
helm upgrade --install ant deploy/chart -n ant -f my-values.yaml --wait
```

A minimal `my-values.yaml` for disk storage behind ingress-nginx:

```yaml
image:
  repository: registry.example.com/myorg/kuvryn-ant
  tag: "0.1.0"
  digest: "" # set sha256:... for production
publicURL: https://ant.example.com
persistence:
  size: 200Gi
  storageClass: "" # cluster default; must be block storage
ingress:
  enabled: true
  className: nginx
  host: ant.example.com
  tlsSecret: ant-tls
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

Complete, commented examples:

- [`deploy/examples/cloud-s3.yaml`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/deploy/examples/cloud-s3.yaml): cloud Kubernetes, AWS S3 (or any S3), ingress-nginx and cert-manager, storage-pressure eviction, trusted proxies.
- [`deploy/examples/onprem-minio.yaml`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/deploy/examples/onprem-minio.yaml): on-premises, MinIO with a private CA, Gateway API `HTTPRoute`, NetworkPolicy with explicit egress.

The chart validates its values twice: `values.schema.json` (types, enums, ranges) and template checks that fail rendering with a clear message, for example `ingress.host must exactly match publicURL` or `S3 requires region, bucket and credentialsExistingSecret`.

## Bootstrap the first administrator

A fresh database has no instance. The pod starts, passes readiness (readiness checks only PostgreSQL) and serves the console and API, but no package endpoints. Create the instance and the first administrator credential once:

```sh
kubectl -n ant exec deploy/ant-ant -- ant bootstrap
```

The command prints JSON with `instance_id`, `principal_id`, `token_id`, `token` and `expires_at`. The token is an administrative service token valid for 24 hours. It is printed once; store it privately and do not run bootstrap from automation that logs output (never as a Helm hook).

The running server notices the new instance within about 2 seconds and reloads in-process to start serving package endpoints. The repository's deploy guide also restarts the pod afterwards, which is harmless:

```sh
kubectl -n ant rollout restart deploy/ant-ant
```

If the token is lost or expires before you create other administrators, issue a new one (previous tokens are not revoked):

```sh
kubectl -n ant exec deploy/ant-ant -- ant bootstrap-token
```

Use the token to sign in to the console at `https://ant.example.com/console/` and to call the API. Next steps: [create a feed](../guide/quick-start.md#first-feed-in-2-minutes), [local users and identity providers](../identity/index.md).

For an existing instance (reinstall, restore), skip bootstrap. Keep PostgreSQL, the root key and the data volume.

## Single writer

This release supports exactly one serving process per instance.

- `replicaCount` must be `1` (schema `const: 1`, and the template fails otherwise). The Deployment always uses `strategy: Recreate`, so an upgrade stops the old pod before the new one starts. Expect a short outage on every upgrade.
- At startup the process acquires a writer lease for its instance in PostgreSQL. A second process against the same instance cannot take the lease while it is held, and every metadata write is fenced by it. This is a safety net, not a scaling mechanism: do not scale the Deployment or run a second release against the same database.
- `terminationGracePeriodSeconds` must be at least `shutdownSeconds + 10`.
- Probes: startup `/health/live` (60 x 5 s, five minutes for migrations), liveness `/health/live`, readiness `/health/ready` (PostgreSQL ping). Neither probe checks S3 or identity providers.

## Upgrade

```sh
git -C Kuvryn-Ant pull
# build and push the new image, then:
helm upgrade ant Kuvryn-Ant/deploy/chart -n ant -f my-values.yaml --wait
```

Migrations run at startup, are forward-only and are serialized by a PostgreSQL advisory lock. An image rollback does not undo them. Take a backup to object storage before upgrading, never onto the data volume. See [Upgrade](../operations/upgrade.md) and [Backup and restore](../operations/backup-restore.md).

## Uninstall

`helm uninstall ant -n ant` removes the workload, but the chart-created PVC carries `helm.sh/resource-policy: keep` and is left in place. To reinstall onto it, set `persistence.existingClaim: ant-ant`. Deleting the PVC, the database and the root key is a manual, deliberate step.

## Values reference

Every key in [`values.yaml`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/deploy/chart/values.yaml). Types and constraints come from [`values.schema.json`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/deploy/chart/values.schema.json) and the template checks in `_helpers.tpl`. The "Env" column names the variable the value sets; see the [configuration reference](../reference/configuration.md) for runtime ranges.

### Image and pod

| Key                         | Type                                   | Default        | Env | Description                                                                                                                                          |
| --------------------------- | -------------------------------------- | -------------- | --- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| `image.repository`          | string (non-empty)                     | `kuvryn-ant`   |     | Image repository.                                                                                                                                    |
| `image.tag`                 | string (non-empty)                     | `"0.1.0"`      |     | Image tag, used when `image.digest` is empty.                                                                                                        |
| `image.digest`              | string, empty or `sha256:<64 hex>`     | `""`           |     | Immutable digest; when set the image is referenced as `repository@digest`. Recommended for production.                                               |
| `image.pullPolicy`          | enum `Always`, `IfNotPresent`, `Never` | `IfNotPresent` |     | Image pull policy.                                                                                                                                   |
| `imagePullSecrets`          | list                                   | `[]`           |     | Pull secrets for a private registry, for example `[{name: regcred}]`.                                                                                |
| `hostAliases`               | list                                   | `[]`           |     | Static `/etc/hosts` entries, for example a federation member only resolvable at its own site: `[{ip: 10.0.0.20, hostnames: [fed.ant.example.com]}]`. |
| `podAnnotations`            | map                                    | `{}`           |     | Annotations on the pod template, for example a backup tool excluding the data volume.                                                                |
| `replicaCount`              | integer, must be `1`                   | `1`            |     | Single writer; any other value fails.                                                                                                                |
| `nodeSelector`              | map                                    | `{}`           |     | Pod node selector.                                                                                                                                   |
| `tolerations`               | list                                   | `[]`           |     | Pod tolerations.                                                                                                                                     |
| `affinity`                  | map                                    | `{}`           |     | Pod affinity rules.                                                                                                                                  |
| `resources.requests.cpu`    | quantity                               | `"1"`          |     | CPU request.                                                                                                                                         |
| `resources.requests.memory` | quantity                               | `2Gi`          |     | Memory request.                                                                                                                                      |
| `resources.limits.cpu`      | quantity                               | `"4"`          |     | CPU limit. Also drives the CPU-scaled concurrency defaults through `GOMAXPROCS`.                                                                     |
| `resources.limits.memory`   | quantity                               | `8Gi`          |     | Memory limit.                                                                                                                                        |

### Server

| Key                             | Type                                                     | Default                       | Env                    | Description                                                                                                                                                              |
| ------------------------------- | -------------------------------------------------------- | ----------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `publicURL`                     | string, `^https://[^/?#@]+$`                             | `https://ant.example.invalid` | `ANT_PUBLIC_URL`       | External HTTPS origin without path. Must equal `https://<ingress.host>` when ingress is enabled and its host must be in `httpRoute.hostnames` when the route is enabled. |
| `instanceID`                    | string                                                   | `""`                          | `ANT_INSTANCE_ID`      | Explicit instance UUID. Empty uses the bootstrapped instance.                                                                                                            |
| `shutdownSeconds`               | integer 1 to 60                                          | `15`                          | `ANT_SHUTDOWN_TIMEOUT` | Request drain time on SIGTERM, rendered as `<n>s`.                                                                                                                       |
| `terminationGracePeriodSeconds` | integer, at least 11 and at least `shutdownSeconds + 10` | `45`                          |                        | Pod termination grace period.                                                                                                                                            |
| `stagingDir`                    | string                                                   | `/var/lib/ant/tmp`            | `TMPDIR`               | Upload staging directory on the data volume.                                                                                                                             |
| `tmp.sizeLimit`                 | quantity                                                 | `256Mi`                       |                        | Size limit of the `/tmp` emptyDir (scratch only).                                                                                                                        |
| `maxRequests`                   | integer 0 to 4096                                        | `0`                           | `ANT_MAX_REQUESTS`     | Concurrent requests; `0` keeps the CPU-scaled default (32 per CPU, 128 to 1024).                                                                                         |
| `dbMaxConns`                    | integer 0 to 500                                         | `0`                           | `ANT_DB_MAX_CONNS`     | PostgreSQL pool size; `0` keeps the default (8 per CPU, 16 to 64). The runtime rejects values below 4.                                                                   |
| `uploadSlots`                   | integer 0 to 256                                         | `0`                           | `ANT_UPLOAD_SLOTS`     | Concurrent uploads per format; `0` keeps the default (2 per CPU, 4 to 32).                                                                                               |
| `trustedProxies`                | list of non-empty strings                                | `[]`                          | `ANT_TRUSTED_PROXIES`  | CIDRs or addresses of the ingress controller or gateway pods whose forwarding headers are trusted. Empty trusts none.                                                    |
| `extraEnv`                      | map of string to string                                  | `{}`                          | any `ANT_*`            | Additional plain environment variables. Names must start with `ANT_`. Do not repeat variables the chart sets. Never put secrets here.                                    |

### PostgreSQL

| Key                          | Type              | Default        | Env                | Description                                                                                                                                                    |
| ---------------------------- | ----------------- | -------------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `postgres.existingSecret`    | string (required) | `ant-postgres` | `ANT_DATABASE_URL` | Secret holding the DSN.                                                                                                                                        |
| `postgres.dsnKey`            | string            | `dsn`          |                    | Key of the DSN in that Secret.                                                                                                                                 |
| `postgres.tlsExistingSecret` | string            | `""`           |                    | Optional Secret mounted read-only at `/run/ant/postgres` (whole Secret), for example `ca.crt` referenced by `sslrootcert=/run/ant/postgres/ca.crt` in the DSN. |

### Root key

| Key                               | Type                                   | Default        | Env                                     | Description                                                                                                                                     |
| --------------------------------- | -------------------------------------- | -------------- | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| `rootKey.existingSecret`          | string, non-empty                      | `ant-root-key` |                                         | Secret mounted at `/run/ant/root` (whole Secret).                                                                                               |
| `rootKey.key`                     | string, `^[A-Za-z0-9][A-Za-z0-9_.-]*$` | `root.key`     | `ANT_ROOT_KEY_FILE=/run/ant/root/<key>` | File name of the active 32-byte key inside the Secret.                                                                                          |
| `rootKey.activeVersion`           | string, non-empty                      | `v1`           | `ANT_ROOT_KEY_VERSION`                  | Version label of the active key.                                                                                                                |
| `rootKey.previousKeysManifestKey` | string, empty or file-name pattern     | `""`           | `ANT_PREVIOUS_ROOT_KEYS_FILE`           | File name of a JSON manifest in the same Secret mapping previous versions to key file names, used during rotation. See [Root key](root-key.md). |

### Persistence

| Key                         | Type                                                       | Default           | Env | Description                                                                            |
| --------------------------- | ---------------------------------------------------------- | ----------------- | --- | -------------------------------------------------------------------------------------- |
| `persistence.existingClaim` | string                                                     | `""`              |     | Use an existing PVC instead of creating one.                                           |
| `persistence.storageClass`  | string                                                     | `""`              |     | StorageClass of the created PVC. Empty uses the cluster default. Choose block storage. |
| `persistence.size`          | quantity                                                   | `120Gi`           |     | Requested size. See [Sizing](sizing.md).                                               |
| `persistence.accessModes`   | list of exactly one of `ReadWriteOnce`, `ReadWriteOncePod` | `[ReadWriteOnce]` |     | PVC access mode.                                                                       |
| `persistence.annotations`   | map                                                        | `{}`              |     | Extra PVC annotations. `helm.sh/resource-policy: keep` is always added.                |

### Blob storage

| Key                                    | Type              | Default            | Env                                                    | Description                                                                                                                                      |
| -------------------------------------- | ----------------- | ------------------ | ------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `storage.backend`                      | enum `disk`, `s3` | `disk`             | `ANT_STORAGE_BACKEND`                                  | Blob backend. The `ANT_S3_*` variables and credential mounts are rendered only for `s3`.                                                         |
| `storage.s3.endpoint`                  | string            | `""`               | `ANT_S3_ENDPOINT`                                      | S3 API endpoint. Empty uses AWS endpoint resolution. Must start with `https://` unless `allowInsecureHTTP` is true and it starts with `http://`. |
| `storage.s3.region`                    | string            | `""`               | `ANT_S3_REGION`                                        | Region; required for `s3`.                                                                                                                       |
| `storage.s3.bucket`                    | string            | `""`               | `ANT_S3_BUCKET`                                        | Existing bucket; required for `s3`.                                                                                                              |
| `storage.s3.prefix`                    | string            | `ant/`             | `ANT_S3_PREFIX`                                        | Key prefix.                                                                                                                                      |
| `storage.s3.pathStyle`                 | boolean           | `false`            | `ANT_S3_PATH_STYLE`                                    | Path-style addressing (MinIO, OCI Object Storage).                                                                                               |
| `storage.s3.allowInsecureHTTP`         | boolean           | `false`            | `ANT_S3_ALLOW_INSECURE_HTTP`                           | Permit a plaintext `http://` endpoint (trusted networks only).                                                                                   |
| `storage.s3.credentialsExistingSecret` | string            | `""`               | `ANT_S3_CREDENTIALS_FILE=/run/ant/s3/credentials.json` | Secret with the credentials JSON; required for `s3`.                                                                                             |
| `storage.s3.credentialsKey`            | string            | `credentials.json` |                                                        | Key in that Secret.                                                                                                                              |
| `storage.s3.caExistingSecret`          | string            | `""`               | `ANT_S3_CA_FILE=/run/ant/s3-ca/ca.crt`                 | Optional Secret with the endpoint's CA certificate.                                                                                              |
| `storage.s3.caKey`                     | string            | `ca.crt`           |                                                        | Key in that Secret.                                                                                                                              |

### Browser identity providers

Each provider mounts its whole Secret at `/run/ant/<provider>`; paths inside the JSON config must use that mount root. Both keys of a provider block are required by the schema (empty `existingSecret` disables the provider).

| Key                        | Type                      | Default       | Env                    | Description                                                 |
| -------------------------- | ------------------------- | ------------- | ---------------------- | ----------------------------------------------------------- |
| `auth.oidc.existingSecret` | string                    | `""`          | `ANT_OIDC_CONFIG_FILE` | Secret with the OIDC config and referenced files.           |
| `auth.oidc.configKey`      | string, file-name pattern | `config.json` |                        | Config file name in the Secret.                             |
| `auth.saml.existingSecret` | string                    | `""`          | `ANT_SAML_CONFIG_FILE` | Secret with the SAML config, key, certificate and metadata. |
| `auth.saml.configKey`      | string, file-name pattern | `config.json` |                        | Config file name.                                           |
| `auth.ldap.existingSecret` | string                    | `""`          | `ANT_LDAP_CONFIG_FILE` | Secret with the LDAP config, bind password and CA.          |
| `auth.ldap.configKey`      | string, file-name pattern | `config.json` |                        | Config file name.                                           |

### Upstream CA bundles

A remote source can trust a private CA by naming it (`ca_ref`); the IDs and PEM files are operator-supplied and an API request can never give a path. Mount one Secret or one ConfigMap (CA certificates are public, so a ConfigMap is fine) that holds both the map file and the PEM bundles. Setting either value sets `ANT_UPSTREAM_CA_MAP_FILE` and mounts the object read-only at `/run/ant/upstream-ca`.

| Key                            | Type                      | Default       | Env                        | Description                                                                        |
| ------------------------------ | ------------------------- | ------------- | -------------------------- | ---------------------------------------------------------------------------------- |
| `upstreamCA.existingSecret`    | string                    | `""`          | `ANT_UPSTREAM_CA_MAP_FILE` | Secret with the map file and the PEM files. Mutually exclusive with the ConfigMap. |
| `upstreamCA.existingConfigMap` | string                    | `""`          | `ANT_UPSTREAM_CA_MAP_FILE` | ConfigMap with the map file and the PEM files.                                     |
| `upstreamCA.mapKey`            | string, file-name pattern | `ca-map.json` |                            | Name of the map file inside the Secret or ConfigMap.                               |

The map is a JSON object of CA ID to PEM file name. Relative names resolve against the map file's directory, so for a ConfigMap named `upstream-ca` with keys `ca-map.json` and `corp-ca.pem`:

```sh
kubectl -n ant create configmap upstream-ca \
  --from-file=ca-map.json=ca-map.json --from-file=corp-ca.pem=corp-ca.pem
helm upgrade --install ant ./deploy/chart -n ant -f values.yaml \
  --set upstreamCA.existingConfigMap=upstream-ca
```

with `ca-map.json` containing `{"corp-ca":"corp-ca.pem"}`. A remote source then sets `"ca_ref":"corp-ca"` in its configuration. The map holds at most 32 entries and 64 KiB. Changing the map or a PEM file needs a pod restart.

### Observability

| Key                                 | Type                                                | Default         | Env                     | Description                                                                                                                  |
| ----------------------------------- | --------------------------------------------------- | --------------- | ----------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| `observability.tracing.enabled`     | boolean                                             | `false`         | `ANT_OTEL_ENABLED`      | Enable OpenTelemetry tracing. The other tracing values are rendered only when true.                                          |
| `observability.tracing.endpoint`    | string, empty or `http(s)://` URL without user info | `""`            | `ANT_OTEL_ENDPOINT`     | OTLP endpoint, for example `http://otel-collector.observability:4318`.                                                       |
| `observability.tracing.protocol`    | enum `http/protobuf`, `grpc`                        | `http/protobuf` | `ANT_OTEL_PROTOCOL`     | OTLP protocol.                                                                                                               |
| `observability.tracing.serviceName` | string, non-empty                                   | `kuvryn-ant`    | `ANT_OTEL_SERVICE_NAME` | Service name.                                                                                                                |
| `observability.tracing.sampleRatio` | number, greater than 0, at most 1                   | `1`             | `ANT_OTEL_SAMPLE_RATIO` | Parent-based sampling ratio.                                                                                                 |
| `metrics.headlessService`           | boolean                                             | `false`         |                         | Creates a headless `<release>-ant-metrics` Service on port 8080 for scraping `/metrics`. Restrict access with NetworkPolicy. |

### Maintenance and retention

| Key                                 | Type                                   | Default | Env                              | Description                                                                                                                                           |
| ----------------------------------- | -------------------------------------- | ------- | -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maintenance.gcInterval`            | string `0` or `<n>m`/`<n>h`            | `6h`    | `ANT_GC_INTERVAL`                | Scheduled maintenance interval; `"0"` disables it. Runtime range 1m to 720h.                                                                          |
| `maintenance.gcGrace`               | string `<n>m`/`<n>h`                   | `24h`   | `ANT_GC_GRACE`                   | Minimum age of an unreferenced blob before GC marks it (1m to 2160h).                                                                                 |
| `maintenance.defaultPolicy.maxIdle` | string, empty or `<n>h`                | `""`    | `ANT_RETENTION_DEFAULT_MAX_IDLE` | Instance default for remote-cached content: delete when not accessed for this long. Empty leaves the console setting in control.                      |
| `maintenance.defaultPolicy.maxAge`  | string, empty or `<n>h`                | `""`    | `ANT_RETENTION_DEFAULT_MAX_AGE`  | Instance default maximum age of remote-cached content.                                                                                                |
| `maintenance.pressure.enabled`      | boolean                                | `false` | `ANT_PRESSURE_ENABLED`           | When true, forces pressure eviction on and renders the threshold and capacity values. When false nothing is rendered and the console setting applies. |
| `maintenance.pressure.highPercent`  | integer 2 to 99                        | `85`    | `ANT_PRESSURE_HIGH_PERCENT`      | Usage that starts eviction.                                                                                                                           |
| `maintenance.pressure.lowPercent`   | integer 1 to 98, below `highPercent`   | `75`    | `ANT_PRESSURE_LOW_PERCENT`       | Projected usage at which eviction stops.                                                                                                              |
| `maintenance.pressure.capacity`     | string, empty or size such as `500GiB` | `""`    | `ANT_STORAGE_CAPACITY_BYTES`     | Capacity quota. Required for object storage; overrides the filesystem size on disk. Minimum 1 MiB.                                                    |
| `maintenance.pressure.interval`     | string `0` or `<n>m`/`<n>h`            | `5m`    | `ANT_PRESSURE_INTERVAL`          | Periodic pressure check; always rendered. `"0"` disables it.                                                                                          |
| `maintenance.pressure.sweepGrace`   | string `<n>m`/`<n>h`                   | `15m`   | `ANT_PRESSURE_SWEEP_GRACE`       | Wait before a blob freed by eviction is swept (1m to 24h); always rendered.                                                                           |
| `auditRetention.retain`             | string `0` or `<n>h`                   | `9600h` | `ANT_AUDIT_RETENTION`            | Audit event retention (400 days). `"0"` keeps events forever. Runtime range 24h to 175200h.                                                           |
| `auditRetention.purgeInterval`      | string `<n>m`/`<n>h`                   | `1h`    | `ANT_AUDIT_PURGE_INTERVAL`       | Purge job interval (1m to 720h).                                                                                                                      |

### Service, Ingress and Gateway API

| Key                     | Type                                                                                | Default               | Env | Description                                                                                                        |
| ----------------------- | ----------------------------------------------------------------------------------- | --------------------- | --- | ------------------------------------------------------------------------------------------------------------------ |
| `service.port`          | integer                                                                             | `8080`                |     | Port of the ClusterIP Service (target port is always the container's `http` port 8080).                            |
| `ingress.enabled`       | boolean                                                                             | `false`               |     | Create an Ingress.                                                                                                 |
| `ingress.className`     | string                                                                              | `""`                  |     | `ingressClassName`; empty uses the cluster default class.                                                          |
| `ingress.host`          | string                                                                              | `ant.example.invalid` |     | Host; must exactly match `publicURL`.                                                                              |
| `ingress.tlsSecret`     | string                                                                              | `""`                  |     | TLS Secret name; required when ingress is enabled.                                                                 |
| `ingress.annotations`   | map                                                                                 | `{}`                  |     | Controller annotations. The chart sets no body-size or timeout annotations; see [Ingress and TLS](ingress-tls.md). |
| `httpRoute.enabled`     | boolean                                                                             | `false`               |     | Create a Gateway API `HTTPRoute` (TLS is terminated by the Gateway).                                               |
| `httpRoute.annotations` | map                                                                                 | `{}`                  |     | Route annotations.                                                                                                 |
| `httpRoute.parentRefs`  | list of objects with `name` (required), `namespace`, `sectionName`, `group`, `kind` | `[]`                  |     | Gateway listeners to attach to; required when enabled.                                                             |
| `httpRoute.hostnames`   | list of non-empty strings                                                           | `[]`                  |     | Must include the host of `publicURL`.                                                                              |
| `httpRoute.filters`     | list of objects                                                                     | `[]`                  |     | Gateway API filters applied to the application rule.                                                               |
| `httpRoute.timeouts`    | object with optional `request`, `backendRequest` (strings)                          | `{}`                  |     | Route timeouts; `"0s"` disables the timeout on implementations that support it.                                    |

Both the Ingress and the HTTPRoute route exactly `/` (redirected to `/console/`) and the prefixes `/console`, `/api`, `/auth`, `/v2`, `/feeds` and `/mcp`. `/metrics` and `/health/*` are not routed.

### Federation

| Key                                           | Type                                         | Default        | Env                                          | Description                                                                                           |
| --------------------------------------------- | -------------------------------------------- | -------------- | -------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `federation.enabled`                          | boolean                                      | `false`        | `ANT_FEDERATION_LISTEN=0.0.0.0:<port>`       | Enable the dedicated mTLS federation listener and its Service.                                        |
| `federation.port`                             | integer 1 to 65535                           | `8444`         |                                              | Container port of the listener; must differ from `service.port`.                                      |
| `federation.publicURL`                        | string, empty or `https://host[:port]`       | `""`           | `ANT_FEDERATION_PUBLIC_URL`                  | URL other members use; required when enabled.                                                         |
| `federation.routing.maxStreams`               | integer, at least 0                          | `0`            | `ANT_FEDERATION_ROUTE_MAX_STREAMS`           | 0 keeps the default 64.                                                                               |
| `federation.routing.streamsPerPrincipal`      | integer, at least 0                          | `0`            | `ANT_FEDERATION_ROUTE_STREAMS_PER_PRINCIPAL` | 0 keeps the default 8.                                                                                |
| `federation.routing.streamsPerClient`         | integer, at least 0                          | `0`            | `ANT_FEDERATION_ROUTE_STREAMS_PER_CLIENT`    | 0 keeps the default 16.                                                                               |
| `federation.routing.principalRate`            | integer, at least 0                          | `0`            | `ANT_FEDERATION_ROUTE_PRINCIPAL_RATE`        | 0 keeps the default 50 reads/s.                                                                       |
| `federation.routing.minTransferRate`          | integer, at least 0                          | `0`            | `ANT_FEDERATION_ROUTE_MIN_TRANSFER_RATE`     | 0 keeps the default 16384 B/s.                                                                        |
| `federation.routing.transferGrace`            | string                                       | `""`           | `ANT_FEDERATION_ROUTE_TRANSFER_GRACE`        | Empty keeps the default `30s`.                                                                        |
| `federation.routing.transferTimeout`          | string                                       | `""`           | `ANT_FEDERATION_ROUTE_TRANSFER_TIMEOUT`      | Empty keeps the default `2h`.                                                                         |
| `federation.service.type`                     | enum `LoadBalancer`, `NodePort`, `ClusterIP` | `LoadBalancer` |                                              | Type of the federation Service.                                                                       |
| `federation.service.port`                     | integer 1 to 65535                           | `8444`         |                                              | Service port.                                                                                         |
| `federation.service.nodePort`                 | integer 30000 to 32767 or null               | `null`         |                                              | Fixed node port for `NodePort`.                                                                       |
| `federation.service.annotations`              | map                                          | `{}`           |                                              | Service annotations (for example load-balancer settings).                                             |
| `federation.service.loadBalancerSourceRanges` | list of strings                              | `[]`           |                                              | Source CIDR allowlist for `LoadBalancer`. Restrict to the other members.                              |
| `federation.tlsRoute.enabled`                 | boolean                                      | `false`        |                                              | Render a Gateway API `TLSRoute` (TLS passthrough listener).                                           |
| `federation.tlsRoute.hostname`                | string                                       | `""`           |                                              | SNI hostname; required when the TLSRoute is enabled.                                                  |
| `federation.tlsRoute.parentRefs`              | list of objects                              | `[]`           |                                              | Gateway passthrough listener; required when enabled.                                                  |
| `federation.ingressPassthrough.enabled`       | boolean                                      | `false`        |                                              | Render an ingress-nginx Ingress with `ssl-passthrough` (controller needs `--enable-ssl-passthrough`). |
| `federation.ingressPassthrough.className`     | string                                       | `nginx`        |                                              | Ingress class.                                                                                        |
| `federation.ingressPassthrough.host`          | string                                       | `""`           |                                              | Host; required when enabled.                                                                          |
| `federation.ingressPassthrough.annotations`   | map                                          | `{}`           |                                              | Extra annotations.                                                                                    |
| `federation.join.existingSecret`              | string                                       | `""`           | `ANT_FEDERATION_JOIN_DIR`                    | Secret with keys `authority_url`, `ca_sha256` and `invitation`; joins automatically at first start.   |
| `federation.networkPolicyFrom`                | list of NetworkPolicy peers                  | `[]`           |                                              | Peers allowed to reach the federation port when `networkPolicy.enabled` is true.                      |

Federation TLS must reach the pod unterminated. See [Federation](../replication/federation.md).

### NetworkPolicy

| Key                         | Type                               | Default | Env | Description                                                                                                                                  |
| --------------------------- | ---------------------------------- | ------- | --- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `networkPolicy.enabled`     | boolean                            | `false` |     | Create a NetworkPolicy with both `Ingress` and `Egress` policy types.                                                                        |
| `networkPolicy.ingressFrom` | list of NetworkPolicy peers        | `[]`    |     | Peers allowed to reach port 8080 (ingress controller, gateway, metrics scraper). Empty denies all ingress.                                   |
| `networkPolicy.egress`      | list of NetworkPolicy egress rules | `[]`    |     | Rendered verbatim. Must allow DNS, PostgreSQL, object storage, identity providers and every remote-source upstream. Empty denies all egress. |

NetworkPolicy restricts peers, not HTTP paths, and needs a CNI that enforces it.
