# Container and OKE deployment baseline

The chart deploys one application writer, an internal Service, optional TLS Ingress,
and persistent local storage. It uses existing PostgreSQL and existing Secrets.
It does not provision OKE, MinIO, AWS resources, a database, an ingress controller,
or an identity-provider fleet. AWS S3 and other compatible endpoints remain targets;
the same S3 settings serve MinIO on TrueNAS SCALE.

Planning targets Kubernetes 1.36.4 on OKE, PostgreSQL 18.6 (17.11 compatibility),
Linux amd64/arm64, and Helm 4.3.1. Chart APIs require Kubernetes >=1.30. These are
compatibility requirements, not a claim of deployment certification. Inventory
actual OKE/node, PostgreSQL, TrueNAS and MinIO versions before the deployment test.
See [compatibility](../docs/planning/compatibility-matrix.md) and
[release workloads](../docs/planning/release-workloads.md). Default requests are an
initial scheduling allocation, not measured W1/W2 capacity; limits allow W1's
4 CPU/8 GiB application budget. Size PostgreSQL separately.

## Image

```sh
docker build -t registry.example/kuvryn-ant:VERSION .
# A registry/image builder that supports both target platforms:
docker buildx build --platform linux/amd64,linux/arm64 -t registry.example/kuvryn-ant:VERSION .
```

The build runs `npm ci` against the web lockfile, checks the generated API contract,
builds React production assets, and embeds them in the Go binary. The final image
contains the binary, shell entrypoint and CA certificates, runs as UID/GID 10001,
and receives SIGTERM directly through `exec`. `NODE_IMAGE`, `GO_IMAGE`, and
`RUNTIME_IMAGE` build arguments allow digest-pinned base images. Pin the resulting
application digest in `image.digest` before a production rollout. The default Go
build toolchain is the planning baseline 1.27.2. No credentials or `.local` files
are sent in the build context.

## Local Compose

```sh
scripts/init-dev.sh
docker compose --profile app up --build -d
# This prints the initial administrator credential once to your terminal.
docker compose --profile app exec app ant-entrypoint bootstrap
# Serving topology is loaded at startup; restart after the first bootstrap.
docker compose --profile app restart app
```

Open `http://localhost:8080/console/` and use the initial credential. Native package
URLs use the loopback public URL. Browser identity-provider cookies require HTTPS;
configure a TLS reverse proxy and `ANT_PUBLIC_URL` for browser SSO testing.

The existing PostgreSQL image, password source, port and `postgres_data` volume are
unchanged. The application is opt-in with profile `app`, uses separate `app_data`
and `app_secrets` volumes, and waits for healthy PostgreSQL. A one-shot local
`app-secrets` container copies the generated files into a private UID10001-owned
volume: Compose file-backed secrets do not reliably remap host file ownership.
Only this preparation container runs as root; the application runs non-root with
a read-only root filesystem. Recreate `app-secrets` after changing development
secret files. Do not use `docker compose down -v` on data you intend to keep.

## Existing production prerequisites

1. PostgreSQL reachable over verified TLS. Put the full libpq DSN (including
   `sslmode=verify-full`, and `sslrootcert` if needed) into an existing Secret key
   `dsn`; the chart default Secret name is `ant-postgres`. Optional PostgreSQL TLS
   files mount from `postgres.tlsExistingSecret` at `/run/ant/postgres`.
2. A persistent 32-byte raw application root key in existing Secret `ant-root-key`,
   key `root.key`. Generate once using a secure random source, preserve across
   reinstalls, and back it up separately from data. The chart never generates it.
3. A block CSI storage class, or an existing PVC. Use ext4/XFS semantics; NFS, SMB
   and object-FUSE do not satisfy the local disk contract. PVC-mounted data remains
   necessary with S3 for persistent OCI upload staging and other local working data.
4. A trusted HTTPS public origin, matching ingress host and certificate Secret.
   Configure the ingress controller's body-size, streaming and timeout limits for
   the artifact sizes you intend to serve. Chart ingress routes `/console`, `/api`,
   `/auth`, `/v2`, `/feeds`; `/metrics` and `/health` are not public ingress paths.

Secret material should be supplied by your existing secret manager/controller or
`kubectl create secret ... --from-file=...` using protected files. Do not put DSNs,
passwords, private keys or raw root-key bytes into Helm values or Git.

```sh
helm lint deploy/chart
helm template ant deploy/chart -f deploy/examples/oke-minio.yaml
# After replacing example inventory and supplying existing Secrets/PVC:
helm upgrade --install ant deploy/chart -f YOUR_PROTECTED_VALUES_FILE --wait
kubectl exec deployment/ant-ant -- /usr/local/bin/ant bootstrap
kubectl rollout restart deployment/ant-ant
```

Bootstrap is a deliberate operator action; its token output is sensitive. Do not
turn it into a Helm hook or log it to deployment automation. For an existing
instance, preserve PostgreSQL, the root key, blobs and staging PVC; skip bootstrap.
Upgrade migrations are forward-only. Snapshot/back up a consistent PostgreSQL/blob
reference set before upgrades; an image rollback does not undo migrations.

Operations guides: [upgrade and rollback](../docs/operations/upgrade.md),
[observability (metrics, tracing, logs, diagnostics, audit retention, metadata
repair)](../docs/operations/observability.md) and
[backup and restore](../docs/operations/backup-restore.md). Tracing is enabled
with `observability.tracing.*` chart values; audit retention with
`auditRetention.*`.

