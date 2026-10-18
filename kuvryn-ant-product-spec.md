# Kuvryn Ant — Product Specification

**Status:** Working product specification  
**Version:** 0.1  
**Date:** 2026-10-18  
**Product class:** Self-hosted artifact platform and build-factory accelerator

This document records product intent and requirements; it is not a claim that every feature is already implemented.

## 1. Product vision

Build a modern, lightweight, high-performance artifact platform for organizations that need broad package-format support and want to accelerate builds across one or more build sites. Kuvryn Ant supports the same broad kinds of artifact ecosystems found in established repository managers, while defining its own APIs, user experience, and behavior.

The system will be written primarily in **Go**, provide a **React** web console, and expose a complete, versioned API described by **OpenAPI**, with an **MCP server** for agent-accessible operations. It will store content on local disk or S3-compatible object storage.

The initial product supports independent instances with explicit artifact replication. **Federation is a later phase** and is not required for Phase 1 operation.

## 2. Goals

1. Support a broad range of package and artifact ecosystems, including the formats listed in this specification.
2. Provide a flexible feed model that combines local storage, remote upstreams, and replicated content through explicit resolution and publish-routing policies.
3. Accelerate builds by caching dependency and container-image traffic near runners, reducing repeat downloads and upstream dependency.
4. Run efficiently as a single instance, with a path to multi-instance replication and later federation.
5. Support local disk and S3-compatible object storage through a reliable storage abstraction.
6. Integrate with enterprise identity and authorization systems, including Zitadel and other standards-compatible providers.
7. Make the complete product operable through the UI, API, and MCP, with consistent permissions and auditability.
8. Provide flexible import tooling and format-aware clients without requiring another product's behavior or administration API to be reproduced.

## 3. Non-goals and boundaries

- Phase 1 is **not** a globally consistent, active-active federated control plane. Instances remain independently administered.
- Artifact replication does not imply shared identity, shared policy, global search, or automatic write conflict resolution.
- The product will not claim ecosystem support merely because it can store arbitrary files. Every format adapter must pass its own defined client workflows.
- Build execution and runner orchestration are outside the product. The product accelerates build factories by serving and caching their artifacts.

## 4. Users and primary workflows

### 4.1 Repository administrator

- Configure feed endpoints, local and remote sources, upstream credentials, routing policies, retention, storage, identity providers, access policies, and replication.
- Inspect health, cache effectiveness, storage use, replication state, audit history, and upstream failures.
- Import/export repository configuration and artifact content through documented, format-aware tools.

### 4.2 Build engineer

- Point standard package managers, container clients, and CI workflows at repository endpoints.
- Use one stable feed endpoint with clear read resolution and publish routing.
- Configure runners at different sites to use nearby cache instances.
- Diagnose cache hit/miss behavior and upstream bottlenecks without changing build logic.

### 4.3 Developer / publisher

- Pull dependencies and images using ecosystem-standard clients.
- Publish packages using standard client commands and scoped credentials.
- Browse package versions, provenance, checksums, and repository metadata in the UI or API.

### 4.4 Platform automation / AI agent

- Discover capabilities and operate repositories through OpenAPI or MCP.
- Use narrowly scoped credentials and receive the same authorization and audit controls as human/API users.

## 5. Supported ecosystems and repository capabilities

Kuvryn Ant does not make users choose between three fixed repository types. It models **feeds**, **sources**, and **policies**:

- A **feed** is a stable, format-specific client endpoint.
- A **local source** stores packages published to Kuvryn Ant.
- A **remote source** connects to an upstream registry/repository. Reads are cached automatically according to freshness and retention policy.
- A **replica source** exposes artifacts copied from another Kuvryn Ant instance.
- A feed's **read policy** selects or combines sources and defines precedence, metadata/index composition, fallback, and failure behavior.
- A feed's **publish policy** routes writes to one explicit local source, or rejects writes. A read endpoint never has ambiguous write destinations.
- A feed may be configured for **read and write on the same URL**. The protocol operation determines whether a request reads/resolves content or publishes content; policy determines source selection or the single write destination.

This model provides the useful single-endpoint experience people often seek with repository groups, while making remote caching, local publishing, replica availability, and conflict handling explicit. Policies are format-aware: an adapter defines which sources can be combined safely and how indexes/metadata are resolved. Nested feed graphs are not required.

