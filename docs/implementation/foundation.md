# C01 service foundation evidence

Implemented on branch `feat/service-foundation`:

- C01-T1: validated configuration and required 32-byte root-key file; PostgreSQL-only startup; live/readiness endpoints; bounded requests and HTTP deadlines; graceful drain with forced cancellation at deadline; local development scripts and CI.
- C01-T2: embedded, checksummed, serialized transactional migrations; instance/principal/feed/source/component/version/blob/asset/tag/provenance schema; audit/outbox foundations; uniqueness and cross-instance foreign-key constraints.

Local validation: Go 1.27.1 on macOS arm64 and temporary PostgreSQL 17.11. `scripts/test-postgres.sh` passed with the race detector, including real service startup/restart, query cancellation and recovery, concurrent migrations, failed-DDL rollback, checksum/future-schema rejection, and domain integrity. `go test -race -count=10 ./internal/server` passed after correcting response consumption in the test client. `go vet ./...` and build passed. GitHub CI runs PostgreSQL 17 and 18; remote results are recorded on the PR and are not implied by local evidence.

Only local temporary PostgreSQL fixtures were run. No OKE, MinIO, AWS or other provider environment was provisioned. PostgreSQL 18 Compose configuration is supplied for development; its deployment is not claimed by the local 17.11 test run.

The HTTP server currently exposes health endpoints only. API authorization, artifact transfer, format support, external identity, replication, UI and release qualification remain subsequent tasks. The root key is validated now and will be consumed by the later encrypted-secret store. Application-level single-writer fencing precedes artifact mutations; the migration lock alone is not an instance ownership lease.
