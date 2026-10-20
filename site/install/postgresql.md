# PostgreSQL

Kuvryn Ant stores all metadata in PostgreSQL: feeds, sources, packages, assets, identities, audit events, replication state and encrypted secrets. There is no embedded database. Blobs live in the [storage backend](storage.md).

## Supported versions

| Version                    | Status                                                                          |
| -------------------------- | ------------------------------------------------------------------------------- |
| PostgreSQL 18              | Primary target (planning baseline 18.6; the Compose file uses `postgres:18.6`). |
| PostgreSQL 17              | Supported (planning baseline 17.11).                                            |
| 16 and older, 19 and newer | Refused at startup with `PostgreSQL major version must be 17 or 18`.            |

The server checks `server_version_num` on every start. CI runs the integration suite against PostgreSQL 17 and 18. No extensions are required; the schema uses only core features.

## Database and role

Create one database and one role that owns it. The role needs to create tables and indexes in its database, because migrations run as that role at startup.

```sql
CREATE ROLE ant LOGIN PASSWORD '<password>';
CREATE DATABASE ant OWNER ant;
```

Use one database per Kuvryn Ant instance. Do not share the database with other applications.

## Connection string

`ANT_DATABASE_URL` accepts a libpq-style URL or key/value string, parsed by the pgx driver. In Kubernetes the chart reads it from a Secret (`postgres.existingSecret`, key `postgres.dsnKey`).

```text
postgres://ant:<password>@db.example.com:5432/ant?sslmode=verify-full&sslrootcert=/run/ant/postgres/ca.crt
```

| Parameter           | Recommendation                                                                                                                                                                                                                                     |
| ------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `sslmode`           | `verify-full`: encrypted, certificate chain and host name verified. Use `disable` only for local development.                                                                                                                                      |
| `sslrootcert`       | Path to the server's CA certificate. With the chart, put the CA in a Secret referenced by `postgres.tlsExistingSecret`; it is mounted at `/run/ant/postgres`. Omit it if the server certificate chains to a public CA in the image's system store. |
| `sslcert`, `sslkey` | Client certificate authentication, if your server requires it. Mount the files through the same TLS Secret.                                                                                                                                        |
| password            | URL-encode special characters, or leave the password out of the DSN and provide `PGPASSWORD` instead.                                                                                                                                              |

The connection string is never logged; `ant diagnostics` reports host, port, database, user and whether TLS is on, with the password redacted. Changing the DSN requires a pod restart.

## CloudNativePG example