The following list defines the initial ecosystem breadth Kuvryn Ant intends to support. It defines product scope, not a protocol compatibility promise to any existing repository manager. Feed capabilities and client behavior are designed and documented by Kuvryn Ant.

| Ecosystem / format | Initial scope |
|---|---|
| Alpine APK | Package indexes, signing, publish and consume workflows. |
| Ansible Galaxy | Collections and Galaxy-style package workflows. |
| APT / Debian | Signed repository metadata, distributions, components, and packages. |
| CocoaPods | Specs and pod source/artifact delivery. |
| Composer / PHP | Package metadata, archives, and source/dist references. |
| Conan | C/C++ recipes, package revisions, and binary packages. |
| Conda | Channels, platform subdirectories, and repodata. |
| Docker / OCI | Registry API, manifests, blobs, tags, authentication, multi-architecture images, and OCI artifacts. |
| Git LFS | Batch API and large-object storage/transfer. |
| Go modules | Module paths, versions, proxy protocol, and checksums. |
| Helm | Chart repositories and OCI-hosted charts. |
| Hugging Face | Model and dataset artifact access, large files, and range/resume workflows. |
| Maven | Coordinates, metadata, snapshots, checksums, and common client workflows. |
| npm | Scoped packages, metadata, dist-tags, tarballs, and publishing. |
| NuGet / Chocolatey | v2/v3 feeds, package publish/consume, and symbol package workflows. |
| OCI artifacts | Distribution-compatible non-container artifacts. |
| Eclipse p2 | Metadata and installable units for Eclipse-based tooling. |
| Pub / Flutter | Packages and Pub-compatible client workflows. |
| PyPI | Simple API, wheel/source distributions, and package publishing. |
| R / CRAN | Package indexes, source/binary packages, and repository metadata. |
| Raw | Generic files and directory-style artifact delivery. |
| RubyGems | Gem indexes, dependency metadata, and uploads. |
| Rust / Cargo | Sparse and git index workflows, crate delivery, and publishing. |
| Swift Package Manager | Package registry and source archive workflows. |
| Terraform | Provider and module registry workflows. |
| Yum / RPM | RPM packages and repository metadata. |

Each format has an implementation record describing supported client flows, publish semantics, source types, read-resolution and publish-routing policies, authentication, indexing, retention, and known limitations.

### 5.1 Format adapter contract

Each adapter must provide:

- Protocol routes and request/response behavior required by Kuvryn Ant's documented supported clients.
- Format-aware metadata parsing, generation, or safe pass-through as appropriate.
- Local-source upload/publish and deletion behavior.
- Remote-source cache keying, freshness rules, upstream credential handling, and negative-cache behavior.
- Safe read-resolution and publish-routing rules for each format, including deterministic precedence and metadata/index composition where required.
- Search/index fields, component and asset mapping, cleanup rules, health checks, and operational metrics.
- Conformance fixtures and end-to-end tests using real ecosystem clients.

An adapter can declare a capability unsupported when the format does not define it or when Kuvryn Ant has not implemented it; the limitation must be explicit in product documentation.

## 6. Functional requirements

### 6.1 Local publishing sources

- Accept standard client uploads and publish operations for supported formats.
- Validate format metadata and artifact integrity before committing a publish.
- Support release/snapshot or equivalent policies where the ecosystem has them.
- Support immutable release policy, overwrite policy, delete policy, and retention policy at feed or namespace scope.
- Provide checksums, content type, size, timestamps, uploader identity, and provenance metadata where available.
- Make publication atomic from the client's perspective: incomplete uploads must not appear as valid packages.

### 6.2 Remote sources and caching

- Proxy configurable upstream registries/repositories with optional TLS trust, credentials, headers, and network policy.
- Cache metadata and blobs independently with configurable freshness and revalidation rules.
- Support stale-if-error and offline-serving policies with visible staleness information.
- Coalesce concurrent misses for the same upstream resource to avoid duplicate downloads.
- Stream large assets; support HTTP range requests when upstream and format semantics permit.
- Retry safely with bounded backoff and circuit-breaking; do not amplify upstream outages.
- Cache negative lookups only for configurable, short periods and avoid poisoning package resolution.
- Expose upstream latency, response codes, cache hits/misses, bytes served, and download savings.

