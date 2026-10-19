# Phase 2 — Federation design

Status: Design draft, revision 1, 2026-10-18. Satisfies the M10 deliverable in the [delivery plan](delivery-plan.md) (scope, threat model, consistency model, migration strategy, acceptance criteria). Implementation starts when Phase 1 (M09) closes. The six owner decisions in §19 have recommended defaults, and work packages are written against those defaults.

Implementation status: P2-M1 (membership foundation) is implemented in `internal/federation` (+ `peer`, `harness`), `internal/management/federation.go`, `cmd/ant/federation.go`, the console Federation page and the Helm `federation` values; see [docs/operations/federation.md](../operations/federation.md). Migration numbers moved: the core schema landed as `031_federation_core.sql`, and later migrations take the next free number at merge time. M1 deviations: request nonces are cached in memory (the `federation_assertion_nonces` table is reserved for P2-M2 assertions); the `ant federation` CLI (re-identify, purge) and the fault-injecting TCP proxy and fake clock of the harness are deferred to P2-M2/P2-M6; documents are canonicalized as described in §4.1, with the chain hash taken over the canonical document including its signature.

P2-M2 (shared identity and policy) is implemented: migration `034_federated_identity.sql` (origin columns, inert grants, guard triggers with SQLSTATE `AFM01` → `409 federation_managed`, group bindings, observed sign-in groups, policy head and drafts, local settings in `instance_settings`), `internal/federation/{policy,policy_sync,reconcile_policy,local_policy,assertion,alerts,maintenance}.go`, `internal/federation/peer/assertion.go`, `internal/management/federation_policy.go`, the console Federation policy section, MCP `get_federation_policy`, `publish_federation_policy`, `check_federation_access`, `list_federated_principals` (+ `ant://federation`, `ant://federation/members`, `ant://federation/policy`), the `ant federation status|re-identify|purge` CLI, and the harness fault proxy (per-instance ingress: pass, drop, blackhole, delay) and fake clock. M2 deviations: `federated_feed_bindings` moved forward from `025` because policy grants resolve through it (P2-M3 extends it with roles and descriptors), and its API is `/api/v1/federation/feed-bindings` (feed keys contain a slash); `principals.federated_id` is derived (name-based UUID over federation, issuer and subject) instead of assigned, so every member computes the same value; assertions sign the exact transmitted claim bytes (not JCS) and are consumed in M2 by `POST /federation/v1/access-check` (exposed as `POST /api/v1/federation/access-check`), with search (P2-M4: `internal/federation/search.go`, `internal/management/federation_search.go`, `GET /api/v1/federation/search`, MCP `federated_search`, `federated_lookup_checksum`, console scope toggle; see docs/operations/federation.md) and proxy consumers following in P2-M4/P2-M5; anonymous assertions are not issued yet; revocations take effect on apply (`effective_at` is informational); OIDC groups come from the ID token `groups` claim. The assertion code has had an implementation self-review against §14; the independent security review remains an open exit item.

Security review fixes (after P2-M4): signatures and keys accept only canonical base64url, and a member whose membership, policy or topology chain diverges recovers by the §6.4 rule 6 snapshot resync from the authority (migration `036_federation_hardening.sql` lets the document log delete only under `ant.federation_resync=on`); `/join` checks invitations before the state lock and is rate-limited per IP with throttled rejection audit; recovery-key rotation is signed offline (`ant federation recovery sign-rotation|sign-authority-cert`) and the API accepts only signed statements; federated artifact/feed allows without a feed key are gated by `accept_admin_grants` (rule 4); peers accept only a member's published certificate (previous one for 24 h after renewal, then revoked) and renewal CSRs must keep or be signed by the current key; the authority enforces unique member hosts and joiners check (and can pin) the authority certificate; request nonces, signed requests and assertion nonces are capped per member; signed documents refuse numbers beyond 2^53 - 1, receivers derive `fed_id` locally, and new federation CAs are name-constrained. See docs/operations/federation.md, Security controls.

P2-M3 (coordinated replication) is implemented: migration `035_federated_replication.sql` (federation-mode peers with `auth_mode`/`member_instance_id` and nullable token, job `origin`/`federation_rule_id`/`federation_feed_key`/`adopted_at`, topology head, `federation_home_feeds`, `federation_feed_freezes`, `federation_transfer_steps`, `federation_quarantines`, `federation_rule_status`), `internal/federation/topology{,_store,_api}.go` (signed topology document with feed keys, homes, descriptors, transfer state machine and rules; authority APIs; transfer driver; compliance), `internal/replication/federated.go` (reconciler: rule → peer/job, adoption, release, re-point, promote, demote/quarantine), the export API on the federation listener (`replication.FederationExport`), the artifact transfer freeze (`503 feed_transferring`), `internal/management/federation_topology.go`, the console feeds/rules/compliance/transfer section, MCP `get_federation_topology`, `list_replication_rules`, `get_replication_rule_compliance`, `get_federation_replication_status`, `set_replication_rule`, `adopt_replication_job`, `start_feed_transfer` (+ `ant://federation/topology`), and harness scenarios for rule → job creation, rule edits, partition, adoption without reseed, planned transfer and forced transfer with quarantine. M3 deviations: the federation export is authorized from the applied, authority-signed topology (caller targeted by a rule for the key, or a party of its transfer) instead of a generated `fed:replication` role for a member service principal; descriptors carry format and anonymous-read only (adapter `federable_options` deferred); feed-key bindings keep the M2 API and the local role (home/replica) is derived from topology instead of a binding column; feed keys have no PATCH (homes change only by transfer); the planned demotion converts the old home's single local source into the replica source, re-stamps its provenance and starts with a manifest walk that deduplicates every blob instead of starting incremental at the new home's baseline (several local sources fall back to quarantine); a forced transfer quarantines the old home's whole local source(s), detached, with the count of changes after the target's checkpoint, and seeds a fresh replica; other members re-point with a reseed against the new home; a removed rule releases its job to origin local (disabled) instead of keeping it federation-managed; the transfer driver runs in the authority's reconcile loop and polls the old home and target over mTLS (`/federation/v1/replication/transfer-state`); the Phase 1 replication suite runs unchanged in token mode and the harness covers federation mode end to end.

