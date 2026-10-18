# Shared format contract rules

Status: Engineering contracts selected for implementation on 2026-10-18. No adapter is implemented or certified by this planning work. These rules plus each Fxx record replace open-ended workflow candidates; implementation tasks supply evidence before any support claim.

## Source, routing and publication contract

All 26 formats have local delivery, remote pull-through and replica delivery in scope. Local publication is either a native ecosystem publish operation or the explicit admin-ingestion workflow in its record; unsupported native commands are not advertised. An ingestion request creates a staged publish session through the management API, uploads declared assets, validates their closure, then commits metadata/indexes atomically. Requests are authenticated/scoped and idempotent. The React console and MCP use these same operations.

Writes resolve to one local source before data acceptance and never implicitly forward upstream. Immutable versions default to conflict on different bytes. Deletion/overwrite/retention is policy-controlled and produces audit/outbox events. Local signing keys are separate from upstream trust keys. No generic union of signed repository metadata is permitted.

`package-owner`, `path-owner`, `object-owner` and `snapshot-owner` select an ownership unit. Explicit namespace bindings outrank source order; a selected owner stays fixed across metadata and byte retrieval. Fallback may choose an owner on initial not-found, never silently replace a missing version from another owner. Snapshot sources are coherent generations. More permissive composition must be added as an explicit adapter capability with conformance evidence.

## Cache and authentication contract

Default metadata TTL is 5 minutes, negative-cache TTL 30 seconds; digest-addressed immutable blobs live until retention. An adapter tightens freshness for mutable indexes. Cache keys include source/normalized identity/representation/generation plus authorization domain where needed. Unauthorized results are not global misses. Stale-if-error/offline is opt-in, respects signature validity and is visible through explain/metrics. Coalesced streaming, cancellation and range support use the shared core where protocol semantics allow.

TLS is required for deployed clients. Native registry token/API-key/basic schemes map to shared scoped identities; browser sessions never replace package-client authentication. Anonymous read is opt-in. Signing, integrity, client auth and uploader provenance are separate controls. Archive metadata parsing must be bounded and must not execute uploaded code or follow unsafe paths.

## Lifecycle and replication contract

Each adapter maps component identity/version, asset roles/digests, tags, raw metadata, search fields and retained references into the shared domain. Search is scoped and paginated. A full feed seed includes cached artifacts, indexes, tags and metadata unless an explicit selective policy excludes them. Preflight reports unresolved external references; it cannot claim a complete offline replica until required dependency/source archives are materialized. Replicas preserve exact immutable byte identities and authoritative ownership. Tombstones never remove destination-only assets.

Each adapter implements metadata-aware cleanup and health checks plus publish/resolve/cache/validation metrics. Read explain and publish routing diagnostics must be available in API, UI and MCP. Distinct forms of deletion (unlist/yank/tag removal/blob dereference) remain format-specific. All of them are implemented once in `artifact.Service.Delete/Unlist/Relist` (policy-gated, audited, tombstoned, bytes left to garbage collection); each F-file records the native method and any unsupported case.

## Conformance evidence required during implementation

1. Execute the pinned baseline clients and positive/negative cases in the individual record; store exact binary/image digests in the harness lockfile at fixture installation. A reference version is a test target, not evidence already obtained.
2. Exercise local publish/ingest then consume, same-endpoint native read/write where defined, remote cold/warm/cache freshness, source ownership, token denial, signature/hash failures, malformed archives, cancellation/restart and concurrent immutable conflicts.
3. Run shared backend scenarios first on disk and the owner's MinIO. Use the same suite for AWS/other compatible endpoints only when needed; do not construct a provider fleet or a full client×backend×provider Cartesian matrix.
4. Verify seed plus incremental metadata/tag/index changes, restart and source outage; every record requires offline closure or an explicit visibly incomplete result.
5. Retain versioned fixtures/results/limitations and document exact client setup. Platform-specific clients use macOS/Windows runners only for their format cases. No support claim before evidence.

Protocol-specific requirements derive from linked official references. Scope, ownership policy and excluded non-goals are Kuvryn Ant engineering choices. Exclusions cannot remove an explicit spec requirement; all 26 ecosystems remain Phase 1 release blockers.
