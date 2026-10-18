# Kuvryn Ant delivery plan

Status: Engineering planning complete, revision 1, 2026-10-18. Implementation readiness follows the recorded coverage/dependency and GitHub publication audit; no additional owner approval gate.
Source of truth for product scope: [product specification](../../kuvryn-ant-product-spec.md), version 0.1.

## Confirmed direction

- Go backend, React console, versioned OpenAPI management API, and MCP access through the same authorization boundary.
- PostgreSQL is required for every installation, including development. No embedded metadata database.
- Local disk and S3-compatible blob storage are both Phase 1 requirements.
- All 26 ecosystem entries in the specification remain Phase 1 scope. Delivery waves do not redefine Phase 1 as a reduced-format MVP.
- Feeds expose stable, format-specific endpoints. Read resolution and publish routing are separate policies. Publishing resolves to exactly one local source before accepting content; writes never implicitly go upstream.
- Phase 1 instances are independently administered. Explicit seed/incremental artifact replication is required; federation belongs to Phase 2.
- License/distribution selection is deferred at the owner's direction. Do not add a license or make an open-source licensing claim.
- Complete planning, questions, milestones, user stories, and GitHub tasks/issues before product implementation begins.

## Planning gate

Development may begin only when all of the following are true:

1. Blocking product questions have recorded answers and delegated engineering decisions have explicit rationale.
2. Every spec section and acceptance criterion maps to a milestone and one or more stories.
3. Every ecosystem has a capability record with named workflows, client/version test policy, source support, composition rules, unsupported behaviors, and evidence required before a support claim.
4. Milestones have explicit entry/exit conditions and dependencies. Calendar commitments require team capacity and workload estimates; none are invented.
5. Each implementation story has a user outcome, testable acceptance criteria, dependencies, task breakdown, and a definition of done.
6. Milestones, stories, and actionable tasks are published to GitHub and cross-linked. The local index contains their URLs.
7. The backlog has been checked for missing requirements, duplicated work, dependency cycles, and prematurely claimed support.
8. Record the engineering readiness audit and verify published deliverables. The owner has authorized completing this work autonomously; no additional confirmation is required.

Planning research and documentation are allowed before this gate. Prototypes that implement product behavior are not.

## Decisions and planning assumptions

| ID | Decision | Proposed direction / consequence |
|---|---|---|
| Q01 | Ecosystem delivery order | Confirmed: OCI/Docker, Go modules, Helm, Rust/Cargo, Raw, then remaining formats. Shared OCI artifact work accompanies the OCI platform. Core storage tests use protocol-neutral fixtures, not early Raw format delivery. |
| Q02 | Enterprise identity release boundary | Confirmed: OIDC, SAML, and LDAP are all required in the first general release. SCIM is later, as allowed by the spec. |
| Q03 | Scale and capacity | Workload-based engineering targets W1–W6; no calendar or staffing promises. See release-workloads.md. |
| Q04 | Deployment | OKE plus existing MinIO on TrueNAS SCALE is primary validation. Compose/Helm, disk, AWS and other S3-compatible providers remain in product scope. Reuse one suite; do not build a vendor environment fleet. |
| Q05 | Adapters | Built-in Go modules; versioned internal capability contract. See architecture A01. |
| Q06 | Write routing | Priority rules with bounded namespace/package/path/principal selectors; reject ambiguous equal-priority targets; one local destination. A04. |
| Q07 | Resolution | Explicit package/path/object/snapshot ownership, source priority and separate error/not-found policies. No unsafe signed index unions. A04 and per-format contracts. |
| Q08 | Secrets | Envelope encryption with externally mounted versioned root keys and rotation. No required cloud KMS. A07. |
| Q09 | API | `/api/v1`, bounded cursor pagination, structured errors, conditional mutation and 24-hour idempotency. Current/preceding product minor upgrade policy. A05. |
| Q10 | Release evidence | Numerical workload targets and reproducible measurement procedure selected; passing results remain implementation work. |

Q01–Q04 reflect owner responses and specification requirements. Q05–Q10 are documented engineering decisions under the owner's instruction to proceed with planning. Exact installed versions, credentials and endpoints are deployment inventory, collected when needed; they do not block core development. No product support or measured performance is claimed by planning completion.

