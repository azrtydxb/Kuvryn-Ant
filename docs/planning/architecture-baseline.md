# Architecture decisions — revision 1

Status: Decided for implementation on 2026-10-18 under the owner's instruction to resolve technical planning autonomously. This is a design record, not an implementation or security certification. No additional owner approval gate is required for these engineering choices. Product scope changes still require discussion.

## A01 — Runtime, adapters and deployment

Build one Go service with built-in adapters, a React/TypeScript console and PostgreSQL. Use Go's standard HTTP server, explicit domain interfaces and SQL migrations; do not require a message broker, external cache or dynamic plugin ABI. Go plugins add binary compatibility/deployment constraints without a demonstrated requirement; external adapter processes are deferred.

An adapter owns protocol parsing/routes, coordinate normalization, indexing/composition, client-auth translation and its capability descriptor. It must use shared artifact/policy/auth/audit services rather than directly modifying storage. Descriptor fields include protocol revision, local publish mode, remote/replica support, allowed policies, index ownership, signing, deletion and conformance evidence.

Initially support one serving process per independently administered instance, with bounded background workers in that process. Acquire a database-backed instance lease with fencing; a second process using the same instance identity must not become writable. Helm rolling replacement uses a recreate strategy until multi-process serving is explicitly designed and tested. Replicas are separate instances, not additional writable pods sharing one instance identity.

Primary deployment validation is the owner's Oracle OKE with existing MinIO on TrueNAS SCALE. Disk and generic S3-compatible storage remain product capabilities, including AWS and other compatible providers. Do not provision a provider test fleet. A reusable backend suite runs first against disk and the existing MinIO; additional provider evidence is collected when needed. Compose remains the small-installation/development path.

## A02 — Domain model and transactions

Use PostgreSQL-generated UUID identifiers, UTC timestamps, explicit revision counters and foreign keys. Separate instances, feeds, sources, namespaces, components, versions, assets, blob references, tags, format metadata, principals, grants, audit, outbox, jobs and replication checkpoints.

Enforce unique `(source_id, normalized_coordinate, version, asset_key)` and `(dedup_domain_id, digest_algorithm, digest)` constraints. Source types are local, remote and replica; their administrative permissions do not grant feed read access. Feed slugs are unique per instance. Namespace ownership prevents silent fallback to public packages with the same private name.

Publication sequence: authorize and resolve one local destination; stage bounded streaming content; verify declared size/digest and adapter metadata; durably finalize blobs; commit all version/asset/index references, audit and outbox records together; expose only committed state. An index snapshot carries its generation and referenced assets. This avoids a distributed transaction between database and object store. Failed metadata commits can leave unreferenced bytes, never valid partial packages.

Use transaction-scoped locks for coordinate mutation and conditional revision checks for mutable configuration/tags. Identical immutable content can be retried; conflicting immutable content returns a protocol-specific conflict. Mutable overwrite and deletion are explicit policies with audit/provenance. PostgreSQL is mandatory for development and production; migrations serialize under an advisory lock, check schema compatibility at startup and document forward/rollback constraints.

## A03 — Blob interface and safety

Blob operations are stage/write/finalize/abort/open-range/stat/delete with context cancellation, digest/size results and typed capability errors. SHA-256 is the internal content identity; preserve additional ecosystem checksums as metadata. The backend is never the authority for package paths or object reachability.

Disk uses a private data directory, safe paths, temporary objects on the same filesystem, verified finalize, fsync of required objects/directories and atomic name publication. S3 uses configured endpoints, SigV4, bucket/prefix, region, path-style or virtual-host addressing, explicit trust roots and server-side encryption settings. Do not equate multipart ETags with whole-object digests. Multipart checkpoints retain upload IDs/parts; failed/cancelled uploads are aborted or reaped.

Budget streaming buffers globally. Default application buffer budget is 1 GiB, with at most 64 MiB charged per active large transfer; queue excess operations. No arbitrary default object-size ceiling below the large-model workload; administrators configure quotas and request limits. Header timeouts are bounded; body deadlines use idle/progress policy so legitimate large uploads are not cut off by a short fixed request deadline.

Use a 24-hour default orphan grace period plus live reference/lease checks. GC excludes committed assets, active transfers, replication manifests and migration/backup pins. Deleting an unreferenced blob is idempotent; a failed deletion is retried and observable. Storage migration copies/verifies first, journals new writes, reaches a verified cutover barrier, and switches backend references without changing feed URLs.

## A04 — Routing and composition