## MinIO / other S3 endpoints

Use `storage.backend: s3`, the HTTPS object API endpoint, configured region, existing
bucket and a deployment-specific prefix. `pathStyle: true` is suitable for many
MinIO installations; AWS and providers with virtual-host addressing may use false.
An empty endpoint uses standard AWS endpoint resolution; no AWS account is required.
The current runtime uses mounted credentials (not an ambient workload-identity chain).
The credentials Secret key `credentials.json` contains an object with
`access_key_id`, `secret_access_key`, and optional `session_token`. Mount a private
CA using `caExistingSecret`/`caKey`; verification remains enabled. This is the object
API, not the MinIO administration console. The example creates no bucket and grants
no bucket policies. Provision least-privilege object/multipart permissions separately.

The entire root-key Secret mounts at `/run/ant/root`. For versioned rotation set
`rootKey.activeVersion` and optionally `rootKey.previousKeysManifestKey` (for
example `previous.json`). That protected JSON object maps previous versions to
key-file names, e.g. `{"v1":"root-v1.key"}`; include each corresponding 32-byte
file in the same Secret. The chart sets `ANT_ROOT_KEY_VERSION` and
`ANT_PREVIOUS_ROOT_KEYS_FILE`. Keep previous keys until all encrypted values and
retained backups requiring them have been migrated under the application rotation
workflow. Never remove old keys merely because a new image was deployed.

Credentials are re-read by the runtime; root-key and provider configuration changes
require a controlled restart. All Secret mounts are directory projections (no
`subPath`) so Kubernetes file rotation is visible. PostgreSQL DSN environment
changes also require a restart.

## Browser providers

For any enabled provider, set `auth.<provider>.existingSecret`. The entire Secret is
mounted at `/run/ant/<provider>` and its `configKey` (default `config.json`) becomes
`ANT_OIDC_CONFIG_FILE`, `ANT_SAML_CONFIG_FILE`, or `ANT_LDAP_CONFIG_FILE`. Include
referenced private keys, bind passwords, certificates and metadata in that same
Secret, and use absolute mounted paths in JSON. All enabled providers must use the
same trusted HTTPS origin as each other and the public URL. Configure callback URLs
under that origin: `/auth/oidc/callback` and `/auth/saml/callback`.

OIDC JSON: `origin`, `key`, `issuer`, `client_id`, `client_secret_file` (optional for
public PKCE clients), `redirect_uri`, `allowed_endpoints` (exact authorization/token/
JWKS URLs). SAML JSON: `origin`, `entity_id`, `idp_entity_id`, `metadata_file`,
`key_file`, `certificate_file`. LDAP JSON: `origin`, `provider_id` (stable HTTPS
identity namespace), `url`, `bind_dn`, `bind_password_file`, optional `ca_file`,
`user_base_dn`, `group_base_dn`, `login_attribute`, `user_id_attribute`,
`group_id_attribute`, `user_class`, `group_class`, `member_attribute`.

Existing explicit issuer/subject bindings are required. Login never silently
creates users or administrator grants. Directory subjects use immutable
entryUUID/objectGUID, not login names. Shared logout revokes the local session;
SAML provider SLO is not currently exposed by the browser adapter.

## Operational boundaries

- `replicaCount` must be 1 and updates use `Recreate`; the database writer lease is
  an additional fencing layer. Do not scale this Deployment or run a second release
  against the same logical instance. Recreate introduces upgrade downtime.
- Grace period must exceed the configured drain timeout by at least 10 seconds.
  Readiness checks PostgreSQL, liveness checks the process; neither certifies S3,
  IdP connectivity or a completed bootstrap. Startup allows five minutes for schema
  initialization. Large migration rehearsals may require deliberate adjustment.
- The non-root entrypoint creates `/var/lib/ant/data` with mode0700. `fsGroup:10001`
  supplies PVC mount access; existing claims must permit this ownership policy.
  Only `/tmp` and the data PVC are writable. No privileged init container is used
  in Kubernetes. All capabilities are dropped and service-account tokens disabled.
- Optional headless metrics Service exposes the same application port at `/metrics`.
  With NetworkPolicy enabled, explicitly allow ingress controllers/metrics peers
  and egress DNS, PostgreSQL, configured storage and identity endpoints. Empty rule
  lists deny traffic; the chart cannot infer your OKE-to-TrueNAS network topology.
  NetworkPolicy restricts network peers, not HTTP paths; apply controller policy
  if a trusted ingress peer must be further restricted.
- PVCs created by the chart have a Helm keep annotation. They are deliberately not
  deleted on uninstall. Reinstallation may need `persistence.existingClaim`; reclaim
  policy and backups remain operator responsibilities.

`deploy/check-chart.sh` performs lint/render and rejection checks locally. Actual
OKE installation, MinIO interoperability, recovery and W1–W6 workload evidence
remain deployment/release tasks, not claims made by rendering the chart.

For the local image permission smoke, use
`deploy/check-image.sh IMAGE linux/arm64` (or `linux/amd64`). It starts only a
short-lived application container with an anonymous data volume, exercises the
real entrypoint and binary, and checks UID10001, mode0700, CA availability and a
read-only root filesystem. It does not start a database or a provider environment;
Docker removes the container and anonymous volume afterward. Cross-architecture
execution under Docker Desktop is emulation, not native hardware certification.