## Delivery strategy

Build capabilities in dependency order and verify each through all relevant interfaces. A usable development increment is not broad availability. Do not start all format adapters against an unstable artifact model.

1. Complete planning, architecture decisions, capability ledger, API conventions, threat model, and conformance design.
2. Build the shared metadata, identity/authorization, audit, blob, and feed-policy foundations with operational diagnostics.
3. Prove the shared artifact lifecycle using protocol-neutral contract fixtures on both storage backends; deliver ecosystem adapters in the owner-selected order.
4. Add remote caching, multi-source resolution, and client conformance. Establish realistic concurrency and large-object baselines.
5. Deliver ecosystem adapters in reviewable waves; keep unfinished capabilities explicitly disabled or marked experimental.
6. Add the React console and MCP incrementally against supported APIs; each capability must retain UI/API/MCP permission parity.
7. Deliver independent-instance replication on stable metadata/manifests, then imports, lifecycle operations, and deployment hardening.
8. Run full release evidence, security, disaster-recovery, compatibility, performance, and usability gates.

Some work can overlap after its prerequisites pass. The GitHub dependency graph must distinguish prerequisites from merely related work. No staffing or parallel-agent assumptions are embedded in this plan.

## Milestone boundaries

These boundaries organize the GitHub backlog. Milestone numbers identify workstreams, not a strictly serial schedule; story dependencies determine execution readiness. No due dates are assigned.

### M00 — Planning and readiness

Outcome: an agreed, traceable, executable backlog.

Scope: resolve decisions; document architecture boundaries and data model; establish format ledger; define client matrix and release criteria; create GitHub milestones, stories, task issues, labels, dependencies, and planning index.

Entry: product spec exists. Exit: planning gate satisfied. Blocks all implementation milestones.

### M01 — Shared platform and secure administration

Outcome: a runnable Go service with PostgreSQL metadata and authenticated, auditable management operations.

Scope: project conventions and CI; migrations and transaction boundaries; feed/source/asset/blob/identity models; API schema and generated client pipeline; service accounts and scoped tokens; authorization checks; audit transactions; health/readiness/logging/metrics; local development and shutdown behavior.

Exit: real PostgreSQL integration tests prove migration, rollback constraints, authorization denials, scoped token expiry/revocation, mutation idempotency, and atomic metadata/audit writes. Secrets do not appear in logs or API responses. Fresh installation and restart work.

Dependencies: M00. External identity provider work may begin once identity boundaries are stable, but browser UI authentication is not considered complete with a bootstrap token.

### M02 — Artifact integrity, storage, and local publishing

Outcome: correct local artifact lifecycle through explicit feed routing.

Scope: streaming disk/S3 blob interfaces; digest and size checks; multipart and cancellation; deduplication domain; transactional publication; protocol-neutral core fixtures; immutable/overwrite/delete policies; source precedence; explain resolution; reference-safe garbage-collection design; browsing/search/checksum lookup; provenance.

Exit: shared storage/lifecycle contract fixtures pass on both disk and selected S3 backend(s); real-client evidence is required in M04/M05; interrupted or corrupt uploads are not visible; concurrent publication cannot silently overwrite immutable releases; metadata references survive restart and cleanup; same-endpoint read/write routing is proven.

Dependencies: M01. S3 implementation is required for milestone completion, not a post-release optional feature.

### M03 — Remote caching and build acceleration

Outcome: runners reuse dependencies through predictable, observable pull-through feeds.

Scope: upstream credentials/TLS/network policy; per-resource cache keys; independent metadata/blob freshness; conditional revalidation; coalesced misses; bounded retry/circuit breaking; negative caching; stale-if-error/offline; safe fallback and source policies; prewarming and schedules; rate limits/quotas; cache diagnostics and savings metrics.

Exit: concurrent cold requests cause one upstream fetch per key under the agreed normal-operation model; outages and cancellation do not poison cache state; stale responses are diagnosable; credentials cannot cross origins; warm/cold and large-object workloads produce recorded evidence.