Feed endpoints are stable for their ecosystem conventions. Management uses `/api/v1`; general adapters use `/feeds/{slug}/...`. OCI uses `/v2/{feed}/{repository}/...`; Terraform uses a feed-bound host with service discovery; a feed may also use its own hostname when a client requires root-relative paths. The console generates valid client examples instead of claiming every ecosystem accepts arbitrary URL prefixes.

Read policy defaults to `package-owner`: explicit namespace binding first, otherwise the first source that owns the normalized package. All versions, mutable pointers and metadata for that package remain with that source; missing versions do not silently switch owners. Raw paths are independent ownership units. Signed repository formats use `snapshot-owner` for the whole configured channel/distribution/component/architecture or repository snapshot. No generic index concatenation or signature rewriting.

An optional ordered fallback searches sources only before ownership is established. Not-found fallback, upstream-error failover and offline/stale serving are separate settings, disabled unless selected. Bind every resolution to a generation/source token so subsequent artifact fetches cannot mix metadata and bytes. Publish promotion from remote to local ownership is an explicit operation requiring a new coherent index generation and cache invalidation.

Write rules use ordered integer priority and bounded selectors: exact namespace, package scope, or normalized path prefix; optional principal/service-account equality. Highest matching priority wins. Configuration validation rejects overlapping selectors at equal priority with different targets; arbitrary regex is excluded. Deny rules override allow rules. The winner must be one local source, otherwise reject before accepting upload. No match uses the single configured default local destination or rejects. Authorization is evaluated separately against the resolved target.

Explain output includes effective rules, winner, skip/failure reasons, source generation and stale state, with permission-aware redaction. Nested feeds are excluded; source references and any policy/template references must be cycle-free.

## A05 — API, retry and compatibility

Use `/api/v1`, JSON management payloads, stable error codes with request IDs, opaque cursor pagination (default 50, maximum 200), bounded filters and ETag/If-Match configuration updates. Keep package protocol errors and routes adapter-specific. OpenAPI and generated clients are release artifacts; schema conformance is a CI check.

Control-plane mutation idempotency keys bind principal, method/operation, canonical resource and request fingerprint. Retain outcomes for 24 hours by default. Concurrent duplicate requests wait for or receive the original result; a mismatched fingerprint returns conflict. Do not replay a response containing a newly issued token secret indefinitely; issuance uses a short-lived encrypted result with one-time retrieval policy, while retaining non-secret mutation identity.

Version 1 changes are additive unless explicitly negotiated. Breaking changes require a new major management API and migration guide. Support the current and preceding product minor for documented upgrades after a second minor exists; never promise a rollback across an irreversible migration. Protocol compatibility is defined by the client matrix, not by the management API version.

## A06 — Authentication and authorization

Implement OIDC authorization code + PKCE (S256) through a backend-for-frontend session. The React app does not persist bearer tokens in browser storage. Validate issuer/audience/signature/time/state/nonce and exact redirect URIs; bound discovery/JWKS fetches and test key rotation. Cookies are Secure/HttpOnly with appropriate SameSite and CSRF protection. Default session maximum 8 hours, inactivity timeout 30 minutes; administrative policy can tighten these.

SAML 2.0 is required at GA: SP-initiated browser SSO, trusted signed assertions, response correlation, audience/recipient/time checks, replay cache, secure XML parsing, NameID/attribute mapping, metadata exchange and certificate rollover. Support signed logout where the provider supports it. IdP-initiated unsolicited login and dynamic untrusted metadata are excluded initially. Keep consumed signed XML nodes tied to validated assertions to prevent signature wrapping. Use reviewed libraries rather than hand-rolled cryptography.

LDAP/AD is also required: LDAPS or mandatory StartTLS; escaped search filters; bounded search results/time; explicit nested-group depth (default 10), cycle detection and fail-closed group lookup. OIDC baseline fixtures are Zitadel and Keycloak; Keycloak also provides the SAML fixture. OpenLDAP provides directory fixtures, with an AD compatibility scenario; fixtures run as needed, not permanent new infrastructure.

Principals and memberships resolve to internal roles. Explicit deny wins; otherwise union permitted grants within resource scopes; absent grant denies. IdP mapping never implicitly creates an administrator. Separate read/publish/delete/configure/replicate/admin. Anonymous read is off by default. Service tokens have random secrets, hashed verifiers, expiry/revocation and narrow scopes; protocol token/basic/API-key exchanges use this same authorization pipeline. Emergency administrator login is restricted, rate-limited and audited, with Argon2id password hashing and documented operator recovery. SCIM is deferred, while preserving external immutable identity/group IDs.

