# Service and storage foundation evidence

Branch: `feat/service-foundation`; draft PR: https://github.com/azrtydxb/Kuvryn-Ant/pull/223.

Implemented increments:

- C01: validated configuration, PostgreSQL-only lifecycle, bounded admission and graceful shutdown, checksummed transactional migrations and domain constraints.
- C02: OpenAPI contract/generated Go client; structured errors and request IDs; authenticated feed CRUD subset (create/read/list/update), signed keyset cursors, optimistic revisions, scoped idempotent mutation replay and atomic audit/outbox events.
- C03: SHA-256 service-token verifiers, expiration/revocation, restrictive token scopes, persisted roles with explicit-deny precedence, bounded bearer failure throttling, privileged local bootstrap/recovery commands. External identity and full account/token management APIs remain outstanding.
- C05: bounded-label HTTP metrics and structured completion logs; no request secrets, raw paths or resource identifiers in labels. Healthy probes are silent. Metrics shares admission limits; liveness bypasses overload. Storage/transfer/tracing metrics remain outstanding.
- S01/S02: streaming disk and S3 SDK storage libraries with digest/length verification, conditional publication, dedup verification, bounded memory and range reads. No artifact endpoint, durable resume, orphan collector or live-provider qualification yet.
- S03: immutable local-write routing policy compiler; deny precedence, overlap rejection, safe path normalization and deterministic explanations. Protocol extraction and persistence remain outstanding.
- I03: authenticated envelope encryption with external versioned root keys; persisted instance/record/purpose-bound secrets, optimistic updates and transactional audit/outbox; resumable bounded key-rewrap batches. Operational keyring/rotation configuration and emergency login remain outstanding.

The combined `scripts/test-postgres.sh` suite passed across all 11 packages, including real database tests for token lifecycle, feed API contract conformance, concurrent mutation replay, secret CAS/rotation/rollback and bootstrap recovery. `scripts/check-generated.sh` and `go vet ./...` passed.

Validation uses Go 1.27.1 on macOS arm64 and a temporary socket-only PostgreSQL 17.11 cluster. The combined test script runs the race detector and requires PostgreSQL integration fixtures. GitHub CI runs PostgreSQL 17 and 18; the PR records results for each commit. SDK fixtures exercise S3 behavior without provisioning a provider environment. Live OKE/MinIO compatibility and load measurements remain release gates, not claimed results.

No OKE, MinIO, AWS or other provider environment was provisioned. PostgreSQL 18 Compose configuration is supplied for development; local PostgreSQL 17.11 tests do not imply it was deployed.

Application-level single-writer fencing must precede artifact publication; migration locks are not an ownership lease. Artifact transfer, all 26 format adapters, external identity, replication, UI and release qualification remain planned work. The feed API and supporting libraries are an implementation increment, not a general release.