Dependencies: M02 and format-specific composition contracts. Cache savings calculations must distinguish measured bytes/time from estimates.

### M04 — First ecosystem delivery wave

Outcome: supported client workflows for the owner-selected first format wave.

Scope: per-format protocol, publish and delete semantics, metadata/indexing, source composition, auth, cache behavior, search fields, cleanup, client configuration, fixtures, conformance, and limitations. Confirmed wave order: Docker/OCI, Go modules, Helm, Rust/Cargo, then Raw. OCI artifact behavior shares the OCI platform but keeps its own conformance record. Remaining ecosystems follow this wave.

Exit: every enabled capability in each wave member has versioned real-client evidence on relevant sources/storage combinations; unsupported capabilities are visible. OCI artifact coverage remains distinct from container-image coverage even if the adapter is shared.

Dependencies: M01–M03; specific work can proceed as the shared APIs stabilize. Completion is a development milestone, not Phase 1 completion.

### M05 — Remaining ecosystem breadth

Outcome: all remaining formats have accepted, evidence-backed capability records.

Scope: Alpine APK, Ansible Galaxy, APT/Debian, CocoaPods, Composer, Conan, Conda, Git LFS, Hugging Face, Maven, npm, NuGet/Chocolatey, Eclipse p2, Pub/Flutter, PyPI, R/CRAN, RubyGems, Swift Package Manager, Terraform, Yum/RPM.

Exit: all 26 spec entries satisfy their approved workflow matrices; signing and metadata-sensitive formats pass correctness and tamper/failure cases; no generic file storage is counted as format support.

Dependencies: M01–M03 plus reusable work from M04. Sub-waves should group shared protocol/index/signing concerns and business priority; no arbitrary equal estimates per ecosystem.

### M06 — Enterprise identity and complete operator interfaces

Outcome: operators and automation can securely use the product through React, API, and MCP.

Scope: OIDC with Zitadel and a second provider; PKCE and session security; SAML and LDAP required for GA; group/role mappings; emergency admin; secret rotation; identity diagnostics and recovery; React overview/feed/package/identity/audit/cache/storage/replication screens; MCP discovery/lookups/health/admin tools; webhooks/event delivery.

Exit: provider and package-client authentication coexist; permission-denial and audit parity tests cover UI/API/MCP; destructive MCP actions carry the appropriate client confirmation semantics; accessibility and error/recovery flows pass; no UI-only administrative capabilities lack supported APIs.

Dependencies: M01 onward, with screens/tools delivered alongside their domain capabilities. Final exit also depends on M07–M08 for replication and lifecycle interfaces. SCIM remains explicitly deferred unless scope changes.

### M07 — Independent-instance replication

Outcome: another site can seed a feed, keep it synchronized, and serve locally during source outage.

Scope: peer identity/TLS; manifests/checkpoints; full seed including cached artifacts by default; optional exclusion of transient cache entries; namespace/package/version filters; scheduled/manual/continuous increments; tags/indexes/metadata/checksums; restart/resume/dedup; conflict authority; deletion dry runs/tombstones; reviewed non-secret configuration templates; lag/progress/failure reporting.

Exit: two real instances across disk and S3 pass interrupted seed, incremental changes, corruption rejection, retries, deletion protection for destination-only content, source outage, and destination-specific configuration preservation. Replicas remain read-only by default.

Dependencies: M02–M03 and stabilized format records. Representative tests do not replace replication compatibility coverage for every claimed format.

### M08 — Adoption and operational lifecycle

Outcome: users can deploy, migrate, operate, recover, and upgrade installations.

Scope: container/Compose and Helm; TLS/reverse proxy trust; import/export inventories and dry runs; selected repository-manager migration mappings; prewarm from lockfiles/package lists/images; storage migration; retention/eviction/GC; metadata repair/integrity checks; backup/restore; schema upgrades and rollback limitations; diagnostics; tracing; capacity planning and quotas.

Exit: documented fresh install, representative import, backup/restore, disk↔S3 migration, repair, and upgrade exercises pass with retained evidence. Images run non-root and expose only required services. Operators can detect and recover from full disks, DB failures, slow stores, and upstream outages.

