# Storage

Blobs (package files, image layers, archives) are content-addressed by SHA-256 and stored either on a local filesystem (`disk`) or in an S3-compatible bucket (`s3`). Metadata always lives in [PostgreSQL](postgresql.md). Blobs are never deleted inline; garbage collection removes unreferenced blobs in a separate mark and sweep pass.

## Choosing a backend

|                                | Disk                                   | S3                                                   |
| ------------------------------ | -------------------------------------- | ---------------------------------------------------- |
| Where blobs live               | `ANT_DATA_DIR` on the data volume      | Bucket and prefix                                    |
| Data volume still required     | Yes, holds everything                  | Yes, for staging and resume state                    |
| Capacity for pressure eviction | Filesystem size, or a configured quota | Must be configured (`maintenance.pressure.capacity`) |
| Typical use                    | Single cluster, fast block storage     | Large or growing caches, cheaper capacity            |

You can move between them later without changing client URLs; see [Storage migration](../operations/storage-migration.md).

## Disk backend

`storage.backend: disk` (the default) stores verified blobs and upload staging under `ANT_DATA_DIR`, which the chart sets to `/var/lib/ant/data` on the PVC mounted at `/var/lib/ant`.

Requirements for the volume:

- **Block storage** (a CSI block volume, local NVMe, cloud disks) formatted **ext4 or XFS**. Publication relies on `fsync` and atomic renames on a local filesystem.
- **Not supported:** NFS, SMB/CIFS, and FUSE mounts of object storage. They do not provide the local-disk semantics the backend depends on.
- Access mode `ReadWriteOnce` or `ReadWriteOncePod` (only one pod mounts it).
- Writable by GID 10001: the chart sets `fsGroup: 10001` with `fsGroupChangePolicy: OnRootMismatch`. An existing claim must allow that ownership policy.

```yaml
storage:
  backend: disk
persistence:
  storageClass: fast-block # your block CSI class; empty uses the cluster default
  size: 500Gi
```

Size the volume for the blobs you expect plus staging headroom; see [Sizing](sizing.md). Do not write backups to this volume: a full data volume stops the service. The metric `ant_data_dir_bytes_free` supports a disk-space alert.

## S3 backend

`storage.backend: s3` stores blobs in an existing bucket. Ant signs requests with SigV4 using static credentials from a mounted file; it does not create buckets or set bucket policies.

| Helm value                             | Env                          | Notes                                                                                                       |
| -------------------------------------- | ---------------------------- | ----------------------------------------------------------------------------------------------------------- |
| `storage.s3.endpoint`                  | `ANT_S3_ENDPOINT`            | Empty: AWS endpoint resolution for the region. Otherwise the object API URL (not a vendor's admin console). |
| `storage.s3.region`                    | `ANT_S3_REGION`              | Required.                                                                                                   |
| `storage.s3.bucket`                    | `ANT_S3_BUCKET`              | Required, must exist.                                                                                       |
| `storage.s3.prefix`                    | `ANT_S3_PREFIX`              | Default `ant/`. Use a deployment-specific prefix if the bucket is shared.                                   |
| `storage.s3.pathStyle`                 | `ANT_S3_PATH_STYLE`          | `true` for MinIO and OCI Object Storage; `false` for AWS virtual-host addressing.                           |
| `storage.s3.credentialsExistingSecret` | `ANT_S3_CREDENTIALS_FILE`    | Required.                                                                                                   |
| `storage.s3.caExistingSecret`          | `ANT_S3_CA_FILE`             | Optional private CA.                                                                                        |
| `storage.s3.allowInsecureHTTP`         | `ANT_S3_ALLOW_INSECURE_HTTP` | Opt-in for `http://` endpoints.                                                                             |

Configuration is validated at startup without network calls. Requests use a 15-second response-header timeout and up to three attempts. The SDK computes and validates checksums only where the S3 API requires them, which avoids incompatibilities with S3 implementations that do not support the newer default checksum headers.

The credentials need object read, write, list and delete permissions on the prefix, plus multipart upload operations (create, upload part, complete, abort, list multipart uploads). Garbage collection aborts multipart uploads that have been idle for more than 24 hours.

### Credentials file

The Secret key (default `credentials.json`) holds one JSON object:

```json
{
  "access_key_id": "<ACCESS_KEY_ID>",
  "secret_access_key": "<SECRET_ACCESS_KEY>",
  "session_token": "<optional>"
}
```

`access_key_id` and `secret_access_key` are required, `session_token` is optional, and any other field is rejected. The file must be at most 16 KiB. It is re-read for new requests, so rotating the Secret takes effect without a restart once the kubelet refreshes the mount.

```sh
umask 077
cat > credentials.json <<'JSON'
{"access_key_id":"<ACCESS_KEY_ID>","secret_access_key":"<SECRET_ACCESS_KEY>"}
JSON
kubectl -n ant create secret generic ant-s3 --from-file=credentials.json=./credentials.json
rm -f credentials.json
```

::: warning Gap
Only mounted static credentials are supported. Ambient credential chains (EKS IRSA or Pod Identity, instance metadata, OCI instance principals) are not used by the runtime.
:::

### AWS S3

```yaml
storage:
  backend: s3
  s3:
    endpoint: "" # AWS endpoint resolution
    region: eu-west-1
    bucket: myorg-ant-blobs
    prefix: ant/
    pathStyle: false
    credentialsExistingSecret: ant-s3
```

Create an IAM user (or role with long-lived keys issued for it) limited to the bucket and prefix, and put its access key in the credentials file.

### MinIO

MinIO normally needs path-style addressing. Point the endpoint at the S3 API port, not the MinIO console.

```yaml
storage:
  backend: s3
  s3:
    endpoint: https://minio.example.com:9000
    region: us-east-1 # the region configured in MinIO (default us-east-1)
    bucket: ant
    prefix: ant/
    pathStyle: true
    credentialsExistingSecret: ant-minio
    caExistingSecret: ant-minio-ca # if MinIO uses a private CA
```

### OCI Object Storage

Oracle Cloud Infrastructure Object Storage exposes an Amazon S3 compatibility API at:

```text
https://<namespace>.compat.objectstorage.<region>.oraclecloud.com
```

`<namespace>` is your tenancy's Object Storage namespace and `<region>` the region identifier. Credentials are a **Customer Secret Key** (created under a user's profile in the OCI console): its access key and secret key go into `credentials.json`. The bucket must be in the compartment designated for the S3 compatibility API.

