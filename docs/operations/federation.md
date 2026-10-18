# Federation (Phase 2: membership, shared identity and policy)

Federation groups independently operable Kuvryn Ant instances under one signed, versioned membership. Delivered: founding, invitations, join, leave, suspend, resume, remove, membership sync and status (P2-M1), and shared identity and policy with principal assertions (P2-M2). Search, routing and coordinated replication are later milestones; see [the design](../planning/phase-2-federation.md).

Serving never depends on federation. A federation listener that cannot bind, an unreachable authority, or a rejected document is logged, shown as `degraded` in federation status, and the instance keeps serving with the last applied membership. Readiness probes never fail because of federation.

## Configuration

| Variable                    | Purpose                                                                                                                                                                                            |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ANT_FEDERATION_LISTEN`     | Address of the dedicated mTLS listener, for example `0.0.0.0:8444`. It is a separate server and is never mounted on the public HTTP listener. Unset means no listener.                             |
| `ANT_FEDERATION_PUBLIC_URL` | `https://host:port` other members use to reach the listener. Required to found or join. The member certificate's SAN is this host.                                                                 |
| `ANT_FEDERATION_JOIN_DIR`   | Optional directory (a mounted Secret) with files `authority_url`, `ca_sha256` and `invitation`. An instance that was never federated joins automatically at startup and retries until it succeeds. |

Federation keys (CA, authority signing key, member keys) are stored encrypted in PostgreSQL under the root key (`internal/secrets`, purposes `federation-ca`, `federation-authority:<epoch>`, `federation-member-key`). Back up the root key as before; no new secret material is mounted.

Helm: set `federation.enabled`, `federation.publicURL` and `federation.service` (LoadBalancer, NodePort or ClusterIP). TLS must reach the pod unterminated: use the federation Service directly, or `federation.ingressPassthrough` with ingress-nginx `--enable-ssl-passthrough`. `federation.join.existingSecret` enables automatic join, and `federation.networkPolicyFrom` opens the port to the other members when `networkPolicy.enabled` is set. The examples in `deploy/examples/` contain commented federation sections.

## Procedures

All operations are available in the console (Federation page) and the management API (`/api/v1/federation…`, see `api/openapi.yaml`). Reads need `read` on the instance; changes need instance `admin`. MCP exposes `get_federation_status` and `list_federation_members`.

1. **Found** on the intended authority: `POST /api/v1/federation {"name":"watteel","member_name":"oke"}`. The response contains the **recovery key once**. Store it offline (password manager or sealed envelope). It is never stored on the instance and is required for forced authority recovery and for rotating the recovery key. Note the CA fingerprint (`ca_sha256`).
2. **Invite** on the authority: `POST /api/v1/federation/invitations {"name":"kw","labels":{"site":"kw"}}`. The secret is shown once and expires after one hour by default (`ttl_seconds`, at most 7 days). Hand the authority URL, CA fingerprint and secret to the joining administrator out of band.
3. **Join** on the new member: `POST /api/v1/federation/join {"authority_url":…,"ca_sha256":…,"invitation":…}`. The connection is pinned to the CA fingerprint; the system trust store is not used. The member verifies the whole signed membership chain before applying it.
4. **Leave** on a member: `POST /api/v1/federation/leave {"confirm":true}`. The authority removes the member and revokes its certificate. Content and local configuration are kept. `force: true` leaves locally if the authority is unreachable. The authority cannot leave (authority handover is P2-M6).
5. **Suspend / resume / remove** on the authority: `POST …/members/{id}/suspend|resume`, `DELETE …/members/{id}?confirm=true`. Suspended and removed members are refused on every listener once the new membership is applied; removal also revokes the certificate serial.
6. **Rotate the recovery key** on the authority: `POST /api/v1/federation/recovery-key/rotate {"recovery_key":"<current>"}`. The next membership generation carries a rotation statement signed by the current key; the new key is returned once.

Members poll the authority every 30 seconds and receive signed nudges after each change. If the authority is unreachable they fetch documents from any other active member; documents are self-authenticating, so relays cannot alter them.

## Status and alerts

`GET /api/v1/federation/status` reports state, role, epoch, applied generation and its age (time since membership was last confirmed current), member certificate expiry, last sync time, source and error, and member counts. `GET /api/v1/federation/members` adds per-member RTT, clock skew and last-seen time. Metrics: `ant_federation_active`, `ant_federation_document_generation{kind}`, `ant_federation_authority_epoch`, `ant_federation_members{status}`.

Alert when health is `degraded` for more than 15 minutes (critical after 24 hours). Audit events: `federation.created`, `federation.joined`, `federation.left`, `federation.member.added|left|removed|suspended|resumed`, `federation.invitation.created|revoked|rejected`, `federation.document.published|applied|rejected`, `federation.peer.rejected`, `federation.certificate.issued|renewed`, `federation.recovery_key.rotated`. Rejections are recorded with result `denied`.