### 6.3 Feed resolution and publish routing

- A feed provides the client-facing URL and selects one ecosystem adapter.
- Support a bidirectional feed where clients use the same base URL and credentials for both dependency pulls and package publishing, subject to the ecosystem client's protocol conventions.
- Configure read resolution independently from writes. Reads may search local, remote, and replica sources; writes route to one configured local source or are rejected.
- On a read miss, a feed may pull through from a remote source, stream the response to the client, and cache the artifact and relevant metadata for subsequent reads through that same endpoint.
- Detect publish operations from the ecosystem protocol and route them according to explicit feed rules, such as namespace/coordinate, package scope, path prefix, or client identity. Rules must resolve to one local source before accepting the upload.
- Never pass a publish request through to an upstream source by default. Upstream publishing requires a separate, explicit opt-in and credential policy if added in a future release.
- Define same-coordinate behavior when content exists in multiple sources: configurable local-first/source priority, immutable version policy, and explicit overwrite/conflict result.
- Keep upload and read paths separate internally for authorization, audit, cache invalidation, and transactional safety even when the public endpoint is shared.
- Define policy primitives such as local-first, source priority, fallback-on-not-found, failover-on-upstream-error, and allow/deny constraints. Adapters expose only combinations that are valid for the ecosystem.
- Compose ecosystem indexes and metadata using adapter-defined rules; do not assume that concatenating paths or indexes is correct.
- Enforce feed-level permissions and source-level administrative controls consistently.
- Provide an explain view showing which source satisfied a read, why alternatives were skipped, where a publish will route, and what stale/offline behavior applies.
- Prevent recursive feed resolution and detect policy cycles at configuration time.
- Allow reusable policy templates, while keeping each feed's effective configuration inspectable and auditable.

### 6.4 Artifact and metadata management

- Model repositories, namespaces, components/packages, assets/blobs, tags, and format metadata separately.
- Store immutable byte objects by cryptographic digest; retain format-specific identity and path mapping in metadata.
- Deduplicate identical blobs within a configured deduplication domain.
- Support browse, search, filter, pagination, checksum lookup, dependency metadata where available, and artifact download.
- Provide safe garbage collection that accounts for references, replication, multipart uploads, and delayed object-store consistency.
- Record provenance and lifecycle events without making format-specific metadata inaccessible to generic tooling.

### 6.5 Build-factory acceleration

- Work with GitHub Actions runners and other CI/build agents through standard ecosystem endpoints; no mandatory runner plug-in.
- Support site-local instances or caches so multiple runners reuse downloaded dependencies.
- Provide optional prewarming by repository, package list, lockfile, image digest, or scheduled policy.
- Report cache hit ratio, cold/warm latency, bytes avoided from upstream, and estimated build download time saved.
- Support rate limits and quotas by user, service account, feed, namespace, or site.
- Make cache behavior observable and tunable without embedding build-specific assumptions in package adapters.

### 6.6 Storage backends

- **Local disk:** simple single-node installation, configurable data path, capacity monitoring, and documented filesystem requirements.
- **S3-compatible object storage:** configurable endpoint, region, bucket, prefix, path-style behavior, encryption, and credentials or workload identity where available.
- Stream large uploads/downloads; support multipart operations and resumable internal transfer.
- Store metadata in a transactional database separate from blob storage. PostgreSQL is the proposed production database; embedded mode may be evaluated for development/small installations.
- Verify blob size and digest on write and support background integrity checks.
- Support storage migration between disk and S3 without changing repository URLs.
- Do not require object listing as the sole source of truth for metadata.

### 6.7 Replication — Phase 1

- Provide an explicit **full-feed sync** option that seeds the complete feed content on another instance: all packages/artifacts, versions, tags, format metadata, indexes, and checksums. Include cached pull-through artifacts by default for a true full copy, with an option to exclude transient cache entries.
- After the initial full seed, support continuous incremental sync as well as scheduled or manually triggered sync. Changes include newly published content, updated format metadata/indexes, and configured deletion events.
- Also support selective sync by feed, namespace, package, version, or policy filter for sites that do not need a complete copy.
- Transfer manifests and metadata with referenced blobs; verify integrity by digest at the destination. Make full seeding restartable and incremental checkpoints durable.
- Keep content sync separate from feed configuration sync. The destination keeps its own endpoint, upstream credentials, identity provider settings, and access policy by default. Optionally copy a reviewed, non-secret feed configuration template without overwriting destination-specific settings.
- Make deletion/tombstone propagation an explicit per-sync policy, with a dry-run preview and protection against a replica deleting destination-only content.
- Make jobs resumable and idempotent; expose progress, lag, failures, and last successful sync.
- Define conflict behavior explicitly. Initial policy: each feed has a configured authoritative writer and replicas are read-only by default; no implicit multi-writer merge.
- Permit replication across supported storage backends and network boundaries with TLS and per-peer credentials.
- Allow a fully seeded destination to serve local clients independently if the source becomes unavailable.