Dependencies: M01–M03; final coverage requires M05 and M07. Required source migration products and versions need an explicit acceptance matrix.

### M09 — Phase 1 release qualification

Outcome: broad availability supported by reproducible evidence.

Scope: all 10 spec acceptance criteria; complete format matrix; identity/provider matrix; backend compatibility; two-site replication; threat model/security verification; performance/resource limits; API compatibility; user documentation; usability/accessibility; upgrade/recovery rehearsal and release support policy.

Exit: all release blockers closed; no unsupported claims; numeric SLOs achieved on documented hardware/workloads; release checklist records client and server versions, test results, known limitations, artifact provenance, and outstanding non-blocking issues.

Dependencies: M01–M08. Licensing/distribution remains an explicit unresolved commercial/legal choice; do not silently pick one to publish artifacts.

### M10 — Federation discovery (Phase 2, not a Phase 1 dependency)

Outcome: separately approved design and backlog for federation.

Scope: shared identity/policy, discovery, cross-site search, placement/routing, coordinated replication, and conflict semantics. Define requirements and design only after independently operable instances are proven.

Exit: a new approved scope, threat model, consistency model, migration strategy, and acceptance criteria. No Phase 1 task depends on federation services.

## Architecture scope (resolved in architecture-baseline.md)

- Artifact domain: separate feeds, sources, namespaces, components/versions, assets, immutable blobs, tags, format metadata, and provenance. Specify IDs, uniqueness, ownership, and lifecycle invariants.
- PostgreSQL: transactional boundaries, schema migration discipline, indices, job leasing, audit/event outbox, replication checkpoints, and retention. Document expected query patterns and size limits before optimization.
- Blob abstraction: context cancellation, bounded buffers, integrity verification, finalize/abort semantics, resumable transfers, and explicit backend capabilities.
- Publication: authorize and select a unique local destination, validate upload, durably store bytes, commit metadata plus audit/event records, then expose the package. Orphans are collectible; partial packages are never valid.
- Resolution: validated acyclic configuration, deterministic source precedence, separate not-found/error policies, format-defined metadata composition, cache provenance, and explain output.
- API: versioned management contract; cursor pagination and bounded filters; stable errors; conditional updates; idempotency scope/TTL/conflict behavior; generated clients and schema conformance.
- Authorization: independent read/publish/delete/configure/replicate/admin permissions; namespace/feed content selectors; explicit anonymous read; source administration; consistent machine and human identity treatment.
- Jobs/events: durable retries, cancellation, progress, scheduling, idempotency, bounded concurrency, and transactional event delivery. No requirement for an external broker until justified.
- Extension contract: format-owned routes and parsing; shared core for storage/auth/audit; explicit capability negotiation; built-in modules selected in A01.
- Security: trust boundaries for upstream URLs, redirects, credentials, signed metadata, archives, secret encryption, package clients, and replication peers. Include SSRF and decompression/resource exhaustion tests.

## Backlog shape and issue quality

GitHub milestones group release outcomes. An epic issue groups a domain; user-story issues express testable user outcomes; independently assignable implementation tasks become linked child issues. Small verification steps can remain checkboxes within a task. Use native sub-issues/dependencies where available and explicit links as the portable fallback. Do not create empty placeholder issues for the sake of counts.

Each story body must contain:

1. Stable planning ID and source spec sections.
2. “As a [persona], I want [capability], so that [outcome].”
3. Scope and explicit exclusions, with experimental/unsupported status where relevant.
4. Concrete acceptance scenarios including denied access, malformed input, cancellation, concurrency, and restart where applicable.
5. Dependencies and blocked-by relationships.
6. Links to actionable tasks and related format/architecture records.
7. Required test evidence, docs, observability, API/UI/MCP work, and migration implications.

Each task must describe a bounded deliverable, its parent story, inputs/prerequisites, acceptance checks, and completion evidence. Avoid task boundaries such as “implement all storage” or “support npm” with no workflow decomposition.

