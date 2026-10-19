# Kuvryn Ant

Artifact repository and caching service. The [delivery plan](docs/planning/README.md) covers all 26 planned ecosystems. Implementation includes authenticated management, atomic local artifact publication, OCI/Go/Helm/Cargo/Raw adapters, browser identity integration, and a React console. Work continues on the full Phase1 backlog; these increments are not a general release. [Current evidence and limits](docs/implementation/local-delivery.md).

## Run locally

Install Go 1.27.2 or newer in the 1.27 series, Docker with Compose, and OpenSSL. From this checkout:

```sh
scripts/init-dev.sh
docker compose up -d --wait postgres
scripts/run-dev.sh
```

The service listens on `127.0.0.1:8080`. `GET /health/live` checks the process; `GET /health/ready` checks PostgreSQL with a two-second timeout. `GET /api/v1/status` exposes API status. The management API configures feeds, sources, publish policies and identities. The console is at `/console/`. `/metrics` exposes operational Prometheus metrics; restrict access at the ingress/network boundary. Send SIGINT or SIGTERM to drain requests and stop. `docker compose stop postgres` stops the local database while retaining its volume.

Development secrets are generated once in `.local/`, excluded from Git, and never printed by the scripts. The database port is loopback-only at 15432. The local connection disables TLS; deployed PostgreSQL connections should use authenticated TLS with their configured CA.

To use an existing PostgreSQL 17 or 18 installation, set `ANT_DATABASE_URL` and `ANT_ROOT_KEY_FILE`, then run `go run ./cmd/ant`. The root-key file must contain exactly 32 raw bytes. Keep it outside the database and blob storage; back it up securely. The service uses this key to authenticate pagination cursors. The encrypted-secret library supports an externally supplied versioned keyring; operational keyring configuration and rotation commands remain to be integrated.

| Setting                | Default / requirement                                      |
| ---------------------- | ---------------------------------------------------------- |
| `ANT_DATABASE_URL`     | Required PostgreSQL connection string; never logged        |
| `ANT_ROOT_KEY_FILE`    | Required path to 32 raw key bytes                          |
| `ANT_LISTEN`           | `127.0.0.1:8080`                                           |
| `ANT_MAX_REQUESTS`     | 32 per CPU (128 to 1024) active requests; overflow returns 503 with Retry-After |
| `ANT_DB_MAX_CONNS`     | 8 per CPU (16 to 64) PostgreSQL connections; keep below the server `max_connections` |
| `ANT_UPLOAD_SLOTS`     | 2 per CPU (4 to 32) concurrent uploads per package format |
| `ANT_SHUTDOWN_TIMEOUT` | 15s; configurable 1s–1m                                    |

## Bootstrap and feed API

After starting PostgreSQL, run `scripts/run-dev.sh bootstrap local` once. It writes a JSON credential to stdout containing a 24-hour administrative service token; store that output privately. `scripts/run-dev.sh bootstrap-token` issues a replacement credential using privileged local database access if delivery failed or the token expired. It does not revoke previous tokens. These commands do not start the HTTP server.

Send the token as `Authorization: Bearer <token>` to the feed endpoints. Creation requires `Idempotency-Key`; updates also require `If-Match` containing the quoted current revision. The [OpenAPI contract](api/openapi.yaml) and generated Go client define request and response shapes. Token permissions intersect persisted grants; explicit deny wins. A configured feed format is metadata, not a claim that its package protocol is implemented.

## Schema lifecycle

Startup verifies PostgreSQL compatibility, applies embedded numbered migrations, then opens the HTTP listener. A transaction-scoped advisory lock serializes concurrent migration attempts. All pending DDL and history entries commit together. A failed upgrade rolls back; modified migration checksums, gaps, and schema versions newer than the binary are rejected. Restore a consistent backup for unsupported downgrades; there is no automatic down-migration.

The first migration models independent instances and principals, feeds and ordered sources, components and versions, blobs and deduplication domains, assets with provenance, tags, audit and outbox records. Composite foreign keys prevent cross-instance references; uniqueness constraints protect coordinates and asset identities. Subsequent migrations add idempotency results, service-token verifiers, persisted roles, bootstrap state, and encrypted secrets. Feed mutations enforce authorization, optimistic revision checks, and transactional audit/outbox writes.

## Verify