### 6.8 Federation — Phase 2

Federation is a distinct future capability, built above independently operable instances. Candidate scope includes global identity, shared policy, instance discovery, cross-site search, placement/routing, and coordinated replication. Phase 1 must avoid assumptions that make these impossible, but must not require federation services for normal operation.

### 6.9 Identity, authentication, and authorization

Identity integration is a core requirement, not an optional add-on. The system must support:

- **OIDC / OAuth 2.0:** authorization-code flow with PKCE for the React UI; validated issuer, audience, signature, nonce, state, expiry, and redirect URI; discovery/JWKS rotation; logout behavior; support Zitadel, Keycloak, Authentik, Microsoft Entra ID, Okta, and generic standards-compliant providers.
- **SAML 2.0:** enterprise browser SSO with metadata import/export, signed assertions, configurable NameID and attribute mapping, certificate rollover, and single logout where supported.
- **LDAP / Active Directory:** optional direct integration or directory-backed authentication, TLS/StartTLS, group lookup, nested-group policy, and connection health status.
- **Local emergency administrator:** disabled or tightly restricted by policy when SSO is mandatory; credentials must be securely hashed, rate-limited, audited, and recoverable through documented break-glass procedures.
- **API and machine identity:** service accounts, scoped API tokens, revocation/expiry, optional client credentials or workload identity, and credentials suitable for CI without interactive login.
- **Registry client authentication:** standard-compatible authentication for Docker/OCI and format-specific token/API-key/basic flows required by other package clients. Browser SSO must not break non-interactive package clients.
- **Authorization:** role-based permissions with repository, namespace, operation, and administrative scope; map IdP groups/claims to internal roles; define conflict and fallback behavior; support read, publish, delete, configure, replicate, and administer permissions independently.
- **Provisioning:** optional SCIM 2.0 user/group provisioning in a later release or as a deployable integration; the internal identity model must support it.
- **Audit:** record successful and failed authentication, token lifecycle, group/role mapping changes, administrative changes, and artifact publish/delete events.

The UI must make provider setup diagnosable: test connectivity, show issuer/metadata status, show claim/group mapping previews, detect clock/certificate errors, and provide safe recovery if SSO configuration is broken. Secrets must be encrypted at rest using a configurable key-management strategy and redacted from APIs/logs.

### 6.10 Access control and governance

- Local users/groups and external identities must resolve into one authorization model.
- Provide repository-level permissions and content selectors for formats where attributes are available; fall back to path/repository scope for generic content.
- Support anonymous read only when explicitly enabled.
- Enforce least privilege for UI, API, MCP, service accounts, and replication peers.
- Provide audit export and retention settings.
- Integrate with external vulnerability scanners, signing systems, and provenance attestations through APIs/events; keep integrations optional and avoid coupling core serving to scanner availability.

### 6.11 API, OpenAPI, and MCP

- All administrative and operational capabilities available in the UI must be available through a versioned HTTP API, except where a browser-only security flow is inherently required.
- Publish and validate an OpenAPI document as part of each release; use it to generate clients and API reference documentation.
- Provide pagination, filtering, stable error schemas, idempotency keys for mutating operations, and backward-compatibility policy.
- Provide webhooks or event delivery for package publication, repository changes, security results, and replication status.
- Provide an MCP server with tools/resources for repository discovery, package/artifact lookup, cache/health insight, and authorized administrative actions.
- MCP must use scoped credentials, enforce the same authorization checks as the API, require confirmation for destructive operations where the MCP client supports it, and emit normal audit events.
- Keep protocol traffic for package managers separate from management API versioning and document both.

### 6.12 User interface

The React console must cover:

- Overview of instance health, cache acceleration, storage capacity, upstream health, and replication.
- Repository creation/editing with format/mode-specific forms and validation.
- Package/component browsing, search, metadata, checksums, assets, and copyable client configuration examples.
- Identity provider, group mapping, user/service-account, role, token, and audit administration.
- Upstream proxy status, cache policies, prewarming, and miss diagnostics.
- Replication peers, full-feed or selective sync scope, initial seed/incremental mode, deletion policy, queues, progress, and errors.
- Storage configuration, data migration, cleanup jobs, and backup/restore guidance.

The UI must remain usable for a small single-instance installation without requiring knowledge of internal storage details.

### 6.13 Operations and lifecycle

- Provide container image and Compose installation for a small deployment; provide Kubernetes/Helm deployment for managed operation.
- Provide readiness/liveness endpoints, structured logs, Prometheus metrics, tracing hooks, and admin diagnostics bundle.
- Support TLS termination behind a reverse proxy and document trusted proxy headers.
- Provide backup/restore procedures that keep metadata and blobs consistent.
- Provide schema migration, upgrade notes, rollback constraints, and supported-version policy.
- Support maintenance tasks such as metadata repair, orphan cleanup, cache eviction, integrity scan, and replication repair.
- Provide capacity planning signals: blob storage, metadata DB, cache working set, active transfers, and file descriptors/connections.
- Secure defaults: non-root container, minimal network exposure, secret redaction, upload limits, request limits, and hardened admin endpoints.

## 7. Architecture principles

1. **Protocol adapters over one artifact core:** format behavior is modular; storage, access control, audit, and observability are shared.
2. **Metadata is not a file tree:** format metadata and indexes live in a transactional metadata service; byte blobs are immutable objects.
3. **Streaming first:** avoid loading large artifacts into memory; keep backpressure and cancellation correct.
4. **Single-node first, multi-site ready:** the default install remains simple; replication is an explicit service capability.
5. **API is the product contract:** UI and MCP call the same supported management API.
6. **Identity is pluggable, authorization is consistent:** external providers authenticate users; the product owns resource authorization semantics.
7. **Format support is evidenced:** each claimed ecosystem capability has client-level conformance results and documented gaps.

### 7.1 Logical components

- **Edge/API service:** HTTP routing, auth middleware, rate limits, UI assets, API endpoints, registry protocols.
- **Format adapters:** ecosystem-specific protocol and metadata logic.
- **Repository service:** feed resolution, source selection, publish routing, permissions, cache policy.
- **Metadata database:** repositories, components, assets, ACLs, proxy state, replication state, audit, jobs.
- **Blob service:** local filesystem or S3-compatible storage, digest verification, multipart and range handling.
- **Background workers:** metadata refresh, cache eviction, indexing, integrity scans, cleanup, replication, prewarming.
- **Web console:** React single-page application served by the Go service or deployable separately.
- **MCP service:** separate or embedded endpoint using the same API authorization and audit pipeline.

## 8. Performance and reliability objectives

Numerical service-level targets must be established from a baseline and representative workloads before release. Initial measurable objectives:

- Cached artifact delivery should be limited primarily by storage/network throughput, not package parsing or unnecessary database round trips.
- Concurrent requests for one uncached object should trigger at most one upstream fetch per cache key under normal operation.
- Large uploads/downloads must stream with bounded memory use and recover cleanly from client cancellation.
- A local single-node deployment must be useful without a cluster or mandatory external cache service.
- Integrity verification and garbage collection must not make valid artifacts unavailable.
- Replication must recover after interruption without retransferring already verified objects.

Benchmark scenarios must cover warm and cold pulls, high fan-out builds, large container layers/models, metadata-heavy package managers, proxy outage, slow object storage, and concurrent publishing.

## 9. Import, export, and adoption

- Provide generic import/export facilities for repository definitions, credentials references, metadata, and artifact content.
- Include optional migration utilities for common existing repository managers, including inventory and dry-run mapping, without making their API or behavior a Kuvryn Ant contract.
- Preserve checksums, paths, and format metadata where the source supports them; report integrity failures and unmapped configuration.
- Permit selective cache warm-up from configured upstreams; cached proxy data need not be copied by default.
- Provide a staged adoption mode that lets teams compare their existing build workflows with Kuvryn Ant before switching endpoints.
- Document endpoint migration and client configuration examples for each supported ecosystem.