```yaml
storage:
  backend: s3
  s3:
    endpoint: https://mytenancy.compat.objectstorage.eu-frankfurt-1.oraclecloud.com
    region: eu-frankfurt-1
    bucket: ant-blobs
    prefix: ant/
    pathStyle: true
    credentialsExistingSecret: ant-oci-s3
```

Replace `mytenancy` with your namespace and the region with yours.

### Private CA

If the endpoint's certificate is issued by a private CA, store the CA certificate in a Secret and reference it. It is added to the system trust store for S3 requests only; verification stays on.

```sh
kubectl -n ant create secret generic ant-s3-ca --from-file=ca.crt=./corp-ca.pem
```

```yaml
storage:
  s3:
    caExistingSecret: ant-s3-ca
    caKey: ca.crt
```

The file must be PEM and at most 1 MiB.

### Plain HTTP endpoints

By default the chart and the runtime refuse an `http://` endpoint. On an isolated, trusted network you can opt in:

```yaml
storage:
  s3:
    endpoint: http://10.0.0.30:9000
    allowInsecureHTTP: true
```

Credentials and blob contents then cross the network unencrypted. Prefer HTTPS with a private CA.

## Why the data volume is still needed with S3

With the S3 backend the PVC still holds:

- upload staging (`stagingDir`, `/var/lib/ant/tmp`): uploads are written and verified locally before they are committed;
- persistent OCI upload sessions and S3 multipart resume state (`/var/lib/ant/data/resume-state`);
- other local working data.

It can be much smaller than with the disk backend, but it must be persistent block storage, sized for the largest concurrent uploads you expect. See [Sizing](sizing.md).

## Storage migration

Blobs can be moved between disk and S3 while the service keeps serving. Both backends must be configured; the copy is verified by size and SHA-256, checkpointed, resumable, and the active backend switches in one transaction only after a clean catch-up pass. The old store is kept until you run `ant storage cleanup`.

```sh
kubectl -n ant exec deploy/ant-ant -- ant storage migrate --to s3 --dry-run
kubectl -n ant exec deploy/ant-ant -- ant storage migrate --to s3
kubectl -n ant exec deploy/ant-ant -- ant storage status
```

The full procedure is in [Storage migration](../operations/storage-migration.md).

::: warning Gap
With the Helm chart, the `ANT_S3_*` variables and the credentials mount are rendered only when `storage.backend` is `s3`, and the chart has no generic extra-volume value. A disk-to-S3 migration of a chart-managed release therefore has no documented, tested values path yet. Note also that `ANT_STORAGE_BACKEND` selects the active backend only until the first migration; after that the backend persisted in PostgreSQL wins.
:::