References: [OIDC Core](https://openid.net/specs/openid-connect-core-1_0.html), [Zitadel code/PKCE integration](https://zitadel.com/docs/guides/integrate/login/oidc/login-users), [Keycloak administration](https://www.keycloak.org/docs/latest/server_admin/index.html), [SAML Core](https://docs.oasis-open.org/security/saml/v2.0/saml-core-2.0-os.pdf). Protocol requirements come from these sources; session limits and scope above are Kuvryn Ant design decisions.

## A07 — Secrets and audit

Use AES-256-GCM envelope encryption with a random nonce per encryption and authenticated context (record identity, purpose, key version). Supply the root key through a mounted secret file outside PostgreSQL/blob storage; never commit it or pass it as a CLI argument. Keep active and previous key versions for resumable rotation, then retire old keys after verification. Missing keys fail dependent configuration closed and show a redacted diagnostic. External KMS integrations can be added through a wrapping-key interface; none is required for Phase 1. OKE Kubernetes secret encryption is defense in depth, not a replacement for application-level ciphertext.

Audit successful mutations in the same transaction as metadata/outbox changes. Record failed authentication/authorization as independent security events without plaintext credentials. Audit exports require explicit grants; retention is administrator-configurable with a documented default and legal/commercial choices left to the operator. Webhooks use signed events, bounded retries and at-least-once delivery IDs. Scanners/signing/provenance integrations are optional consumers and cannot block artifact serving.

## A08 — Jobs, cache and failure recovery

Use PostgreSQL jobs with a 30-second lease, 10-second renewal, fencing generation and default four workers. Reclaim expired leases only after verifying ownership epoch; jobs are idempotent and durable. Resume eligible jobs within 60 seconds after dependencies recover. No external broker requirement.

Coalesce cache misses per key in the single serving process; do not claim multi-node coalescing. Keys include source, normalized identity, immutable version/digest or metadata generation, representation and credential/authorization domain where responses vary. Never cache an unauthorized response as a global miss. Defaults: metadata freshness 5 minutes, negative-cache TTL 30 seconds, immutable blobs indefinite until retention; adapters can tighten these. Stale serving is opt-in and never extends signed metadata beyond its valid signature/expiry policy.

Retry safe upstream reads at most three attempts with capped exponential backoff/jitter; no retry of a non-idempotent write. Circuit-break repeated failures and expose state. TLS verification is mandatory; upstream network policy rechecks DNS/redirect destinations and prevents credential forwarding to untrusted origins. Signed external asset links are resolved internally under source policy or explicitly denied; an incomplete referenced asset closure cannot be reported as a complete offline replica.

## A09 — Replication and backup consistency

A full-feed snapshot records a manifest generation and a high-water change cursor. Retain the corresponding blobs/metadata until seed acknowledgement. Transfer verified objects, apply the snapshot, then replay changes after the cursor. Continuous and scheduled jobs use durable checkpoints; duplicate events and tombstones are idempotent. Metadata/tags/indexes/checksums and cached pull-through content are included by default. Selective sync records what was intentionally omitted.

An authoritative source owns its replica records. Tombstones delete only that authority's replicated objects, never destination-only objects; show dry-run impact and require configured propagation policy. Destination policy, credentials and IdP settings remain local. Reviewed non-secret templates are separate imports. Source unavailability cannot prevent reads from a fully materialized seeded snapshot. No implicit concurrent-writer merge.

Backup uses a consistent metadata snapshot plus a pinned blob-reference manifest; briefly fence new publication when taking the snapshot boundary, then release writes while pinned bytes copy. Restore validates every required digest before reopening writes. Data-loss and restore-time goals are in the workload plan. Database-only or TrueNAS-only snapshots are not claimed to be complete application backups.

## A10 — Review outcome and alternatives

Resolved Q05–Q10: built-in adapters; deterministic ordered publish rules; package/snapshot ownership; external root-key envelope encryption; `/api/v1` with defined retry/evolution semantics; measurable workload gates. Rejected for Phase 1: plugin ABI, arbitrary regex routing, generic index unions, broker requirement, distributed write transactions and writable active-active pods. These add unresolved failure modes without being required by the spec.

No blocking architecture decision remains. Exact deployed OKE/TrueNAS/MinIO versions and addresses are deployment inventory, not a reason to stop core development. Product license remains deliberately unselected. Infrastructure provisioning and performance/conformance evidence remain implementation tasks; this record does not claim they have run.