Backlog labels: `type:epic`, `type:story`, `type:task`, `type:decision`, `type:conformance`; `phase:0`, `phase:1`, `phase:2`; `status:planning`, `status:blocked`, `status:ready`; domain labels for core, storage, formats, identity, API, UI, MCP, cache, replication, operations, adoption, and quality. Milestones carry release order; priorities carry business urgency. Do not assign people or dates without agreement.

## Definition of ready

A task is ready when its parent outcome is accepted, dependencies have passed their gates, open decisions that affect implementation are resolved, acceptance scenarios are executable, test dependencies/fixtures exist or are explicitly part of the task, and it has no unacknowledged scope assumptions. Global planning readiness is additionally required before any first task starts.

## Definition of done

- Behavior satisfies the story's positive and negative acceptance criteria.
- Relevant unit, real PostgreSQL integration, backend contract, protocol-client, and end-to-end tests pass; concurrency/large-object tests are included where they test actual risks.
- API schema, capability ledger, operator/client documentation, and UI/MCP surfaces reflect the implemented behavior.
- Authorization and audit are enforced; no credential leakage; operational metrics and actionable failures exist.
- Migrations, interruption/restart behavior, cleanup, and compatibility implications are reviewed.
- Evidence links record tested versions and backend/client combinations. Code merged is not by itself evidence of format conformance.

## Release evidence matrix

For each format, select representative and boundary clients with pinned test versions; document supported version ranges separately. Cover publish/consume, update/delete, source selection, cache freshness, upstream failure, authentication, checksums/signatures, metadata composition, range/resume where meaningful, and replication. Unsupported/inapplicable cells require a reason.

Cross-cutting suites include:

- Storage: identical digest, lifecycle, cancellation, multipart, corruption, and GC-reference scenarios for disk and primary MinIO, with the same reusable suite for additional S3 providers.
- Concurrency: cold fan-out, simultaneous immutable publishes, refresh races, deletion versus active reads, job leases, and interrupted replication.
- Identity: Zitadel plus another OIDC provider; agreed SAML/LDAP profiles; scoped tokens, revoked/expired credentials, group mapping, anonymous denial, and emergency recovery.
- Interfaces: each administrative capability through API, then UI and MCP parity; OpenAPI validation and generated-client compatibility.
- Reliability: DB unavailable, full disk, slow/unavailable object store, upstream outage, restart mid-upload, restart mid-job, and restore on a clean installation.
- Performance: warm/cold pulls, large blobs/models/layers, metadata-heavy clients, high fan-out, bounded-memory transfers, and accurate cache savings.

## Spec traceability

| Specification | Planned coverage |
|---|---|
| §1–4 vision, goals, boundaries, personas | M00 scope and all story personas; Phase 2 separation |
| §5 ecosystem breadth and adapter contract | M00 capability ledger, M04–M05, M09 conformance |
| §6.1 local publishing | M02, each format story |
| §6.2 remote caching | M03, each format story |
| §6.3 feed resolution/routing | M01 policy contracts, M02–M03 behavior, format-specific validation |
| §6.4 metadata/artifacts | M01 data model, M02 lifecycle/search, M08 maintenance |
| §6.5 build acceleration | M03 caching/prewarm/metrics/quotas, M08 runner/adoption guidance |
| §6.6 storage | M01 PostgreSQL, M02 disk/S3, M08 migration/repair |
| §6.7 replication | M07, M09 two-instance evidence |
| §6.8 federation | M10 only |
| §6.9 identity | M01 machine identity, M06 providers/recovery, Q02/Q08 |
| §6.10 governance | M01 authorization/audit, M06 integrations/events, M08 retention/export |
| §6.11 API/OpenAPI/MCP | M01 API, M06 MCP/parity, domain milestones and M09 validation |
| §6.12 UI | M06 and incremental domain screens |
| §6.13 operations | M01 diagnostics, M08 deployments/lifecycle, M09 rehearsals |
| §7 architecture | M00 decisions, M01–M03 contracts, M07 replication |
| §8 performance/reliability | M03 baseline, M09 numeric release gates |
| §9 import/export/adoption | M08 |
| §10 delivery phases | M00 = Phase 0; M01–M09 = Phase 1; M10 = Phase 2 |
| §11 acceptance 1–2 | M04–M05 format workflows and M09 evidence |
| §11 acceptance 3–4 | M01/M06 identity, permission, and audit parity |
| §11 acceptance 5–6 | M07 replication and M02 backend contracts |
| §11 acceptance 7–9 | M08 adoption/recovery, M03/M09 performance |
| §11 acceptance 10 | M00 ledger and M09 evidence gate |
| §12 open decisions/risks | Decision register below and capability records |