## 10. Delivery phases

### Phase 0 — Product and format foundation

- Define the format capability ledger, supported client workflows, and conformance harness.
- Select license/distribution approach, supported database, API conventions, adapter extension model, and target deployment baseline.
- Build a representative client matrix and performance benchmark suite.
- Validate storage and metadata architecture with large-object and concurrent-proxy prototypes.

### Phase 1 — Full breadth and independent-instance operations

Phase 1 is the full breadth product scope, not a reduced-format MVP. It includes the listed ecosystems and their defined capabilities, local/S3 storage, modern identity integration, API/OpenAPI/MCP, CI/build acceleration, import/export tooling, operations, and explicit artifact-copying replication. Formats can be delivered behind feature flags during development, but the phase is not complete until every claimed capability meets acceptance criteria.

### Phase 2 — Federation

- Shared identity/policy model across instances.
- Instance discovery and cross-instance repository/package search.
- Global routing or placement-aware client access.
- Coordinated replication policy and conflict semantics.
- Federation health, audit, and administration.

## 11. Acceptance criteria

The product is ready for its broad-availability release only when:

1. Every supported ecosystem/capability in the format ledger passes conformance for documented client workflows.
2. Local publish/pull, remote cache hit/miss, feed resolution/routing, and same-endpoint read/write flows work for each applicable format that supports both operations.
3. Supported identity flows work for Zitadel and at least one generic OIDC provider; SAML and LDAP integrations meet their documented acceptance scope before enterprise readiness claims.
4. UI, API, and MCP enforce equivalent authorization and produce audit events.
5. A two-instance Phase 1 replication test completes initial seed and incremental sync, survives interruption, verifies digests, and reports lag/failure accurately.
6. Local disk and an S3-compatible backend pass the same artifact integrity and lifecycle scenarios.
7. Import/export tooling produces an accurate inventory and dry-run report for representative source configurations.
8. Performance benchmarks demonstrate the defined cache-hit, concurrency, memory, and large-object objectives.
9. Backup/restore, upgrade, and recovery procedures are exercised and documented.
10. No format-support claim is made without versioned evidence and a stated known-limitation list.

## 12. Risks and open design decisions

- **Full breadth is a large engineering surface.** Each ecosystem has non-obvious client behavior, metadata formats, and publishing semantics. The format capability ledger and conformance harness are foundational, not documentation polish.
- **Proxy metadata can contain absolute upstream URLs or signatures.** Adapters must determine when rewriting is required and how to preserve integrity.
- **Feed resolution varies by ecosystem.** Metadata aggregation must be format-aware; simple path fallback is not sufficient for every protocol.
- **S3-compatible does not guarantee identical consistency or multipart behavior.** Maintain a tested backend capability matrix.
- **Identity and package-client auth differ.** Browser SSO cannot be reused blindly for Docker or package-manager traffic; auth flows must be tested with real clients.
- **Replication requires a clear authority model.** Phase 1 avoids concurrent writers by default; any multi-writer mode requires conflict semantics before implementation.

Open decisions for the next design pass:

1. License/distribution model.
2. Exact initial scope for local publishing, remote sources, and read-resolution policies by ecosystem.
3. PostgreSQL-only production metadata vs optional embedded database mode.
4. Adapter extension model: built-in modules, Go plugins, or external adapter processes.
5. Which multi-source read and routed-publish workflows should be supported for Docker/OCI and npm.
6. Which SAML, LDAP, SCIM, and external secret/KMS capabilities are required for the first generally available release.
7. Explicit SLO numbers and reference hardware/workload profiles.

## 13. Reference inputs

The ecosystem list is informed by established repository-manager format inventories. These references are useful for identifying package families only; they do not define Kuvryn Ant APIs or behavior:

- [Sonatype Nexus Repository Formats](https://help.sonatype.com/en/formats.html)
- [Sonatype Repository Types](https://help.sonatype.com/en/repository-types.html)
- [Sonatype NuGet Repositories](https://help.sonatype.com/en/nuget-repositories.html)

---

**Change log**

- 0.1 — Initial working specification capturing Kuvryn Ant product vision, broad ecosystem support, build-factory acceleration, identity integrations, local/S3 storage, Phase 1 artifact replication, and Phase 2 federation.