```sh
go test -race ./...
scripts/check-generated.sh
go vet ./...
go build ./cmd/ant
```

Without a test DSN, real PostgreSQL tests explicitly skip. To run them against an isolated temporary database using installed PostgreSQL binaries (`initdb`, `pg_ctl`, `createdb`):

```sh
scripts/test-postgres.sh
```

The script creates a temporary socket-only cluster and removes it on exit. Alternatively set `ANT_TEST_DATABASE_URL` to a disposable database and `ANT_REQUIRE_INTEGRATION=1` to make a missing fixture fail. Integration tests create schemas and tables; never point them at production. CI runs formatting, generated-contract checks, vet, race tests, and real PostgreSQL17/18 jobs. `scripts/ocismoke` additionally runs an isolated real-crane smoke test when PostgreSQL binaries and crane are installed.

Tests cover lifecycle and migrations, concurrent mutation replay and revision conflicts, authorization, token lifecycle, generated-client API contracts, encrypted-secret rotation/rollback, blob integrity, deterministic routing, and metrics privacy. S3 tests use an injected SDK fixture; they do not certify a live vendor deployment. See [implementation evidence](docs/implementation/foundation.md).

Control endpoints have bounded total timeouts. Artifact adapters use progress-based streaming deadlines and bounded copy buffers; large-object performance qualification remains outstanding.

## Artifact storage and endpoints

Bootstrap before serving artifacts. The process acquires a database writer lease for the bootstrap instance; `ANT_INSTANCE_ID` can select an explicitly provisioned instance. Two serving processes cannot write the same instance concurrently.

Set `ANT_DATA_DIR` to persistent private storage (default `.local/data`). With `ANT_STORAGE_BACKEND=disk`, verified blobs and upload staging are stored there. With `s3`, upload staging remains local and persistent; configure `ANT_S3_ENDPOINT`, `ANT_S3_REGION`, `ANT_S3_BUCKET`, optional `ANT_S3_PREFIX`, `ANT_S3_PATH_STYLE=true` for MinIO, and `ANT_S3_CREDENTIALS_FILE`. The credentials file is private JSON with `access_key_id`, `secret_access_key` and optional `session_token`; it is reloaded for new requests. `ANT_S3_CA_FILE` adds a private CA. HTTPS verification remains enabled.

Set `ANT_PUBLIC_URL` to the externally reachable HTTPS origin, especially behind an OKE ingress. Only a loopback listener can infer a development HTTP origin. Download URLs never trust forwarded headers or caller-controlled Host values.

Create a format-matching local source, attach it to a feed, then configure its explicit default publish destination. OCI endpoints use `/v2/{feed}/{repository}/...`; Go, classic Helm, Cargo and Raw use `/feeds/{slug}/{go|helm|cargo|raw}/...`. See adapter READMEs for ingestion contracts. Publication is currently immutable by default; unsupported lifecycle operations fail explicitly.

## Browser identity

Optional mounted configuration files enable OIDC (`ANT_OIDC_CONFIG_FILE`), SAML (`ANT_SAML_CONFIG_FILE`) and LDAP (`ANT_LDAP_CONFIG_FILE`). They must agree on the configured HTTPS console origin. Client/bind secrets and signing keys are referenced by private file paths. `/auth/providers` lists configured choices; `/auth/session` and CSRF-protected `/auth/logout` manage the local browser session. Administrators explicitly bind trusted provider subjects to existing user principals and roles; login never implicitly creates an administrator.

Configuration fields and tested boundaries are recorded in [OIDC](docs/implementation/oidclogin-agent-result.md), [SAML](docs/implementation/saml-agent-result.md) and [directory](docs/implementation/directory-agent-result.md) integration reports. Provider-wide logout and live vendor qualification remain release work.

## Deployment direction

OKE plus existing MinIO on TrueNAS SCALE is the primary integration target. Disk, AWS S3, and other compatible providers remain in scope through a reusable storage contract. No provider-specific test fleet is required. See the [compatibility baseline](docs/planning/compatibility-matrix.md).

Licensing remains undecided.

Root-key rotation uses `ANT_ROOT_KEY_VERSION`, `ANT_PREVIOUS_ROOT_KEYS_FILE`, and the bounded local `ant rotate-secrets` command. See [rotation and delivery evidence](docs/implementation/local-delivery.md).
