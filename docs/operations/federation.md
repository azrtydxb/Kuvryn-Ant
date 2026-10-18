# Federation (Phase 2, P2-M1: membership)

Federation groups independently operable Kuvryn Ant instances under one signed, versioned membership. This release delivers the foundation: founding, invitations, join, leave, suspend, resume, remove, membership sync and the status views. Shared policy, search, routing and coordinated replication are later milestones; see [the design](../planning/phase-2-federation.md).

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
