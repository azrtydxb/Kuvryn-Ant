# Kuvryn Ant

Artifact repository and caching service. The [delivery plan](docs/planning/README.md) covers all 26 planned ecosystems. Implementation currently provides a PostgreSQL-backed service foundation, schema migrations, and health endpoints. Package publishing, authentication, UI, and format adapters are still under development.

## Run locally

Install Go 1.27.1 or newer in the 1.27 series, Docker with Compose, and OpenSSL. From this checkout:

```sh
scripts/init-dev.sh
docker compose up -d --wait postgres
scripts/run-dev.sh
```

The service listens on `127.0.0.1:8080`. `GET /health/live` checks the process; `GET /health/ready` checks PostgreSQL with a two-second timeout. Other routes are not yet implemented. Send SIGINT or SIGTERM to drain requests and stop. `docker compose stop postgres` stops the local database while retaining its volume.

Development secrets are generated once in `.local/`, excluded from Git, and never printed by the scripts. The database port is loopback-only at 15432. The local connection disables TLS; deployed PostgreSQL connections should use authenticated TLS with their configured CA.

To use an existing PostgreSQL 17 or 18 installation, set `ANT_DATABASE_URL` and `ANT_ROOT_KEY_FILE`, then run `go run ./cmd/ant`. The root-key file must contain exactly 32 raw bytes. Keep it outside the database and blob storage; back it up securely. Its cryptographic use arrives with secret storage implementation; the service already requires it so development does not create an insecure alternate startup path.

| Setting | Default / requirement |
|---|---|
| `ANT_DATABASE_URL` | Required PostgreSQL connection string; never logged |
| `ANT_ROOT_KEY_FILE` | Required path to 32 raw key bytes |
| `ANT_LISTEN` | `127.0.0.1:8080` |
| `ANT_MAX_REQUESTS` | 128 active requests; overflow returns 503 with Retry-After |
| `ANT_SHUTDOWN_TIMEOUT` | 15s; configurable 1s–1m |

## Schema lifecycle

Startup verifies PostgreSQL compatibility, applies embedded numbered migrations, then opens the HTTP listener. A transaction-scoped advisory lock serializes concurrent migration attempts. All pending DDL and history entries commit together. A failed upgrade rolls back; modified migration checksums, gaps, and schema versions newer than the binary are rejected. Restore a consistent backup for unsupported downgrades; there is no automatic down-migration.

The first migration models independent instances and principals, feeds and ordered sources, components and versions, blobs and deduplication domains, assets with provenance, tags, audit and outbox records. Composite foreign keys prevent cross-instance references; uniqueness constraints protect coordinates and asset identities. This schema does not yet expose mutation APIs or enforce the full application authorization policy.

## Verify

```sh
go test -race ./...
go vet ./...
go build ./cmd/ant
```

Without a test DSN, real PostgreSQL tests explicitly skip. To run them against an isolated temporary database using installed PostgreSQL binaries (`initdb`, `pg_ctl`, `createdb`):

```sh
scripts/test-postgres.sh
```

The script creates a temporary socket-only cluster and removes it on exit. Alternatively set `ANT_TEST_DATABASE_URL` to a disposable database and `ANT_REQUIRE_INTEGRATION=1` to make a missing fixture fail. Integration tests create schemas and tables; never point them at production. CI runs formatting, vet, race tests, and real PostgreSQL 17/18 jobs.

Tests cover configuration rejection, readiness failure, bounded concurrency, graceful/forced shutdown, real startup/restart, database cancellation/recovery, concurrent migrations, DDL rollback, downgrade/checksum rejection, and domain constraints.

Current HTTP timeouts are appropriate for health/control endpoints. Artifact streaming will introduce separate progress-based body limits as part of storage/protocol work; these health-server timeouts are not a claimed large-upload implementation.

## Deployment direction

OKE plus existing MinIO on TrueNAS SCALE is the primary integration target. Disk, AWS S3, and other compatible providers remain in scope through a reusable storage contract. No provider-specific test fleet is required. See the [compatibility baseline](docs/planning/compatibility-matrix.md).

Licensing remains undecided.