## Confirmed decision log

- D001: PostgreSQL only for all installations. Owner response, 2026-10-18.
- D002: No license selection at present. Owner response, 2026-10-18.
- D003: Complete planning and GitHub backlog before implementation. Owner instruction, 2026-10-18.
- D004: Initial ecosystem order is OCI/Docker, Go, Helm, Cargo, Raw, then the remaining formats. Owner response, 2026-10-18.
- D005: OIDC, SAML, and LDAP are all required for first general release. Owner response, 2026-10-18.
- D006: Continue planning with workload-based gates; no scale, deadline, or team capacity was supplied. Planning assumption following owner response, 2026-10-18.

- D007: Primary validation is OKE plus MinIO on TrueNAS SCALE; AWS and other providers remain supported targets. Reuse compatibility tests without creating test environments for every provider. Owner clarification, 2026-10-18.
- D008: Finish engineering planning autonomously and ask only material unresolved questions; no extra owner sign-off gate. Owner instruction, 2026-10-18.

## Planning completion checklist

- [x] Read the complete product spec.
- [x] Confirm repository access and inspect existing issues/milestones (none at planning start).
- [x] Remove the initial dependency scaffold; no application code remains.
- [x] Record PostgreSQL-only and deferred-licensing decisions.
- [x] Draft full-scope delivery strategy and spec traceability.
- [x] Record owner answers on ecosystem order, identity scope, and continued workload-based planning.
- [x] Resolve architecture and compatibility decisions (P01/P02).
- [x] Finalize all 26 format contracts and source/client workflows.
- [x] Finalize milestone boundaries and dependencies.
- [x] Write user stories and bounded task issues with acceptance criteria.
- [x] Publish and cross-link GitHub backlog; verify publication against local index.
- [x] Audit spec coverage and dependencies under delegated planning authority.
- [x] Define readiness handoff; implementation itself remains separate work.

## Published backlog

Verified: 11 milestones, 56 user stories, 164 task issues, all 26 formats, and an acyclic dependency graph. These counts exclude the two original planning overview issues. Architecture, protocol and compatibility decisions are complete. The readiness record and remote verification identify completed planning work and the first ready implementation task.

- [M00 — Planning and readiness](https://github.com/azrtydxb/Kuvryn-Ant/milestone/1)
- [M01 — Shared platform and secure administration](https://github.com/azrtydxb/Kuvryn-Ant/milestone/2)
- [M02 — Artifact integrity, storage, and local publishing](https://github.com/azrtydxb/Kuvryn-Ant/milestone/3)
- [M03 — Remote caching and build acceleration](https://github.com/azrtydxb/Kuvryn-Ant/milestone/4)
- [M04 — First ecosystem delivery wave](https://github.com/azrtydxb/Kuvryn-Ant/milestone/5)
- [M05 — Remaining ecosystem breadth](https://github.com/azrtydxb/Kuvryn-Ant/milestone/6)
- [M06 — Enterprise identity and complete operator interfaces](https://github.com/azrtydxb/Kuvryn-Ant/milestone/7)
- [M07 — Independent-instance replication](https://github.com/azrtydxb/Kuvryn-Ant/milestone/8)
- [M08 — Adoption and operational lifecycle](https://github.com/azrtydxb/Kuvryn-Ant/milestone/9)
- [M09 — Phase 1 release qualification](https://github.com/azrtydxb/Kuvryn-Ant/milestone/10)
- [M10 — Federation discovery (Phase 2, not a Phase 1 dependency)](https://github.com/azrtydxb/Kuvryn-Ant/milestone/11)

Architecture review: [P01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/4). Workload review: [P02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/7). Compatibility matrix: [P02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/8). Planning readiness: [P01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/5).