Requirements: NTP on every member (signed requests are rejected beyond 60 seconds of clock skew), and a reachable federation port between members. Member certificates last 30 days and renew automatically after two thirds of their lifetime.

## Shared identity and policy (P2-M2)

The authority publishes a signed, versioned **policy document** on the same envelope and hash chain as membership: identity domains (trusted issuers), federated roles (keys start with `fed:`) with grants on feed keys (`<federation>/<feed-slug>`), principal bindings, group bindings, federated service accounts and revocations. Every member verifies and applies it locally; the evaluator is unchanged (deny wins, union of allows).

- **Publish** on the authority: `PUT /api/v1/federation/policy/draft {"policy":{…}}` (validated, diff against the applied generation), then `POST /api/v1/federation/policy/publish {"base_generation":N,"confirm":true}` (or include `policy` inline). A stale `base_generation` answers `409 stale_generation`. Members are nudged and otherwise pick it up on their next poll (relays from any member work). History and diffs: `GET /api/v1/federation/policy`, `GET /api/v1/federation/policy/diff?from=&to=` (`to=draft`), `GET /api/v1/federation/documents/policy/{generation}`.
- **Bind feed keys** on each member: `POST /api/v1/federation/feed-bindings {"feed_key":"lab/release","feed_id":…}`. Grants on a key that is not bound stay inert (never a wildcard) and are listed as unresolved in status; binding activates them, deleting the feed or unbinding makes them inert again.
- **Local overrides** (member admin, never the authority): local deny roles beat federated allows, local allows add to federated ones, and federated denies beat local allows. Federated instance-admin allows are inert until `PUT /api/v1/federation/settings {"accept_admin_grants":true}`. Federation-managed roles, grants, memberships and principals are read-only locally (`409 federation_managed`); local roles can still be assigned to federated principals. A binding that matches an existing local principal is reported as `conflict_local_principal` until `POST /api/v1/federation/principals/{id}/adopt {"confirm":true}`.
- **Principals.** Federated service accounts are materialized on every member (tokens stay local: issue them per member). Federated users are materialized when bound or at their first sign-in through a matching identity domain; group bindings are evaluated against the groups of the last sign-in (OIDC `groups` claim, SAML Groups, LDAP immutable group IDs). Revocations disable the principal and revoke its local tokens and sessions on apply. Leaving the federation removes federation-managed roles and grants; principals stay as local principals with their local roles.
- **Assertions.** Cross-member requests carry a signed, 60-second, request-bound assertion on the mTLS listener; replays are refused through `federation_assertion_nonces` and skew beyond 60 seconds is refused. `POST /api/v1/federation/access-check {"member_id":…,"kind":"feed","feed_key":…,"action":"read"}` asks another member for its own decision for the calling federated principal (local principals get `403 not_federated`).
- **Stale policy (D4).** Members keep enforcing the last known good policy when the authority is unreachable or a document is rejected (forged, tampered, stale, gap): rejections are audited (`federation.document.rejected`, kind `policy`) and shown in status. Alerts `policy_stale` (warning after 15 minutes, critical after 24 hours without confirmation), `policy_rejected`, `membership_stale` and `policy_conflicts` appear in `GET /api/v1/federation/status` (`alerts`), mark health `degraded` (never unready), are exported as `ant_federation_alert{code,severity}` and raised/cleared once per transition as `federation.alert.raised` / `federation.alert.cleared` events (webhooks and audit sinks deliver them). The optional hard limit `policy_max_age_seconds` (default off) denies federated principals beyond it; local principals are unaffected.

MCP: `get_federation_policy`, `list_federated_principals`, `check_federation_access` and the destructive `publish_federation_policy` (`confirm: true`); resources `ant://federation`, `ant://federation/members`, `ant://federation/policy`. Metrics add `ant_federation_document_generation{kind="policy"}`, `ant_federation_document_age_seconds{kind="policy"}`, `ant_federation_policy_unresolved_feed_keys` and `ant_federation_assertions_total{outcome}`. Audit adds `federation.policy.applied|draft_saved`, `federation.principal.materialized|revoked|restored|adopted`, `federation.feed.bound|unbound`, `federation.settings.updated`, `federation.assertion.accepted` (sampled) and `federation.assertion.rejected`.

## Offline maintenance

Run with the serving process stopped (both refuse while the writer lease is held):

- `ant federation status` prints the local federation view from the database.
- `ant federation re-identify --confirm` gives a restored copy of an instance a new instance ID (every UUID column holding the old ID is rewritten in one transaction, encrypted secrets are re-sealed under the new ID) and resets its federation state to none, so it can join with a new invitation instead of being refused as a duplicate. Update `ANT_INSTANCE_ID` if it is set.
- `ant federation purge --confirm` deletes all federation rows (state, documents, members, invitations, drafts, bindings, federation keys) of an instance that left or was removed, so a binary downgrade sees no federation data. It is irreversible.
