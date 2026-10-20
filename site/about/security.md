# Security model

This page summarizes how Kuvryn Ant protects credentials, artifacts and administrative access, and what it expects from the operator. Each statement links to the page that documents the mechanism in detail.

To report a vulnerability, follow the [security policy](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/SECURITY.md): use GitHub's private vulnerability reporting, never a public issue.

## Secrets at rest

- **Envelope encryption under an external root key.** Every stored secret (remote-source credentials, audit-sink credentials, identity-provider secrets, federation keys) is encrypted with a fresh random 256-bit data key using AES-GCM; the data key is wrapped by the instance root key. The root key is a 32-byte file mounted from outside (`ANT_ROOT_KEY_FILE`) and is never stored in PostgreSQL or in blob storage. See [Root key](../install/root-key.md).
- **Bound to their owner.** Each encrypted record is authenticated together with its purpose and owning record ID (for example the remote source's UUID), so a ciphertext cannot be moved to another source or purpose and still decrypt.
- **Rotation.** The keyring is versioned: `ANT_ROOT_KEY_VERSION` names the active key and `ANT_PREVIOUS_ROOT_KEYS_FILE` holds older versions while `ant rotate-secrets` rewraps every record.
- **Never returned.** Credentials are write-only in the API (`writeOnly` in the contract): responses only say whether a credential is set. They are never logged, never included in exports or diagnostics bundles, and backups contain only the ciphertext (the root key is never in a backup).
- **Tokens are stored as verifiers**, not as secrets. A service token or personal access token secret is shown once, at issue time.

## Upstream (remote source) safety

- **Credentials only go to their origin.** A remote source's credential (`Authorization` value or API key) is attached only to requests to that source's configured origin. Hosts the upstream redirects downloads to must be listed in `redirect_origins`; they are allowed as download targets but never receive the credential. Redirects to any other host are refused. During registry token handshakes, the realm is checked the same way and a configured credential is never sent to a CDN host.
- **HTTPS origins and private-network protection.** Origins must be `https`. Connections to non-public addresses (private, loopback, link-local) are refused unless the source sets `allow_private`, which limits server-side request forgery through a remote source.
- **CA references are operator IDs.** A remote source can only reference a CA bundle by an identifier from the operator-mounted map (`ANT_UPSTREAM_CA_MAP_FILE`); API callers can never supply a filesystem path.
- **No self loops.** Remote sources and replication peers that point back at the instance's own public URL are rejected at configuration time.
- **Publishes never go upstream.** A publish resolves to exactly one local source through the feed's publish policy; nothing is ever written to a remote source or forwarded to an upstream. See [Publish policies](../concepts/publish-policies.md).

## Dependency confusion

Read resolution is **owner first**: before any upstream request, Kuvryn Ant looks up which source owns the requested package in the feed. If a local source owns the name, the request is answered from local content only, and a public source can never supply a version of that package. A not-found from a remote falls through to the next source, but an upstream **error** stops the search (503), so an outage of a private source cannot silently resolve the name from a lower-priority public one. `GET /api/v1/feeds/{id}/resolve` explains the decision for any coordinate. See [Read order and resolve explain](../concepts/resolution.md).

## Integrity

- Every published or cached blob is verified against its digest and size before it becomes visible; publication is staged and committed atomically, so interrupted or corrupt uploads are never served.
- When the format supplies a digest (OCI, PyPI files), a pull-through miss is fully staged and verified before any byte is served.
- A rolling integrity scan re-hashes stored blobs and records mismatches (see [Storage maintenance](../operations/storage-maintenance.md#integrity-scan)).
- Blobs are never deleted inline; garbage collection re-checks references under a row lock.

## Authentication and authorization

- **Identities**: OIDC, SAML and LDAP browser sign-in, local accounts, a break-glass account that works when SSO is down, service accounts and personal access tokens, and GitHub Actions OIDC exchange for short-lived scoped tokens. See [Identity and access](../identity/index.md).
- **Scoped tokens.** Token scopes name actions (`read`, `publish`, `delete`, `configure`, `replicate`, `admin`), optionally limited to a feed and a namespace, and every token has an expiry. A token's effective permission is the intersection of its scopes and the principal's role grants.
- **Explicit deny wins** over any allow. See [Roles and grants](../identity/roles.md).
- **One authorization boundary.** The console, the management API and MCP (`/mcp`, which calls the management handlers in process) share the same authorization checks and audit.
- **Browser sessions** use HttpOnly cookies and CSRF protection. Login never implicitly creates an administrator.
- **Throttling.** Authentication failures are rate limited and audited (`auth.denied`); local accounts lock after repeated failures (see [Break-glass access](../operations/break-glass.md)).

## Audit

Every management mutation writes its audit event in the **same database transaction** as the change, so the audit log cannot miss a committed change or record one that rolled back. The audit log can be streamed to a SIEM over syslog or HTTPS with a durable checkpoint, and audit retention never purges events the sink has not exported. See [Observability](../operations/observability.md#audit-log).

## Network and deployment

- **Trusted proxies.** `X-Forwarded-For` and `X-Real-IP` are believed only from the CIDRs in `ANT_TRUSTED_PROXIES` (chart value `trustedProxies`); by default no forwarded header is trusted. Download URLs are built from `ANT_PUBLIC_URL`, never from forwarded headers or the request `Host`. See [Ingress, Gateway API and TLS](../install/ingress-tls.md).
- **Metrics are not public.** `/metrics` has no authentication and the chart's Ingress and HTTPRoute do not route it; scrape it in-cluster and restrict it with a NetworkPolicy. Metric labels never contain paths, package names, principals or credentials. See [Observability](../operations/observability.md#metrics-endpoint).
- **Hardened container.** The image runs as a non-root user (UID 10001). The chart sets `runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`, drops all capabilities, uses the `RuntimeDefault` seccomp profile and does not mount a service account token. Writable paths are the data volume and `/tmp` only.
- **NetworkPolicy.** The chart can deny all ingress and egress except explicitly listed peers (`networkPolicy.ingressFrom`, `networkPolicy.egress`); no provider destination is implicitly trusted.
- **S3 and PostgreSQL TLS.** HTTPS certificate verification stays enabled for object storage (custom CA via `ANT_S3_CA_FILE`); plain HTTP to object storage requires the explicit `ANT_S3_ALLOW_INSECURE_HTTP`. Use TLS with a verified CA for PostgreSQL.
- **Single writer.** A database writer lease ensures only one process writes an instance; background workers (GC, replication, webhooks, audit purge and forwarding) run only under that lease.

## Operator responsibilities

- Protect the root key and the previous-keys file in a secret manager, separately from database and blob backups.
- Restrict network access to `/metrics`, the PostgreSQL server and the object store.
- Terminate TLS in front of the service and set `ANT_PUBLIC_URL` to the external HTTPS origin.
- Keep the break-glass password in a vault and reset it after every use.
- Review diagnostics bundles before sharing them outside your organization; redaction is defence in depth, not a guarantee.

::: warning Gap
No independent third-party security audit has been published. The federation assertion, proxy and blob-URL code have had internal reviews and fixes; an independent review is still an open item on the roadmap.
:::

## Reporting a vulnerability

See [SECURITY.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/SECURITY.md). In short: report privately through GitHub's private vulnerability reporting, include the affected version or commit and reproduction steps, and do not include real credentials. You will get an acknowledgement within 5 working days. Until a stable release exists, security fixes are made on `main`.