[CloudNativePG](https://cloudnative-pg.io/) (CNPG) is a convenient way to run PostgreSQL in the same cluster. The following `Cluster` creates database `ant` owned by role `ant`, with TLS enabled by default.

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: ant-db
  namespace: ant
spec:
  instances: 2 # one primary, one standby
  # Use a PostgreSQL 17 or 18 image. Pin an exact version or digest in production.
  imageName: ghcr.io/cloudnative-pg/postgresql:18
  postgresql:
    parameters:
      max_connections: "200"
      shared_buffers: "1GB"
  bootstrap:
    initdb:
      database: ant
      owner: ant
  storage:
    size: 50Gi
    storageClass: "" # block storage class; empty uses the cluster default
  resources:
    requests:
      cpu: "2"
      memory: 4Gi
    limits:
      memory: 4Gi
```

CNPG generates:

- Secret `ant-db-app` with the application credentials (keys include `username`, `password`, `host`, `port`, `dbname`, `uri`).
- Secret `ant-db-ca` with the cluster CA (`ca.crt`, and the CA private key `ca.key`).
- Services `ant-db-rw` (primary), `ant-db-ro` and `ant-db-r`. Kuvryn Ant must connect to `ant-db-rw`: it writes on every request path that mutates state.

### Build the Ant Secrets from the CNPG secrets

Copy only the CA certificate into its own Secret. Do not mount `ant-db-ca` directly, because it also contains the CA private key.

```sh
umask 077
NS=ant
PW=$(kubectl -n $NS get secret ant-db-app -o jsonpath='{.data.password}' | base64 -d)
kubectl -n $NS get secret ant-db-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > ca.crt

# The server certificate covers ant-db-rw, ant-db-rw.<ns> and ant-db-rw.<ns>.svc.
printf 'postgres://ant:%s@ant-db-rw.%s.svc:5432/ant?sslmode=verify-full&sslrootcert=/run/ant/postgres/ca.crt' \
  "$(printf '%s' "$PW" | jq -sRr @uri)" "$NS" > dsn

kubectl -n $NS create secret generic ant-postgres --from-file=dsn=./dsn
kubectl -n $NS create secret generic ant-postgres-tls --from-file=ca.crt=./ca.crt
rm -f dsn ca.crt
unset PW
```

Helm values:

```yaml
postgres:
  existingSecret: ant-postgres
  dsnKey: dsn
  tlsExistingSecret: ant-postgres-tls
```

If CNPG runs in another namespace, replace the namespace in the host name and create the two Secrets in the Ant release namespace. When CNPG rotates the application password, rebuild `ant-postgres` and restart the pod.

::: details Optional: backups to object storage with CNPG
CNPG can archive WAL and take base backups to S3-compatible object storage. Recent CNPG releases move this to the Barman Cloud plugin; the in-tree `barmanObjectStore` field below still exists but is deprecated there. Follow the documentation of your operator version.

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: ant-db
  namespace: ant
spec:
  # ... as above ...
  backup:
    retentionPolicy: "30d"
    barmanObjectStore:
      destinationPath: s3://ant-pg-backups/ant-db
      endpointURL: https://s3.example.com # omit for AWS S3
      s3Credentials:
        accessKeyId:
          name: ant-pg-backup-s3
          key: ACCESS_KEY_ID
        secretAccessKey:
          name: ant-pg-backup-s3
          key: SECRET_ACCESS_KEY
      wal:
        compression: gzip
---
apiVersion: postgresql.cnpg.io/v1
kind: ScheduledBackup
metadata:
  name: ant-db-daily
  namespace: ant
spec:
  schedule: "0 0 2 * * *" # six fields, seconds first
  cluster:
    name: ant-db
```

A PostgreSQL backup alone is not a complete Kuvryn Ant backup: blobs and the root key are separate. See [Backup and restore](../operations/backup-restore.md) for `ant backup`, which captures metadata together with a consistent blob reference set.
:::

## Connection pool sizing

Each Kuvryn Ant process has one pgx pool.

| Setting                                | Default                        | Notes                                                                                                                                                        |
| -------------------------------------- | ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `ANT_DB_MAX_CONNS` (Helm `dbMaxConns`) | 8 per CPU, clamped to 16 to 64 | Allowed 4 to 500. "CPU" is `GOMAXPROCS`, derived from the container CPU limit. With the chart default limit of 4 CPUs the pool holds at most 32 connections. |
| Minimum idle connections               | 0                              | Connections are opened on demand.                                                                                                                            |
| Connect timeout                        | 5 s                            | Startup fails after 10 s if PostgreSQL is unreachable.                                                                                                       |

Budget `max_connections` on the server as:

```text
max_connections >= dbMaxConns (serving pod)
                 + dbMaxConns per concurrent CLI command (ant backup, ant gc, ant storage, ... run with kubectl exec)
                 + superuser_reserved_connections (default 3)
                 + replication and monitoring connections
                 + any other clients
```

A one-off command started with `kubectl exec` inherits the pod's environment and opens its own pool with the same maximum, so leave room for at least one. The CNPG example above sets `max_connections: "200"`, which is ample for the default pool.

Raise `dbMaxConns` only together with `maxRequests` and the CPU limit, and only when metrics show requests waiting for connections. More connections than PostgreSQL has cores rarely helps.

::: warning Gap
Running Kuvryn Ant behind a connection pooler such as PgBouncer has not been tested. The service uses transaction-scoped advisory locks and table-based writer leases rather than session state, but no pooler mode is documented as supported.
:::

## Migrations

- The schema is embedded in the binary as contiguous, numbered SQL migrations. All pending migrations are applied at startup, before the HTTP listener opens, in one transaction.
- The transaction takes a PostgreSQL advisory lock (`pg_advisory_xact_lock`), so concurrent starters (for example a CLI command and the server) serialize instead of racing.
- If any migration fails, the transaction rolls back, the process exits and the previous schema is intact.
- A history with gaps, a modified checksum, or a version newer than the binary is refused. A binary never runs against a newer schema.
- Migrations are forward-only; there are no down migrations. Rolling back the image after an upgrade requires restoring a pre-upgrade backup.
- The chart's startup probe allows five minutes (60 x 5 s) for migrations. Rehearse large upgrades on a restored copy.

Inspect the schema state without changing anything with `ant diagnostics` (`migrations.json` reports the applied and binary versions). See [Upgrade](../operations/upgrade.md).