P2-M5 (placement-aware routing) is implemented: per-feed-key `routing` in the signed topology (`mode proxy|redirect|off`, `blob_urls`, `cache`, `cache_ttl_seconds`, `max_lag_seconds`, `prefer`; `PUT /api/v1/federation/feeds/{fed}/{slug}/routing`), `internal/federation/route_{placement,proxy,serve,cache}.go` (placement from topology homes and rule targets, replica state from members' replication status, RTT EWMA from pings and proxied requests, circuit breaker; the adapter wrapper in `cmd/ant` that routes GET/HEAD after a local 404 or for keys without a local feed; the mTLS proxy endpoint `/federation/v1/proxy/feeds/{fed}/{slug}/…` and `GET /federation/v1/routes/feeds/{fed}/{slug}`; signed blob URLs on the public `/federation/blob/{name}`; the optional entry cache), migration `037_federation_routing.sql` (`federation_route_cache`, a GC root), `GET /api/v1/federation/routes/{fed}/{slug}`, the `federation` plan in `GET /api/v1/feeds/{id}/resolve`, the console routing column, explain and editor, metrics, and harness scenarios (proxied read from the home with Range and HEAD, member authorization, no write routing, latency-based selection, local replica miss, failover, circuit and 503, redirect, signed blob URLs, entry cache). M5 deviations: metadata URLs are rebased by the entry member with a bounded streaming rewrite of the holder's API URL and feed path (textual, non-content-addressed bodies and `Location`/`Link`) instead of an adapter `X-Ant-Fed-Public-Base` hook, so no adapter declares `federation_proxy` and every adapter is routed; replica health comes from the members' replication status (`/federation/v1/replication/status`, cached 15 s) and RTT from the 30 s status pings, not a new status document; the transfer budget is a per-member stream limit (64), not the A03 global buffer budget; blob URL tickets reuse the assertion claim set (five-minute lifetime, multi-use, no IP binding, no replay table) and are signed by the entry member; proxied-read assertions use an in-memory replay cache; redirect applies only to entry members without a local copy; `/api/v1/federation/routes/explain?feed_key=&path=` became `GET /api/v1/federation/routes/{fed}/{slug}` (the decision does not depend on the path); MCP `explain_route` and the client conformance federation profile are not part of this increment; the independent security review of the proxy and ticket code remains an exit item.

P2-M6 (operations) is implemented: migration `038_federation_operations.sql`, `internal/federation/{authority,joinops,audit_query}.go`, `internal/management/federation_authority.go`, the console Federation page sections "Authority" and "Federated audit", MCP `get_federation_authority`, `handover_federation_authority`, `federated_audit` and the deferred `explain_route`, the offline `sign-authority-cert` (forced recovery) and `sign-challenge` (drill) commands, the runbooks in [docs/operations/federation-runbooks.md](../operations/federation-runbooks.md) and harness scenarios for handover (and back), forced recovery with fencing and a partition, recovery of a compromised authority, the drill, intercepted joins, revocation pruning and the federated audit. M6 deviations: a document that changes the authority epoch also carries the new authority member's own acceptance (a signature by its member key), so neither the previous authority nor the recovery-key holder can appoint a member that did not agree; a planned handover transfers the federation CA key (sealed only by the mutually authenticated channel) because the new authority must keep issuing certificates, while the epoch signing key is never exported; a forced recovery creates a new CA (both CAs stay trusted, a later recovery displaces the oldest) and the new authority keeps presenting its old listener certificate until it renews, because members learn the new CA only from the recovery document; invitations pin the CA of the authority's listener certificate; `ant federation purge` and `re-identify` were delivered in M2; the "recovery key loss" case (a new recovery key certified by the current authority plus a second administrator) is covered by the offline rotation of M1 and the drill, not by a second-administrator flow; federated audit requires instance administration on each member (the grant of `GET /api/v1/audit`) and does not filter by principal ID because principal IDs are member-local; the revoked-serial list is pruned by age (32 days after first publication, certificates live 30) because membership documents carry serials only; joining is two-phase (claim, then confirmation by the joiner), so a relayed join no longer consumes an invitation; alerts `authority_fenced`, `handover_pending`, `recovery_key_unverified` and `member_unreachable` and metrics `ant_federation_authority`, `ant_federation_member_up` and `ant_federation_member_rtt_seconds` were added; Helm needs no change. Live qualification on OKE (authority) and kw (member) is recorded in [docs/implementation/federation-evidence.md](../implementation/federation-evidence.md) with raw outputs under `evidence/2026-10-19/federation/`: in-place adoption of the nine replication jobs, federated search and proxied reads at kw, a planned home transfer to kw and back, and a 16-minute partition with last-good policy, alerts and resync; the authority handover and recovery scenarios were qualified in the harness only. Findings (kw reachable in one direction only, the 10 Mbit/s federation LoadBalancer, partition policies) are in the evidence document; the peer protocol is published as `api/federation-openapi.yaml`. The client conformance federation profile is still not part of this increment. The independent security review of the proxy, ticket and epoch code remains an exit item.

Inputs: [product spec](../../kuvryn-ant-product-spec.md) §6.8, §10 Phase 2, §12; [architecture baseline](architecture-baseline.md) A01–A10; current code in `internal/replication`, `internal/identity`, `internal/directory`, `internal/oidclogin`, `internal/samllogin`, `internal/management`, `internal/mcp`, `internal/events`, migrations `001`–`022`.

---

## 1. Goals and non-goals

### Goals

| ID  | Goal                                                                                                                                              | Spec                   |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------- |
| G1  | Group independently operable instances into a named **federation** with authenticated, signed membership.                                         | §6.8 discovery         |
| G2  | **Shared identity and policy**: federated principals, groups and role bindings defined once, distributed with versions, enforced locally.         | §10 P2 bullet 1        |
| G3  | **Cross-instance search** of repositories and packages, filtered by the caller's authorization on each member.                                    | §10 P2 bullet 2        |
| G4  | **Placement-aware access**: a client can reach content held on another member (proxied or redirected) based on health and latency.                | §10 P2 bullet 3        |
| G5  | **Coordinated replication**: federation-level rules that generate and supervise Phase 1 replication jobs, with one authoritative writer per feed. | §10 P2 bullet 4        |
| G6  | **Federation health, audit and administration** through UI, API and MCP with the same authorization as the API.                                   | §10 P2 bullet 5, §6.11 |

### Non-goals

- **No hard runtime dependency.** Serving, publishing, local authorization and Phase 1 replication never wait on another member or on the federation authority. A member partitioned for a long time keeps working with its last-known-good state.
- **No shared database, no consensus cluster, no broker.** Each member keeps its own PostgreSQL, blob store and instance writer lease (A01, A08). Federation state is signed documents copied between members.
- **No multi-writer feeds.** Each federated feed has exactly one home (authoritative writer). Concurrent writers and merge semantics are out of scope (spec §12). A planned home transfer is in scope (§9.5). Automatic failover is not.
- **No global index replication.** Search fans out queries (§7). Package metadata is not copied for search.
- **No federation of secrets.** Upstream credentials, IdP client secrets, service-token secrets and root keys stay on each instance.
- **No cross-instance token validation.** A token issued by member A is never accepted directly by member B. Cross-member requests carry a signed principal assertion over mTLS (§6.5).
- **No SCIM, no new IdP types.** Federation reuses the Phase 1 OIDC/SAML/LDAP integrations.
- **No GSLB/DNS product.** Kuvryn Ant exposes health and latency signals plus routing behaviour. Operators may put an external GSLB in front.

---

## 2. Current-state baseline (what Phase 2 builds on)

| Area              | Present today                                                                                                                                                                                                                                                                                                           | Implication for federation                                                                                                                                                                                  |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Instance identity | `instances(id uuid, name)`, `ANT_INSTANCE_ID`, `instance_writer_leases` with fencing                                                                                                                                                                                                                                    | The instance UUID becomes the member ID. Restored backups clone the UUID, so join must reject duplicates and a "re-identify" procedure is required (WP-1.2).                                                |
| Principals        | `principals(instance_id, kind user\|service\|emergency, issuer, subject)`. `oidc_identity_bindings` maps issuer+subject to existing principals and never provisions.                                                                                                                                                    | `(issuer, subject)` is already a portable identity key. Emergency and local service principals must never be federated.                                                                                     |
| Roles/grants      | Per-instance `roles`, `principal_roles`, `role_grants(kind, effect, feed_id uuid, source_id uuid, namespace, actions)`. Deny wins, union of allows, absent denies (A06).                                                                                                                                                | Grants reference **local UUIDs**, so a federated grant has to reference a portable feed key that is resolved locally (§6.3). The evaluator (`identity.Policy.Authorize`) does not change.                   |
| Group mapping     | LDAP `GroupRoles` (immutable group ID → role IDs) in a config file. SAML extracts `Groups`. OIDC has no group mapping.                                                                                                                                                                                                  | A federated **group binding** (IdP + group ID → federated role) replaces per-instance file mapping for federated roles. Local file mappings keep working.                                                   |
| Tokens            | Hashed verifiers, scopes `{feed_id, namespace, actions}`, revocation rechecked in `Roles.Resolve`                                                                                                                                                                                                                       | Scopes travel inside principal assertions, and the receiving member intersects them with its own policy.                                                                                                    |
| Replication       | Pull-based. Authority exports `head/manifest/changes/blobs` and requires the `replicate` action. Destination peers use URL + `ca_ref` + per-peer bearer token in `encrypted_secrets`. Jobs create a lowest-precedence `replica` source. Gapless commit-ordered change feed. Tombstones are policy-gated with a dry run. | Federation adds a `federation` peer auth mode (mTLS + assertion of the member's replication principal) and a reconciler that owns jobs created from federation rules. Export and apply code stay unchanged. |
| Config templates  | Not implemented (README limitation)                                                                                                                                                                                                                                                                                     | Federated feed descriptors (§9.2) are the reviewed, non-secret template mechanism. Phase 2 delivers it.                                                                                                     |
| Events            | Transactional outbox, HMAC-signed webhooks, at-least-once                                                                                                                                                                                                                                                               | Federation document changes produce outbox events (`federation.*`), and members get nudges through the same dispatcher pattern.                                                                             |
| MCP               | Tools dispatch in-process to management handlers. Destructive tools need `confirm: true`. Calls are audited.                                                                                                                                                                                                            | New federation tools are thin wrappers over the new management endpoints. No new authorization code.                                                                                                        |
| API               | `/api/v1`, cursor pagination, ETag/If-Match, idempotency keys, `api/openapi.yaml` → `api/generated.go` + `web/src/generated.ts` (`scripts/check-generated.sh`)                                                                                                                                                          | Management additions go in `api/openapi.yaml`. The peer protocol gets its own `api/federation-openapi.yaml`.                                                                                                |

---

## 3. Federation model

A **federation** is a named set of member instances. Each member is a complete Phase 1 instance. Membership adds three things:

1. **A trust fabric.** Each member has a federation certificate issued by the federation CA. Membership is a signed, versioned document.
2. **A small replicated control state**, made of three document kinds, each with a monotonically increasing generation and signed by the authority:
   - `membership`: members, endpoints, keys, labels, status, protocol versions.
   - `policy`: federated roles, grants, principal and group bindings, revocations.
   - `topology`: federated feeds, their home member, feed descriptors (templates), replication rules, routing preferences.
3. **Peer protocols** on a dedicated mTLS listener: document sync, status, search fan-out, proxied reads, replication transport.

### 3.1 The authority role

One member holds the **authority** role, identified by an `authority_epoch`. It is the only writer of federation documents. This applies the "authoritative writer, read-only replicas" rule from A09 to the control plane. Concretely:

- Control-plane writes (join, policy publish, topology changes) need the authority to be reachable. The data plane does not.
- If the authority is down, every member keeps enforcing the last documents it applied. Only federation administration pauses.
- Authority transfer is explicit: either a planned handover signed by the current authority, or a forced recovery signed by the offline **federation recovery key** (§5.1). Members accept the highest `authority_epoch` that has a valid signature chain. A previous authority that sees a higher epoch fences itself.

Rejected alternative: Raft or another quorum protocol across members. It would make control-plane availability depend on a majority of sites, add a consensus implementation to a single-process service (A01), and still give no data-plane benefit. Revisit only if multi-writer or automatic failover enters scope.

### 3.2 Consistency model

- Federation documents are **eventually consistent** with **monotonic application**. A member applies generation N+1 only on top of N (hash chain `prev_hash`). Gaps are filled by fetching the missing generations, or by fetching a full snapshot document at the latest generation. Documents are never applied out of order or rolled back.
- **Bounded staleness** is reported, not enforced. Each member exposes `applied_generation` and `age` per document kind. Alert thresholds default to 15 minutes (warning) and 24 hours (critical). The fail-closed behaviour is described in §13 and decision D4.
- Data-plane consistency stays exactly as in Phase 1: replicas lag their authority, are read-only, and serve independently.

---

## 4. Instance discovery and membership

### 4.1 Membership document

```json
{
  "kind": "membership",
  "federation_id": "uuid",
  "federation_name": "watteel",
  "generation": 42,
  "authority_epoch": 3,
  "prev_hash": "sha256:…",
  "protocol": { "min": 1, "max": 1 },
  "members": [
    {
      "instance_id": "uuid",
      "name": "oke",
      "status": "active|suspended|removed",
      "api_url": "https://ant.oke.example",
      "federation_url": "https://fed.ant.oke.example:8444",
      "cert_sha256": "…",
      "signing_key": "ed25519:…",
      "labels": { "site": "oke", "region": "eu-ams" },
      "software_version": "2.0.0",
      "joined_at": "…"
    }
  ],
  "revoked_cert_serials": ["…"],
  "issued_at": "…",
  "signature": { "key_id": "authority-epoch-3", "alg": "Ed25519", "value": "…" }
}
```

Canonicalization: RFC 8785 (JCS) JSON, Ed25519 signature over the canonical bytes with the `signature` member removed. The same envelope is used for `policy` and `topology`.

### 4.2 Trust bootstrap and join

1. **Found.** An instance administrator runs `POST /api/v1/federation` (or `ant federation init`). The instance:
   - Generates the federation CA key (ECDSA P-256), the authority signing key (Ed25519, epoch 1) and the **recovery key** (Ed25519).
   - Stores the CA and authority keys through `internal/secrets` envelope encryption (A07) with purposes `federation-ca`, `federation-authority:<epoch>`.
   - Shows the recovery private key **once**, as an encrypted export the operator keeps offline (D2). The instance stores only the public half.
   - Writes membership generation 1 containing itself.
2. **Invite.** On the authority, `POST /api/v1/federation/invitations` creates a one-time invitation. It is bound to the expected member name and optional labels, expires after 1 hour by default, and its verifier is hashed like service tokens. The response contains the invitation secret once, together with the **federation CA fingerprint** for out-of-band pinning.
3. **Join.** On the joining instance, an administrator calls `POST /api/v1/federation/join` with `{authority_federation_url, ca_sha256, invitation}`. The joiner:
   - Connects to the authority. TLS is pinned to `ca_sha256`, so the system trust store is not used.
   - Generates its member key pair and CSR (CN = instance UUID, SAN = federation_url host).
   - Presents the invitation.

   The authority then validates the invitation, the instance UUID uniqueness, the name uniqueness and the protocol version range. It issues a member certificate (30-day lifetime, auto-renewed at 2/3 of its lifetime over mTLS), publishes membership N+1, and returns the current membership, policy and topology snapshots.

4. **Activate.** The joiner applies the snapshots in one transaction, starts its federation workers, and becomes `active` after its first successful status report.

Both sides audit every step. A failed invitation use is a security event (A07).

### 4.3 Leave, suspend, remove

- **Leave** (member-initiated): the member sends a request signed with its member key. The authority publishes membership with `status=removed` and the certificate serial revoked.
- **Remove** (authority-initiated, destructive, needs confirmation): same outcome. It is used for a compromised or decommissioned member.
- **Suspend**: peers refuse the member's federation calls, but its documents stay. Used during incident handling.
- **Effects of leaving on the former member:**
  - Federation-origin roles and grants are deleted in one transaction. Local roles are untouched.
  - Federation-origin replication jobs are **converted to local jobs**. They keep their checkpoints and replica sources, but replication against the former peers stops because the certificate is revoked. Administrators can re-point the jobs with Phase 1 token peers.
  - Content is **never deleted**.
- **Effects on the other members:** stop routing and searching to the removed member, and mark any replication rules that targeted it as `degraded`.

### 4.4 Discovery

Members find each other through the membership document only (`federation_url`, `api_url`). There is no multicast or DNS-SD. Every member polls the authority for document heads every 30 seconds by default, and also accepts a signed "head changed" nudge from the authority. A member that cannot reach the authority can fetch documents from **any** active member. Documents are self-authenticating through the signature chain, so any member can relay them, which makes the authority's reachability less critical.

---

## 5. Transport and keys

### 5.1 Keys

| Key                                            | Holder                                 | Purpose                                                                        | Rotation                                                                        |
| ---------------------------------------------- | -------------------------------------- | ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------- |
| Recovery key (Ed25519)                         | Operator, offline                      | Signs `authority_epoch` changes (forced recovery) and recovery-key rotation    | Manual, rare                                                                    |
| Authority signing key (Ed25519)                | Authority instance, `internal/secrets` | Signs membership/policy/topology documents                                     | Per epoch. Planned rotation creates a new epoch signed by the old key.          |
| Federation CA (ECDSA P-256)                    | Authority instance, `internal/secrets` | Issues member TLS certificates                                                 | Rotating it needs a dual-trust window. Membership carries both CA fingerprints. |
| Member key (ECDSA P-256 TLS + Ed25519 signing) | Each member, `internal/secrets`        | mTLS client/server identity, signs assertions, blob tickets and leave requests | 30-day certificates, auto-renewed                                               |

The authority key for epoch N+1 is certified by a signature from the epoch N key (planned) or from the recovery key (forced). Members verify the chain back to the recovery public key that is pinned in membership generation 1.

### 5.2 Listener

Federation peer traffic uses a **dedicated listener** (`ANT_FEDERATION_LISTEN`, default `:8444`). It requires client certificates (TLS 1.3 only) chained to the federation CA, checks that the certificate serial is not revoked in the current membership, and checks that the certificate CN maps to an `active` member. The listener is not served through the HTTP ingress that terminates TLS, so the Helm chart gets a separate Service (LoadBalancer or NodePort) plus optional ingress-nginx `ssl-passthrough` (decision D1). The management API (`/api/v1`) and package protocols stay on the existing listener.

Each request on the federation listener also carries a member request signature (Ed25519 over method, path, body digest, timestamp and nonce). This gives defense in depth and a uniform audit identity. Nonces are cached for 5 minutes. Requests with more than 60 seconds of clock skew are rejected and reported on the status page.

---

## 6. Shared identity and policy

### 6.1 Federated principals

- A **federated principal** has the identity `(issuer, subject)` from a trusted IdP. The policy document lists the federation's trusted issuers (OIDC issuer URL, SAML entity ID, LDAP directory ID) as **identity domains**. Each member still configures its own IdP client (secrets stay local) but must use the same issuer identifiers for federation to apply.
- **Federated service accounts** are named principals defined in the policy document (`issuer = "federation:<federation_id>"`, `subject = <name>`). Members materialize them as `principals(kind='service')` rows. **Tokens stay instance-local**: each member issues tokens for the federated service account itself. Phase 2 has no federation-wide token. CI uses the token of its nearest member.
- Members materialize federated principals lazily: a row with `origin='federation'` is created at first authentication or first binding. Rows are never created for `emergency` principals, and local principals are never promoted.
- `principals.federated_id` stores the stable ID that the authority assigns, so audit records can be correlated across members.

### 6.2 Policy document contents

```json
{
  "kind": "policy",
  "generation": 17,
  "authority_epoch": 3,
  "prev_hash": "…",
  "identity_domains": [
    { "id": "zitadel", "type": "oidc", "issuer": "https://id.example" }
  ],
  "roles": [
    {
      "key": "fed:release-readers",
      "revision": 4,
      "grants": [
        {
          "kind": "feed",
          "effect": "allow",
          "feed_key": "watteel/oci-release",
          "actions": ["read"]
        },
        {
          "kind": "artifact",
          "effect": "deny",
          "feed_key": "watteel/oci-release",
          "namespace": "internal",
          "actions": ["read"]
        }
      ]
    }
  ],
  "principal_bindings": [
    {
      "domain": "zitadel",
      "subject": "2834…",
      "roles": ["fed:release-readers"]
    }
  ],
  "group_bindings": [
    { "domain": "zitadel", "group_id": "…", "roles": ["fed:release-readers"] }
  ],
  "service_accounts": [
    { "name": "ci-oke", "roles": ["fed:release-publishers"] }
  ],
  "revocations": [{ "domain": "zitadel", "subject": "…", "effective_at": "…" }],
  "member_overrides": { "<instance_id>": { "accept_admin_grants": false } }
}
```

Grants use the existing kinds and actions. They reference **feed keys** (`<federation>/<feed-slug>`, §9.1) instead of UUIDs. `source`-kind grants are not federable because sources are instance-local.

### 6.3 Materialization (apply)

The policy reconciler applies a document in **one transaction** that is fenced by the writer lease:

- Upserts roles with `origin='federation'`, `federation_key`, `federation_generation`. Role names are always prefixed `fed:`, and local role creation rejects that prefix, so names cannot collide.
- Resolves each `feed_key` through `federated_feed_bindings` to the local `feed_id`. Grants whose feed is not bound locally are stored as **inert** rows (`feed_id NULL`, `unresolved_feed_key` set, never matched by `Resolve`). They are reported in status and activate automatically when the feed gets bound. An unresolved key never widens into "all feeds", because the empty-feed wildcard is exactly the danger here.
- Rewrites `principal_roles` rows of origin federation for bound principals. Group bindings are evaluated at authentication time through the existing group extraction (LDAP groups, SAML `Groups`, and OIDC `groups` claim support, which is added as WP-2.4).
- Applies `revocations`: it disables federated principals (`disabled=true`, `origin` kept) and revokes the local tokens of revoked federated service accounts. Revocations are processed **before** role changes within the same document.
- Records `federation_documents.applied_at` and emits `federation.policy.applied` to the outbox and audit.

Rows with origin federation are **read-only through the management API**. Mutating them returns `409 federation_managed`. Enforcement uses an API-layer check plus a trigger that requires the transaction-local setting `ant.federation_apply = on`.

### 6.4 Conflict rules and local overrides

The decision semantics do not change (A06): any matching deny wins, otherwise the union of allows, otherwise deny. Federation only adds roles and grants, and the rules below follow from that:

1. **Local deny beats federated allow.** A member can add local roles with deny grants for federated principals, for example to block a feed locally. The federation cannot override a local deny.
2. **Federated deny beats local allow.** Policy authors can therefore enforce restrictions everywhere. A member that needs an exception must leave or ask the authority to change the policy. This is intentional because it makes federated denies dependable.
3. **Local allow adds to federated allow.** Members may grant federated principals extra local access through local roles.
4. **Federated `admin` on `instance` kind is inert** unless the member sets `accept_admin_grants=true` in its local settings (default false). The same applies to federated artifact or feed allows without a feed key, which would otherwise match every local feed. This mirrors "IdP mapping never implicitly creates an administrator". The authority cannot set it remotely: `member_overrides` in the document only records what members reported.
5. **Name or identity collisions.** If a local principal row already exists with the same `(issuer, subject)` and `origin='local'`, the reconciler does not take it over. The binding is reported as `conflict_local_principal` until an administrator runs the explicit adopt operation (`POST /api/v1/federation/principals/{id}/adopt`).
6. **Version conflicts are impossible by construction.** There is a single writer, and the generation is monotonic. A document with `generation <= applied` is ignored, and a document with a `prev_hash` mismatch triggers a snapshot fetch.

`GET /api/v1/permissions/effective` (exists) gains an `origin` per contributing grant and the applied policy generation, so explain output shows federated and local contributions separately.

### 6.5 Principal assertions (cross-member requests)

When member A needs to act for a caller on member B (search fan-out, proxied read, federated audit query), A sends an **assertion** on the mTLS channel:

```
X-Ant-Fed-Assertion: <base64url(JCS{iss: A.instance_id, fid: federated_id, dom, sub,
                       scopes:[…token scopes or null for session], aud: B.instance_id,
                       req: sha256(method|path|query|body-digest), iat, exp: iat+60s, jti})>.<Ed25519 sig by A>
```

B verifies the signature against A's key from membership, checks `aud`, `exp` and `jti` replay, and that A is active. It then maps `(dom, sub)` to its local federated principal and evaluates **B's own policy**, intersected with `scopes`. B never trusts A's authorization decision. Only federated principals can be asserted. A request for a local-only principal is refused at A with `403 not_federated`. Anonymous reads are asserted as `anonymous` and only succeed where B allows anonymous read.

---

## 7. Cross-instance search

**Decision: query fan-out, not index gossip.**

| Criterion     | Fan-out (chosen)                                                        | Index gossip                                                     |
| ------------- | ----------------------------------------------------------------------- | ---------------------------------------------------------------- |
| Authorization | Evaluated at the owning member with its live policy                     | Requires copying ACL state, risks stale or over-broad visibility |
| Data exposure | Package names never leave the member except in authorized results       | Every member stores every member's catalog                       |
| Freshness     | Live                                                                    | Lagged                                                           |
| Cost          | Latency bounded by a per-member deadline. Partial results are possible. | Storage, change feed per member, compaction                      |
| Fit with code | Reuses `GET /api/v1/feeds/{id}/packages` query logic                    | New indexer                                                      |

Design:

- `GET /api/v1/federation/search?q=&format=&namespace=&digest=&members=&cursor=` on any member. The member queries locally and in parallel sends `POST /federation/v1/search` with an assertion to every active member whose topology lists a feed matching the filters. Concurrency is capped at 8 by default. The per-member deadline is 1.5 s and the overall deadline is 2 s.
- Results are merged by `(feed_key, package, version)`. Each hit carries `member`, `feed_key`, `home` (whether this member is the feed home or a replica), `replica_lag_seconds` and `api_url`. Duplicates from replicas collapse under the home entry with a `copies[]` list.
- **Partial results are explicit**: `members: [{id, status: ok|timeout|error|skipped, took_ms}]`. There is no silent truncation.
- Pagination uses a per-member composite cursor (opaque, signed by the originating member so it cannot be tampered with). The page size limits from A05 apply.
- `lookup_checksum` (digest search) uses the same fan-out. It is the main input for placement (§8).
- Abuse controls: fan-out requires the `read` action somewhere. Each member enforces a per-peer rate limit of 50 req/s by default and a per-asserted-principal limit. Assertions are bound to request hashes, so a captured assertion cannot be reused for another query.
- Caching: responses are cached for 30 seconds keyed by `(federated_id, scopes hash, query, policy generation)`, and the cache is dropped when policy is applied.

---

## 8. Placement-aware routing

### 8.1 What gets routed

Only **reads of federated feeds that the entry member does not hold**, or holds with unacceptable lag. Metadata is composed by a member that holds the feed (A04 owner semantics, generation-bound). The entry member never composes metadata from another member's partial view.

Placement inputs:

- `topology`: feed home and replica members per feed key.
- Member status reports: health, replica job phase, lag, last success, per feed key.
- Locally measured RTT to each member: an EWMA over status pings every 15 seconds, plus passive measurements from proxied requests.
- Routing preferences in topology: an ordered list of label preferences (`prefer: [{site: same}, {region: same}]`), maximum acceptable lag per feed, and per-feed `routing: off|proxy|redirect`.

Candidate selection: active members holding the feed, filtered by lag ≤ max, then by health `ok`, then by label preference, then by lowest RTT. The home member is the last resort if no replica qualifies. The decision is deterministic for identical inputs and is shown in explain output (`GET /api/v1/federation/routes/explain?feed_key=&path=`).

### 8.2 Mechanisms

1. **Proxy (default, D5).** Entry member X authenticates the client locally, asserts the principal to holder Y (§6.5), and streams Y's response through. This works with every package client because the client sees one origin. It uses A03 buffer budgeting (the proxy is charged against the global transfer budget) and supports Range pass-through and cancellation propagation. URLs in metadata are rewritten to X's feed endpoint by the adapter's existing URL-generation path (the adapter on Y renders with `X-Ant-Fed-Public-Base`, which is validated against X's `api_url` in membership). Adapters that cannot rebase URLs declare `federation_proxy: unsupported` in their capability descriptor and are not proxied.
2. **Redirect.** X returns `307` to Y's public feed endpoint. This only works when the client can authenticate to Y on its own (same IdP session or anonymous feeds). It is opt-in per feed and **never used for publish**.
3. **Blob tickets** (an optimization inside proxy mode). For immutable blob GETs (OCI blobs, tarballs), X authorizes and resolves the asset, then redirects the client to Y with a capability URL `…/federation/blob/{digest}?t=<ticket>`. The ticket is signed by X and contains digest, size, audience Y, expiry 5 minutes and client IP binding (optional). Y serves the bytes if the digest belongs to an asset of a feed whose key is in the ticket and X is an active member. The ticket is opt-in per feed. Y needs a publicly reachable blob endpoint on its normal listener.

**Writes:** a publish to a feed whose home is another member returns a protocol-appropriate error that names the home endpoint (`409`/`405` with `Ant-Feed-Home: <url>`). Publish forwarding is out of scope for Phase 2 (§19 D6 explains why).

### 8.3 Signals and health

`GET /federation/v1/status` (mTLS) returns the member's health (existing `/api/v1/health` composition), software and protocol version, applied document generations, per-feed-key replica phase, lag and last success, the blob store state and load hints (active transfers, buffer budget utilization). Each member caches peer status for 15 seconds. A member with 3 failed pings in a row is marked `unreachable` and removed from candidates until it recovers. Circuit-breaker behaviour follows A08.

---

## 9. Coordinated replication

### 9.1 Federated feeds

A **federated feed** is a local feed on its home member that is published into topology under a **feed key** `<federation_name>/<slug>`, which is unique within the federation. `federated_feed_bindings(feed_id, feed_key, role home|replica|routed)` binds local feeds to keys on every member.

### 9.2 Feed descriptors (templates)

For each feed key, topology carries a reviewed, non-secret descriptor: format, display name, anonymous-read default, publish-policy shape (rules without principal UUIDs), retention class and adapter options that are marked federable in the capability descriptor. A member that receives a replication rule for a feed it does not have can create the destination feed from the descriptor. Local settings always win. The descriptor seeds creation only and never overwrites (spec §6.7 "without overwriting destination-specific settings"). Secrets, upstream URLs and credentials are never in descriptors.

### 9.3 Replication rules

```json
{"id": "uuid", "feed_key": "watteel/oci-release", "targets": {"labels": {"site": "kw"}} | {"members": ["uuid"]},
 "mode": "continuous", "interval_seconds": 300, "include_cached": true,
 "filters": {"namespace": "", "package": "", "version_prefix": ""},
 "deletion_policy": "ignore|propagate", "create_destination_feed": true, "max_lag_seconds": 900}
```

On every matching target member, the **replication reconciler**:

- Ensures a `replication_peers` row for the home member with `auth_mode='federation'` and `member_instance_id`. There is no token secret: the transport is mTLS and the request is authorized as an assertion of the target member's **replication service principal** `federation:<fid>/member:<instance_id>`. Policy grants that principal `replicate` on the feed key automatically for each rule (a generated role `fed:replication`).
- Ensures a `replication_jobs` row with `origin='federation'` and `federation_rule_id`, and patches mode, interval, filters and deletion policy when the rule changes (existing PATCH semantics; filter changes affect future changes only, as today).
- Never touches jobs with `origin='local'`.
- On rule removal it disables the job and keeps content and the replica source. Deletion of replicated content stays an explicit local action, because removing a rule must not destroy a site's offline copy.

Export endpoints (`/api/v1/replication/export/...`) are also mounted on the federation listener with assertion auth, so replication traffic between members does not depend on per-peer bearer tokens. The existing token mode stays for Phase 1 peers and non-members.

**Adoption.** `POST /api/v1/federation/replication/adopt` takes an existing local job that already matches a rule (same remote feed, same destination feed) and switches it to `origin='federation'` while keeping its checkpoint and replica source. Existing Phase 1 replication between sites can therefore join the federation without a reseed.

### 9.4 Authority and conflict semantics

- **One home per feed key**, recorded in topology and enforced by the authority (a second claim is rejected).
- Replication always flows home → replicas. A member never exports a replica source (existing rule: replica-kind sources are never recorded).
- Phase 1 conflict behaviour is unchanged: the authority wins over its own earlier replicas, and higher-precedence local origins and destination-only content are never overwritten (counted in `conflicts`).
- Federation adds **topology validation**:
  - A rule targeting the home member is rejected.
  - Replica chains (A→B→C) are not supported because they would require exporting replica content. A rule always pulls from the home.
  - A feed key cannot be home on two members across concurrent epochs (§13).
- **Multi-writer is out of scope.** Topology has no field for a second writer.

### 9.5 Planned home transfer

Planned home transfer is for site decommissioning or moving write traffic. It runs as an operator-driven sequence with an explicit state machine in topology (`transfer: {from, to, state}`):

1. `freeze`: the old home switches the feed to read-only. Publish returns `503 feed_transferring`. The current change head is recorded.
2. `drain`: the target's job reaches `checkpoint == head` with zero pending blobs. The deadline is configurable and the transfer aborts on timeout.
3. `promote`: on the target, the replica source is converted to a local source in one transaction (new A02 operation: provenance kept, `kind` changed, a new replication change-feed baseline started at the current state). Topology publishes the new home.
4. `demote`: the old home converts its local source into a replica source of the new home, attaching a federation job that starts in `incremental` from the new home's baseline.

Every step is idempotent and resumable. An abort before `promote` unfreezes the old home. There is **no automatic failover**: if the home is lost, the operator runs a forced transfer that skips `drain` and records possible data loss explicitly. Scope is decision D6.

---

## 10. Federation health, audit and administration

### Health

- `GET /api/v1/federation/status` shows the local view: role, epoch, applied generations and their ages, members with status, RTT, last seen, version and skew, rule compliance per feed key (targets in sync / lagging / failed / missing), and unresolved policy references.
- New Prometheus metrics: `ant_federation_document_generation{kind}`, `ant_federation_document_age_seconds{kind}`, `ant_federation_member_up{member}`, `ant_federation_member_rtt_seconds`, `ant_federation_rule_lag_seconds{feed_key,member}`, `ant_federation_assertions_total{peer,outcome}`, `ant_federation_proxy_bytes_total`, `ant_federation_search_partial_total`.
- Health composition: federation problems mark the instance `degraded`, never `unready`, because serving does not depend on federation. Readiness probes therefore never fail on federation state.

### Audit

- Local audit events, written in the same transaction as their effect (A07): `federation.created|joined|left|member.added|member.removed|member.suspended`, `federation.document.received|applied|rejected` (reason), `federation.policy.applied`, `federation.assertion.accepted|rejected` (sampled or aggregated for accepted reads, always recorded for rejections and mutations), `federation.route.proxied` (aggregated in `traffic_buckets`), `federation.transfer.*`, `federation.authority.epoch_changed`.
- The authority also keeps an append-only **document log** (`federation_documents`). Every member keeps the same log, so the chain can be audited anywhere.
- **Federated audit query**: `GET /api/v1/federation/audit?…` fans out like search. It requires the audit read grant on each member, which is evaluated by that member.
- Audit sinks (`audit_sinks`, existing) forward federation events like any other events.

### Administration (UI / API / MCP)

- **Console**: a new **Federation** area with:
  - Overview: topology map/list, member health, generations.
  - Members: invite, join, suspend, remove, certificate status.
  - Policy: roles, bindings, diff against the applied generation, publish with confirmation, generation history.
  - Feeds and rules: feed keys, homes, descriptors, rules, per-target compliance, adopt.
  - Routing: preferences and explain.
  - Search: global search with per-member status.
  - Authority: handover, recovery-key upload flow.

  Pages that write are enabled only on the authority. Other members show read-only pages with a link to the authority.

- **API**: §12.
- **MCP tools** (thin wrappers, same authorization, audited):
  - Read-only: `get_federation_status`, `list_federation_members`, `federated_search`, `federated_lookup_checksum`, `get_federation_policy`, `list_replication_rules`, `explain_route`.
  - Admin: `create_federation_invitation`, `publish_federation_policy` (destructive hint, `confirm` required), `set_replication_rule`, and the destructive `remove_federation_member`, `suspend_federation_member`, `start_feed_transfer`.

  Resources: `ant://federation`, `ant://federation/members`, `ant://federation/policy`.

**Permissions.** A new resource kind is not required. Federation administration uses `admin` on `instance` (local admin of the authority). Federation read uses `read` on `instance`. A new action is avoided to keep the six-action model (A06), and instance admin is already distinct.

---

## 11. Data model and migrations

All migrations are additive and forward-only. They do nothing until federation is initialized. The latest migration today is `022_operations.sql`.

| Migration                             | Contents                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `023_federation_core.sql`             | `federation_state` (singleton per instance: `federation_id`, `name`, `role member\|authority`, `authority_epoch`, `recovery_public_key`, `state none\|joining\|active\|left`, `revision`). `federation_members` (`member_instance_id`, `name`, `api_url`, `federation_url`, `cert_sha256`, `signing_key`, `labels jsonb`, `status`, `software_version`, `membership_generation`, `last_seen_at`, `rtt_ms`, `skew_ms`, `health jsonb`). `federation_documents` (`kind membership\|policy\|topology\|epoch`, `generation`, `authority_epoch`, `hash`, `prev_hash`, `body jsonb`, `signature`, `received_at`, `applied_at`, `rejected_reason`; `UNIQUE(kind,generation)`; append-only trigger). `federation_invitations` (`verifier`, `name`, `labels`, `expires_at`, `consumed_at`, `consumed_by`). `federation_assertion_nonces` (`jti`, `expires_at`). Secret purposes `federation-ca`, `federation-authority:<epoch>`, `federation-member-key` via `encrypted_secrets`. |
| `024_federated_identity.sql`          | `principals` add `origin local\|federation DEFAULT 'local'`, `federated_id uuid`, `UNIQUE(instance_id,federated_id)`. `roles` add `origin`, `federation_key`, `federation_generation` and a CHECK that `fed:` names imply origin federation. `principal_roles` add `origin`. `role_grants` add `unresolved_feed_key text` and allow `feed_id NULL` with it set (inert). `federation_group_bindings` (`domain`, `group_id`, `role_id`). Trigger guarding origin-federation rows unless `current_setting('ant.federation_apply', true)='on'`. `instance_settings` key `federation.accept_admin_grants`.                                                                                                                                                                                                                                                                                                                                                                    |
| `025_federated_feeds_replication.sql` | `federated_feed_bindings` (`feed_id`, `feed_key UNIQUE per instance`, `role`, `descriptor_revision`). `replication_peers` add `auth_mode token\|federation DEFAULT 'token'`, `member_instance_id uuid`. `token_secret_id` becomes nullable with `CHECK((auth_mode='token') = (token_secret_id IS NOT NULL))`. `replication_jobs` add `origin`, `federation_rule_id uuid`. `feed_transfers` (state machine rows).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `026_federation_routing.sql`          | `federation_route_stats` (bucketed per member and feed key: proxied requests, bytes, errors). `federation_blob_ticket_replay` (only if single-use tickets are chosen).                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |

The `instances` table is unchanged. One process still serves exactly one instance (A01).

Migration safety rules:

- No backfill of existing rows other than defaults.
- Rollback to a Phase 1 binary is supported **only while `federation_state.state='none'`**. The startup schema-compatibility check (A02) refuses to downgrade when federation data exists.
- `scripts/check-generated.sh` covers the new OpenAPI.

Go packages:

- New: `internal/federation` (state, documents, signing, JCS, membership, reconcilers), `internal/federation/peer` (mTLS listener, request signing, assertions, peer client), `internal/federation/search`, `internal/federation/routing`, `internal/management/federation*.go`, `cmd/ant/federation.go` (wiring plus `ant federation` CLI for init, recovery and re-identify).
- Touched: `internal/identity` (origin awareness, assertion principal), `internal/replication` (federation auth mode, origin), adapters (capability flag, public-base rebasing), `internal/mcp/tools.go`, `web/src/Federation*.tsx`, `deploy/chart`.

---

## 12. API surface (OpenAPI additions)

### 12.1 Management API (`api/openapi.yaml`, `/api/v1`, existing conventions: cursors, ETag/If-Match, idempotency keys, stable errors)

| Method + path                                                                                 | Auth                                    | Purpose                                                                  |
| --------------------------------------------------------------------------------------------- | --------------------------------------- | ------------------------------------------------------------------------ |
| `POST /federation`                                                                            | instance admin                          | Initialize federation as founder. Returns the recovery-key export once.  |
| `GET /federation/status`                                                                      | instance read                           | Local view (§10)                                                         |
| `POST /federation/join`                                                                       | instance admin                          | Join with invitation + pinned CA                                         |
| `POST /federation/leave`                                                                      | instance admin, confirm                 | Leave                                                                    |
| `GET /federation/members` · `GET /federation/members/{id}`                                    | instance read                           | Membership view                                                          |
| `POST /federation/invitations` · `GET` · `DELETE /{id}`                                       | authority admin                         | Invitations (secret returned once, A05 one-time retrieval)               |
| `POST /federation/members/{id}/suspend` · `/resume` · `DELETE /federation/members/{id}`       | authority admin, confirm                | Lifecycle                                                                |
| `GET /federation/documents/{kind}` · `GET /federation/documents/{kind}/{generation}`          | instance read                           | Applied document and history                                             |
| `GET /federation/policy` · `PUT /federation/policy/draft` · `POST /federation/policy/publish` | read / authority admin                  | Draft, diff, and publish with expected base generation (409 on mismatch) |
| `GET /federation/policy/diff?from=&to=`                                                       | instance read                           | Change review                                                            |
| `GET/POST /federation/feeds` · `GET/PATCH/DELETE /federation/feeds/{key}`                     | read / authority admin                  | Feed keys, home, descriptor                                              |
| `POST /federation/feeds/{key}/bind`                                                           | local instance admin                    | Bind a local feed to a key (replica or routed)                           |
| `GET/POST /federation/replication/rules` · `GET/PATCH/DELETE /{id}` · `GET /{id}/compliance`  | read / authority admin                  | Coordinated replication                                                  |
| `POST /federation/replication/adopt`                                                          | local admin + feed configure            | Adopt an existing job                                                    |
| `POST /federation/feeds/{key}/transfer` · `GET` · `POST …/abort`                              | authority admin, confirm                | Home transfer                                                            |
| `GET /federation/search` · `GET /federation/checksums/{digest}`                               | caller's read                           | Fan-out search                                                           |
| `GET /federation/audit`                                                                       | audit read on each member               | Fan-out audit query                                                      |
| `GET /federation/routes/explain`                                                              | feed read                               | Routing decision explanation                                             |
| `POST /federation/principals/{id}/adopt`                                                      | instance admin                          | Resolve `conflict_local_principal`                                       |
| `POST /federation/authority/handover`                                                         | authority admin, confirm                | Planned epoch change                                                     |
| `POST /federation/authority/recover`                                                          | instance admin + recovery-key signature | Forced epoch change                                                      |

New stable error codes: `federation_not_configured`, `federation_managed`, `not_authority`, `not_federated`, `stale_generation`, `feed_home_elsewhere`, `feed_transferring`, `member_unreachable`, `invitation_invalid`.

### 12.2 Peer protocol (`api/federation-openapi.yaml`, mTLS listener, `/federation/v1`, versioned separately; `protocol.min/max` in membership)

`GET /heads` (document generations), `GET /documents/{kind}?after=` (chain segment or snapshot), `POST /nudge`, `POST /csr/renew`, `POST /leave`, `GET /status`, `POST /search`, `POST /checksums`, `POST /audit/query`, `ANY /proxy/feeds/{key}/{path...}` (proxied reads), `/replication/export/feeds/{feed}/{head|manifest|changes|blobs/{digest}}` (existing handlers with assertion auth), and on the authority only: `POST /join`.

Blob-ticket endpoint on the public listener: `GET /federation/blob/{digest}?t=`.

---

## 13. Failure modes

| Failure                                  | Behaviour                                                                                                                                                                                                                                                                                                                                                                                                                                         | Detection / recovery                                                                                                                                                                                                                                        |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Network partition** (member ↔ rest)    | Member serves and publishes to its home feeds normally. Replicas of remote feeds serve last-synced content (Phase 1 guarantee). Search and routing exclude unreachable members and report partial results. Documents are frozen at the last applied generation.                                                                                                                                                                                   | `member_up=0`, document age alerts. On heal, it fetches the chain segment, applies in order and catches up replication from checkpoints.                                                                                                                    |
| **Authority down**                       | Data plane unaffected. Control-plane mutations return `503 not_authority_reachable` on the authority's own API (or are unavailable). Members relay documents among themselves.                                                                                                                                                                                                                                                                    | Restore authority or run a planned/forced epoch change (§5.1).                                                                                                                                                                                              |
| **Split brain (two authorities)**        | Happens only through a forced recovery while the old authority is alive and partitioned. Members accept the highest valid epoch. Documents from a lower epoch with a higher generation are **rejected** (`stale_epoch`). When the old authority sees a higher epoch it fences: read-only federation administration and an alert.                                                                                                                  | Documents carry `authority_epoch`, and every epoch is certified by the recovery key or the previous epoch key. Operator runbook: reconcile any policy edits made on the fenced authority by republishing them on the new one. Nothing merges automatically. |
| **Split brain (two homes for a feed)**   | Prevented. Home changes only through the transfer state machine published by the single authority. In the forced case, the old home learns about the transfer at the next document apply and **demotes itself to read-only** for that feed before accepting writes. Writes accepted during the partition stay on the old home as local content in a quarantined local source, reported for manual reconciliation. They are never silently merged. | `feed_home_conflict` alert, quarantine report                                                                                                                                                                                                               |
| **Stale policy**                         | Last-known-good keeps being enforced. Nothing fails open. Revocations are delayed by staleness. Optional hard limit `federation.policy_max_age` (default off, D4): when exceeded, federated principals are denied while local principals are unaffected.                                                                                                                                                                                          | Age metrics, status banner, alert at 15 min and 24 h                                                                                                                                                                                                        |
| **Corrupt, forged or replayed document** | Signature or chain check fails → rejected and audited. The previous generation stays in force. A lower generation is ignored.                                                                                                                                                                                                                                                                                                                     | `federation.document.rejected`                                                                                                                                                                                                                              |
| **Member key compromise**                | The authority removes or suspends the member and revokes its certificate serial. Peers enforce revocation on the next membership apply (seconds when reachable). Assertions are short-lived (60 s).                                                                                                                                                                                                                                               | Runbook + forced certificate rotation                                                                                                                                                                                                                       |
| **Authority key compromise**             | Forced epoch change using the recovery key. The compromised epoch's later documents are rejected. CA rotation follows.                                                                                                                                                                                                                                                                                                                            | Runbook                                                                                                                                                                                                                                                     |
| **Recovery key loss**                    | Planned handovers still work (signed by the current epoch). Forced recovery becomes impossible until a new recovery key is certified by the current authority plus a second admin confirmation.                                                                                                                                                                                                                                                   | Status warns if the recovery key has never been verified (periodic "prove possession" drill)                                                                                                                                                                |
| **Clock skew**                           | Assertions and request signatures are rejected beyond 60 s. Member status shows skew.                                                                                                                                                                                                                                                                                                                                                             | NTP required (documented)                                                                                                                                                                                                                                   |
| **Cloned instance (restored backup)**    | Join rejects a duplicate `instance_id`. A clone that was already a member is refused by peers because of the certificate and key mismatch. `ant federation re-identify` gives a restored copy a new instance ID before joining.                                                                                                                                                                                                                   | Duplicate-ID audit event                                                                                                                                                                                                                                    |
| **Version skew**                         | Members advertise their protocol range. The authority will not publish documents that use features above the minimum active member's `protocol.max`. A member below `protocol.min` is suspended with a clear reason.                                                                                                                                                                                                                              | Status                                                                                                                                                                                                                                                      |
| **Fan-out overload**                     | Per-peer and per-principal rate limits, deadlines and caps. Proxy transfers are charged to the A03 buffer budget and queue beyond it.                                                                                                                                                                                                                                                                                                             | Metrics                                                                                                                                                                                                                                                     |

---

## 14. Security threat model

Trust boundaries: (1) client ↔ entry member (Phase 1 auth unchanged); (2) member ↔ member on the federation listener; (3) authority ↔ members (documents); (4) operator ↔ recovery key; (5) federated IdPs.

| Threat (STRIDE)                             | Vector                                                  | Mitigation                                                                                                                                                                                                                                                                                                                                                 |
| ------------------------------------------- | ------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| S: Rogue instance joins                     | Stolen or guessed invitation                            | One-time, hashed, 1 h TTL, name-bound invitation. CA pinning by the joiner. Join is audited and appears in membership, which admins review. Invitations can be revoked.                                                                                                                                                                                    |
| S: MITM during join                         | Attacker presents a fake authority                      | Joiner pins the CA SHA-256 received out of band and never uses system roots on the federation channel                                                                                                                                                                                                                                                      |
| S: Impersonating a principal across members | Forged or replayed assertion                            | Ed25519 signature by an active member, `aud`, 60 s `exp`, `jti` replay cache, request-hash binding, mTLS identity must equal `iss`                                                                                                                                                                                                                         |
| E: Confused deputy                          | Member A asserts a principal it did not authenticate    | B re-authorizes with its own policy and A's token scopes. Assertions are recorded in both audits. A compromised member can act as any federated principal **only within that principal's federated grants on other members**. This residual risk is documented, limited by suspension and revocation, and can be reduced per member with local deny roles. |
| E: Federation grants itself admin           | Malicious or mistaken policy                            | `accept_admin_grants=false` by default, local only. Federated grants cannot target sources or local roles. Federated grants never create emergency principals.                                                                                                                                                                                             |
| E: Unresolved feed key widens access        | Missing binding treated as wildcard                     | Inert grant rows, `feed_id NULL` never matches. Unit and property tests.                                                                                                                                                                                                                                                                                   |
| T: Policy rollback or freeze                | Replay of older signed document, or withholding updates | Monotonic generation, hash chain and epoch checks. Freeze attacks (withholding) are visible as document age alerts. Optional hard staleness limit (D4).                                                                                                                                                                                                    |
| T: Topology manipulation to exfiltrate      | Rule targets an attacker-controlled member              | Only active members can be targets. Rule changes are authority-admin only, audited, and require confirmation in the UI and MCP.                                                                                                                                                                                                                            |
| I: Search leaks names                       | Fan-out returns unauthorized hits                       | Authorization at the owning member, assertion scoping, no catalog gossip, cache keyed by principal and policy generation                                                                                                                                                                                                                                   |
| I: Blob ticket leakage                      | Shared capability URL                                   | 5 min expiry, digest-scoped, audience-bound, opt-in, optional IP binding. Bytes behind the ticket are content the issuing member already authorized.                                                                                                                                                                                                       |
| I: Secret disclosure via descriptors        | Template contains credentials                           | Descriptor schema allowlist of federable fields declared by adapters. Secrets are excluded by type. Redaction test.                                                                                                                                                                                                                                        |
| D: Fan-out amplification                    | One request → N member queries                          | Concurrency caps, per-peer and per-principal rate limits, deadlines                                                                                                                                                                                                                                                                                        |
| D: Authority as a DoS target                | Control-plane outage                                    | Data plane independent. Documents can be relayed by any member.                                                                                                                                                                                                                                                                                            |
| S/T: SSRF via member URLs                   | Malicious `api_url` or `federation_url`                 | URLs come only from signed membership. HTTPS required. Redirects are not followed (as in replication). Private address allowlisting is explicit per deployment (lab LANs).                                                                                                                                                                                 |
| R: Repudiation                              | Admin denies a policy change                            | Signed documents carry author principal and approval metadata. Append-only document log on every member. Audit sinks.                                                                                                                                                                                                                                      |
| Key custody                                 | Root key theft                                          | Keys sit in `internal/secrets` envelope encryption (A07), and the recovery key is offline. The authority signing key is never exported except through the planned handover protocol (signed new-epoch key certification, not key transfer).                                                                                                                |

A security review (A10 style) of `internal/federation/peer` and the assertion and ticket code is an exit criterion for P2-M2 and P2-M5.

---

## 15. Rollout and compatibility with Phase 1

- **Off by default.** Federation code is inert until `POST /federation` or `join`. The federation listener only binds when `ANT_FEDERATION_LISTEN` is set or federation is active. A Phase 1 deployment that upgrades to the Phase 2 binary sees only the additive migrations.
- **Phase 1 interop.** A federated member can still replicate with non-member Phase 1 instances in token mode, in both directions. Non-members are invisible to search and routing.
- **Adoption path for existing estates** (OKE and kw today):
  1. Upgrade both instances.
  2. Found the federation on the intended authority (OKE).
  3. Join kw.
  4. Bind existing feeds to feed keys.
  5. Adopt existing replication jobs (no reseed).
  6. Move group mappings from LDAP config files into federated group bindings gradually. Local mappings keep working in the meantime.
  7. Enable routing per feed.
- **Mixed versions.** Protocol negotiation through membership `protocol.min/max`. Rolling upgrades proceed one member at a time. The authority avoids publishing features that lagging members cannot apply.
- **Downgrade.** Possible only before federation init (§11). After that, leaving the federation is the supported path, and a binary downgrade after leaving is still blocked by the schema check unless an explicit `ant federation purge` removes federation rows. The purge must be run deliberately and is documented.
- **Helm chart.** New values: `federation.enabled`, `federation.listen`, `federation.service` (type, port, annotations), `federation.ingressPassthrough`. The example values for `azrty-oke.yaml`, `kw.yaml` and `oke-minio.yaml` get commented federation sections.
- **Single-process constraint (A01) holds.** Federation workers run under the instance writer lease like replication workers.

---

## 16. Test strategy

| Layer                             | What                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | Where                                                                                                       |
| --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| Unit                              | JCS canonicalization (RFC 8785 vectors), signature and chain verification, epoch rules, invitation lifecycle, assertion verify/replay, ticket verify, policy materialization (including inert grants, conflict rules 1–6), routing candidate selection (table-driven, deterministic)                                                                                                                                                                                                                                                                                                                                                                                                                                                                            | `internal/federation/...`                                                                                   |
| Property                          | Applying any ordered prefix of documents equals applying a snapshot at that generation. Re-applying is idempotent. Out-of-order or lower generations never change state. Authorization never grants more than the union of local and federated policy, and a local deny always holds.                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | `internal/federation/policy_property_test.go`                                                               |
| DB integration                    | Migrations 023–026 up on a Phase 1 database with data. Origin-guard triggers. Downgrade refusal. Writer-lease fencing of reconcilers.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                           | `scripts/test-postgres.sh` (existing)                                                                       |
| **Multi-instance harness**        | Go harness that starts N in-process instances, each with its own PostgreSQL database (same server, separate DBs), disk blob dir, real TLS with a test CA, and both listeners. A controllable TCP fault proxy between every pair (drop, delay, blackhole, one-way) and an injectable clock per instance. Scenarios: found/join/leave/remove, document relay through a non-authority member, partition and heal, authority outage, forced epoch with a partitioned old authority (split brain), stale policy with and without the hard limit, revocation propagation time, rule → job creation, adoption without reseed, home transfer (planned and forced with quarantine), search partial results, proxy Range/cancel, ticket expiry, version-skew negotiation. | `internal/federation/harness`, `scripts/test-federation.sh`, CI job (opt-in like `ANT_REQUIRE_INTEGRATION`) |
| Client conformance via federation | Through a proxy-routed feed on a member that does not hold it: `docker pull`/`oras`, `npm install`, `go mod download`, `helm pull`, `cargo fetch`, `pip download`, i.e. the first-wave formats plus every adapter that declares `federation_proxy: supported`. Redirect and blob-ticket modes for OCI and npm.                                                                                                                                                                                                                                                                                                                                                                                                                                                  | Existing conformance runner, new federation profile                                                         |
| Security                          | Fuzzing of document and assertion parsers. Negative suites: wrong CA, revoked certificate, expired or replayed assertion, `aud` mismatch, forged chain, descriptor secret injection, SSRF member URL. Review sign-off.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | `internal/federation/peer`                                                                                  |
| **Real federation (OKE + kw)**    | OKE (`deploy/examples/azrty-oke.yaml`) as founder/authority and kw (`deploy/examples/kw.yaml`, TrueNAS MinIO) as member. Exercise: join over the dedicated federation Service, publish policy with Zitadel group bindings, adopt the existing replication job, federated search from both sides, proxy-routed `docker pull` at kw of a feed homed on OKE but not replicated to kw, a partition (NetworkPolicy deny on the federation port) with continued serving and catch-up afterwards, authority outage, a planned home transfer of a test feed, leave and rejoin. Evidence is recorded like Phase 1 deployment evidence (versions, results, known limitations).                                                                                            | `docs/implementation/federation-evidence.md` (created during P2-M6)                                         |

---

## 17. Milestones

Dependency graph: `P2-M1 → P2-M2 → {P2-M3, P2-M4} → P2-M5 → P2-M6`. Each milestone ships its own API, UI, MCP and docs increment, so P2-M6 consolidates rather than starting UI work.

### P2-M1: Federation foundation and membership

Outcome: instances can found, join, leave and be removed from a federation over mTLS, with signed and versioned membership visible in API and UI.
Exit: harness scenarios for found/join/leave/remove/relay/partition/version skew pass. Duplicate-ID and invalid-invitation paths are rejected and audited. Phase 1 behaviour is unchanged with federation off (full existing suite green).

### P2-M2: Shared identity and policy

Outcome: federated roles, principal, group and service-account bindings, and revocations are published once and enforced on every member, with local overrides. Principal assertions work.
Exit: property tests green. Conflict rules 1–6 covered. Revocation reaches all reachable members within 60 s in the harness. Stale-policy behaviour is verified. Security review of the assertion code is done. Explain output shows origins.

### P2-M3: Coordinated replication

Outcome: federation rules create and supervise replication jobs over mTLS, feed keys and descriptors exist, and existing jobs can be adopted without reseed. Planned home transfer works.
Exit: in the harness, a rule creates destination feeds and jobs on labelled targets, rule edits propagate, rule removal keeps content, adoption keeps the checkpoint, and home transfer works planned and forced with quarantine. The Phase 1 replication suite passes in both auth modes.

### P2-M4: Cross-instance search

Outcome: global package and checksum search with per-member authorization and explicit partial results, through UI, API and MCP.
Exit: authorization filtering is proven with differing per-member policies. Deadlines and partial results work under partition. Rate limits hold. Cursor tampering is rejected.

### P2-M5: Placement-aware routing

Outcome: reads of non-local federated feeds are proxied (default) or redirected to the best holder based on health, lag, labels and RTT. Blob tickets are optional.
Exit: client conformance passes through proxy for all adapters that declare support, plus redirect and ticket modes for OCI and npm. Range and cancel are correct. The buffer budget is respected. Explain output is deterministic. Security review of proxy and ticket code is done.

### P2-M6: Federation operations and qualification

Outcome: complete health, audit and administration (federated audit query, metrics, alerts, authority handover/recovery, runbooks), Helm support, and real-federation evidence on OKE + kw.
Exit: all §16 OKE + kw scenarios pass with recorded evidence. Runbooks exist for partition, split brain, key compromise, recovery-key drill, re-identify and purge. Upgrade from Phase 1 and the adoption path are rehearsed. The OpenAPI files are published, and the docs make no unsupported claims.

---

## 18. Work packages

Size: S ≈ up to 3 days, M ≈ up to 2 weeks, L ≈ more than 2 weeks for one engineer. These are relative estimates, not calendar commitments. "Parallel" lists the work packages that can run at the same time once dependencies are met.

### P2-M1: Foundation

| WP     | Scope                                                                                                                                                                                                | Packages / files                                                                                                     | Depends              | Size | Parallel |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------- | -------------------- | ---- | -------- |
| WP-1.1 | Signed document core: JCS canonicalization, Ed25519 envelope, hash chain, generation/epoch rules, chain verifier, snapshot format                                                                    | `internal/federation/document.go`, `jcs.go`, `chain.go` + tests                                                      | none                 | M    | 1.2, 1.3 |
| WP-1.2 | Schema `023_federation_core.sql`. Federation state store. Secret purposes. `ant federation re-identify` CLI                                                                                          | `internal/database/migrations/023_*.sql`, `internal/federation/store.go`, `cmd/ant/federation.go`                    | none                 | M    | 1.1, 1.3 |
| WP-1.3 | Federation listener: TLS 1.3 mTLS, CA and member certificates, revocation check against membership, member request signatures, nonce cache, skew checks. Peer HTTP client (no redirects, pinned CA). | `internal/federation/peer/{listener,client,signing}.go`, `cmd/ant/main.go` wiring                                    | none                 | M    | 1.1, 1.2 |
| WP-1.4 | Found / invite / join / leave / remove / suspend flows. Certificate issuance and renewal. Recovery-key generation and export.                                                                        | `internal/federation/membership.go`, `invite.go`, `ca.go`                                                            | 1.1, 1.2, 1.3        | L    | none     |
| WP-1.5 | Document sync worker: heads polling, nudges, relay from any member, gap fill, apply membership. Status endpoint and member pings (RTT, skew).                                                        | `internal/federation/sync.go`, `status.go`, `cmd/ant/workers`                                                        | 1.4                  | M    | 1.6      |
| WP-1.6 | Management API + OpenAPI for federation, members, invitations and documents. Peer OpenAPI file. Error codes. Generated clients.                                                                      | `internal/management/federation.go`, `api/openapi.yaml`, `api/federation-openapi.yaml`, `scripts/check-generated.sh` | 1.4                  | M    | 1.5, 1.7 |
| WP-1.7 | Multi-instance harness v1 (N instances, separate DBs, test CA, fault proxy, fake clock) + M1 scenarios                                                                                               | `internal/federation/harness`, `scripts/test-federation.sh`, CI                                                      | 1.3 (scaffold early) | L    | 1.4–1.6  |
| WP-1.8 | Console: Federation overview, members, join/invite. MCP read tools `get_federation_status`, `list_federation_members`. Helm listener/Service values.                                                 | `web/src/Federation*.tsx`, `internal/mcp/tools.go`, `deploy/chart`                                                   | 1.6                  | M    | 1.7      |

### P2-M2: Identity and policy

| WP     | Scope                                                                                                                                                                        | Packages / files                                                                                | Depends  | Size | Parallel |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- | -------- | ---- | -------- |
| WP-2.1 | Schema `024_federated_identity.sql`. Origin columns, inert grants, guard triggers, `federation_group_bindings`.                                                              | migrations, `internal/identity/roles.go` (ignore inert rows)                                    | M1       | M    | 2.2      |
| WP-2.2 | Policy document schema and validation. Authority draft/diff/publish with base-generation CAS.                                                                                | `internal/federation/policy.go`, `internal/management/federation_policy.go`                     | M1       | M    | 2.1      |
| WP-2.3 | Policy reconciler: transactional materialization, revocation-first ordering, conflict rules 1–6, `accept_admin_grants`, principal adopt, effective-permissions origin output | `internal/federation/reconcile_policy.go`, `internal/management/ops_permissions.go`             | 2.1, 2.2 | L    | 2.4      |
| WP-2.4 | Group binding evaluation at login: OIDC `groups` claim mapping (new), SAML Groups, LDAP immutable IDs. Lazy federated principal materialization.                             | `internal/oidclogin`, `internal/samllogin`, `internal/directory`, `internal/identity`           | 2.1      | M    | 2.3      |
| WP-2.5 | Principal assertions: issue at the entry member, verify at the peer, map to the local federated principal, scope intersection, replay cache, audit                           | `internal/federation/peer/assertion.go`, `internal/identity/middleware.go` (peer authenticator) | M1, 2.1  | M    | 2.3, 2.4 |
| WP-2.6 | Stale-policy handling: age metrics, alerts, optional hard limit (D4). Property tests. Harness scenarios (revocation latency, partition, conflict rules). Security review.    | `internal/federation`, harness                                                                  | 2.3, 2.5 | M    | 2.7      |
| WP-2.7 | Console policy editor/diff/history. MCP `get_federation_policy`, `publish_federation_policy` (confirm).                                                                      | `web/src`, `internal/mcp`                                                                       | 2.2      | M    | 2.6      |

### P2-M3: Coordinated replication

| WP     | Scope                                                                                                                                                               | Packages / files                                                                                                   | Depends                    | Size | Parallel |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ | -------------------------- | ---- | -------- |
| WP-3.1 | Schema `025_*`. Feed keys and bindings. Topology document (feeds, homes, descriptors, rules). Authority APIs.                                                       | migrations, `internal/federation/topology.go`, `internal/management/federation_feeds.go`                           | M2 (policy generated role) | M    | 3.2      |
| WP-3.2 | Federation peer auth mode in replication: export handlers on the federation listener with assertion auth, client using mTLS, `auth_mode` on peers                   | `internal/replication/{client,export_http,http,service}.go`                                                        | M1, 2.5                    | M    | 3.1      |
| WP-3.3 | Feed descriptors: adapter capability field `federable_options`, descriptor rendering, create-from-descriptor (never overwrite)                                      | `internal/artifact` capability descriptor, adapters (flag only), `internal/federation/descriptor.go`               | 3.1                        | M    | 3.4      |
| WP-3.4 | Replication reconciler: rule → peer/job materialization, rule edits, removal keeps content, compliance status, adopt endpoint                                       | `internal/federation/reconcile_replication.go`, `internal/management/federation_replication.go`                    | 3.1, 3.2                   | L    | 3.3      |
| WP-3.5 | Home transfer state machine: freeze/drain/promote/demote, replica→local source conversion, change-feed baseline, forced mode with quarantine                        | `internal/federation/transfer.go`, `internal/replication` (baseline), `internal/artifact` (source kind conversion) | 3.4                        | L    | 3.6      |
| WP-3.6 | Harness scenarios for rules, adoption, transfer. Console feeds/rules/compliance pages. MCP `list_replication_rules`, `set_replication_rule`, `start_feed_transfer`. | harness, `web/src`, `internal/mcp`                                                                                 | 3.4                        | M    | 3.5      |

### P2-M4: Search (can run in parallel with P2-M3 after P2-M2)

| WP     | Scope                                                                                                                                  | Packages / files                                                                             | Depends                                | Size | Parallel |
| ------ | -------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- | -------------------------------------- | ---- | -------- |
| WP-4.1 | Peer search and checksum endpoints reusing the package query logic, behind assertion auth                                              | `internal/federation/search/peer.go`, `internal/management/packages.go` (extract query core) | 2.5                                    | M    | 4.2      |
| WP-4.2 | Fan-out coordinator: target selection from topology, deadlines, concurrency, merge/dedup, signed composite cursors, cache, rate limits | `internal/federation/search/fanout.go`                                                       | 2.5, 3.1 (feed keys; stub until ready) | M    | 4.1      |
| WP-4.3 | Management API, console global search, MCP `federated_search`, `federated_lookup_checksum`. Harness authorization/partition scenarios. | `internal/management/federation_search.go`, `web/src`, `internal/mcp`                        | 4.1, 4.2                               | M    | none     |

### P2-M5: Routing

| WP     | Scope                                                                                                                         | Packages / files                                                       | Depends       | Size                          | Parallel |
| ------ | ----------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- | ------------- | ----------------------------- | -------- |
| WP-5.1 | Placement engine: status ingestion, RTT EWMA, candidate selection, routing preferences, explain endpoint                      | `internal/federation/routing/{placement,explain}.go`                   | M3            | M                             | 5.2      |
| WP-5.2 | Adapter capability `federation_proxy` + public-base rebasing hook. Audit every adapter's URL generation.                      | `internal/artifact`, each adapter's URL rendering (S per adapter, ~26) | M1            | L (wide, parallel by adapter) | 5.1      |
| WP-5.3 | Proxy path: feed-level dispatch for non-local feed keys, streaming with A03 budget, Range/cancel, assertion, metrics          | `internal/federation/routing/proxy.go`, `internal/server` routing hook | 5.1, 5.2, 2.5 | L                             | 5.4      |
| WP-5.4 | Redirect mode and blob tickets (issue, verify, public endpoint), publish-elsewhere errors with `Ant-Feed-Home`                | `internal/federation/routing/{redirect,ticket}.go`                     | 5.1           | M                             | 5.3      |
| WP-5.5 | Client conformance federation profile. Harness routing scenarios. Security review. Console routing page. MCP `explain_route`. | conformance runner, harness, `web/src`, `internal/mcp`                 | 5.3, 5.4      | M                             | none     |

### P2-M6: Operations and qualification

| WP     | Scope                                                                                                                    | Packages / files                                                       | Depends                 | Size | Parallel |
| ------ | ------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------- | ----------------------- | ---- | -------- |
| WP-6.1 | Authority handover and forced recovery (epoch chain, fencing of old authority), recovery-key drill, CA rotation          | `internal/federation/authority.go`, `cmd/ant/federation.go`            | M1 (can start after M2) | L    | 6.2, 6.3 |
| WP-6.2 | Federated audit query, federation audit event catalogue completion, metrics/alerts, health composition (`degraded` only) | `internal/federation`, `internal/observability`, `internal/management` | M4 (fan-out core)       | M    | 6.1, 6.3 |
| WP-6.3 | Runbooks (partition, split brain, key compromise, re-identify, purge, adoption) + `ant federation purge`                 | `docs/operations/federation-*.md`, `cmd/ant`                           | 6.1                     | M    | 6.2      |
| WP-6.4 | OKE + kw real federation qualification and evidence. Phase 1 → Phase 2 upgrade rehearsal.                                | `deploy/examples/*`, `docs/implementation/federation-evidence.md`      | M3, M5, 6.1             | M    | none     |

The critical path is WP-1.1/1.3 → 1.4 → 2.1 → 2.3/2.5 → 3.1/3.2 → 3.4 → 5.1 → 5.3 → 6.4. WP-1.7 (harness) and WP-5.2 (adapter rebasing audit) are the most parallelizable and should start as early as their dependencies allow.

---

## 19. Open decisions for the owner

| #   | Decision                          | Options                                                                                                                                                                                                                     | Recommended default                                                                                                                                                                                                                                          |
| --- | --------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| D1  | **Federation transport exposure** | (a) Dedicated mTLS listener on its own Service/port, with ingress-nginx `ssl-passthrough` where a hostname is needed. (b) Reuse the existing TLS-terminating ingress and rely only on application-layer request signatures. | **(a)**. True mTLS identity, no dependence on ingress client-certificate forwarding, and the request signatures stay as defense in depth. Cost: one extra Service per site (OKE LB, kw NodePort/MetalLB).                                                    |
| D2  | **Root of trust custody**         | (a) Offline recovery key held by the operator, with the authority key and CA on the authority instance in `internal/secrets`. (b) All keys on the authority instance only. (c) External KMS/HSM.                            | **(a)**. Allows forced recovery and split-brain resolution without trusting a possibly compromised instance. (c) can be added later through the A07 wrapping-key interface.                                                                                  |
| D3  | **Control-plane authority model** | (a) Single designated authority with planned handover and recovery-key forced transfer. (b) Quorum/Raft across members.                                                                                                     | **(a)**. Data plane never depends on it, it fits the single-process model (A01), and it extends the authoritative-writer principle already used by replication.                                                                                              |
| D4  | **Stale-policy behaviour**        | (a) Last-known-good indefinitely, with alerts at 15 min and 24 h. (b) Additionally deny federated principals after a hard maximum age.                                                                                      | **(a) by default, with (b) available as an opt-in setting** (suggested value 7 days when enabled). Availability first, consistent with "no hard dependency". Security-sensitive operators can opt in.                                                        |
| D5  | **Default routing mechanism**     | (a) Proxy through the entry member. (b) 307 redirect. (c) Proxy plus blob tickets by default.                                                                                                                               | **(a) proxy**, with redirect and blob tickets opt-in per feed. Works with every package client and identity setup. Tickets are an optimization once WAN cost matters.                                                                                        |
| D6  | **Write-side scope in Phase 2**   | (a) Single home per feed plus planned and forced home transfer, no publish forwarding, no multi-writer. (b) Add publish forwarding from any member to the home. (c) Multi-writer.                                           | **(a)**. Publish forwarding doubles large-upload transfer paths and complicates protocol-specific upload sessions (OCI chunked uploads), and multi-writer needs conflict semantics the spec has not defined. Revisit (b) after P2-M5 with workload evidence. |

---

## 20. Acceptance criteria (Phase 2)

1. With federation off, the Phase 1 suite and behaviour are unchanged. With federation on, no serving or publishing path blocks on another member (proven by the partition and authority-outage scenarios).
2. Members join and leave only through signed membership over mTLS. Invitation, CA pinning, revocation and duplicate-ID protections are verified.
3. A policy published once is enforced on all reachable members within 60 s. Local deny and federated deny semantics, inert unresolved grants and `accept_admin_grants` are verified. Stale policy never fails open.
4. Federated search returns only results the caller is authorized to see on each member, reports partial results, and respects deadlines and rate limits.
5. Proxied reads of non-local feeds pass client conformance for every adapter that declares support. Redirect and ticket modes pass for OCI and npm.
6. Federation replication rules create, update and supervise jobs. Adoption avoids reseed. Rule removal keeps content. Planned home transfer completes without data loss, and forced transfer quarantines divergent writes.
7. Federation status, audit (local and fan-out), metrics, UI, API and MCP cover every federation capability with equivalent authorization and audit events.
8. The OKE + kw real federation passes the §16 scenario list with recorded evidence, and the runbooks have been exercised.
