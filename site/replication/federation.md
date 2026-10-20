# Federation

Federation groups independently operable Kuvryn Ant instances under one signed, versioned membership. On top of [Phase 1 replication](index.md) it adds shared identity and policy, coordinated replication with one home per feed, cross-instance search, placement-aware read routing, and authority operations (handover, forced recovery, recovery-key drills, federated audit).

Serving never depends on federation. A federation listener that cannot bind, an unreachable authority or a rejected document is logged and shown as `degraded` in federation status, and the instance keeps serving with the last applied state. Readiness probes never fail because of federation.

## Concepts

| Concept                     | Meaning                                                                                                                                                                                                                             |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Federation**              | A named group of instances (members). Each member keeps its own PostgreSQL, blob storage and writer lease; there is no shared database and no consensus cluster.                                                                    |
| **Authority**               | The one member that signs federation documents and issues member certificates. The role has an **epoch**; it can be handed over (planned) or recovered with the offline recovery key (forced).                                      |
| **Member**                  | An instance that joined with an invitation. It has a member key, a certificate from the federation CA (30 days, renewed automatically after two thirds of its lifetime) and labels (for example `site=eu`).                         |
| **Invitation**              | A one-time secret bound to a member name and labels, valid one hour by default (at most 7 days), handed to the joining administrator out of band.                                                                                   |
| **Signed documents**        | **Membership**, **policy** and **topology** documents, each signed by the authority and chained by hash with increasing generations. Members verify and apply them; any member can relay them because they are self-authenticating. |
| **Policy**                  | Identity domains (trusted issuers), federated roles (`fed:` keys) with grants on feed keys, principal and group bindings, federated service accounts and revocations. Enforced locally by every member.                             |
| **Feed key**                | A portable feed name, `<federation>/<slug>`. A **feed binding** maps a key to a local feed on a member. Federated grants on an unbound key stay inert.                                                                              |
| **Home**                    | The single member that is authoritative (writable) for a feed key.                                                                                                                                                                  |
| **Replication rule**        | Authority-defined rule that makes the targeted members replicate a feed key from its home. Each member's reconciler turns rules into ordinary replication jobs.                                                                     |
| **Placement-aware routing** | A read on a member that lacks the content is routed (proxied or redirected) to a member that holds the key. Writes are never routed.                                                                                                |
| **Quarantine**              | Content an old home accepted after a **forced** home transfer is detached into a quarantined source instead of being merged.                                                                                                        |

Members poll the authority every 30 seconds and receive signed nudges after each change. If the authority is unreachable they fetch documents from any other active member. A member whose chain diverges recovers with a snapshot resync from the authority; the applied generation never moves backwards.

## Requirements

- **Clock.** NTP on every member: signed requests are rejected beyond 60 seconds of clock skew.
- **Network.** A reachable federation port between members, in **both directions** for full functionality. A member that initiates traffic (status pings, nudges, search and proxied reads, replication from a feed home) must be able to resolve and reach the other member's federation URL.
- **TLS passthrough.** The federation listener does mutual TLS itself; TLS must reach the pod unterminated.
- **Root key.** Federation keys (CA, authority signing key, member keys) are stored encrypted in PostgreSQL under the root key. No new secret is mounted; back up the root key as before (see [Root key](../install/root-key.md)).

## Configuration

| Variable                    | Purpose                                                                                                                                                                                                                                |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ANT_FEDERATION_LISTEN`     | Address of the dedicated mTLS listener, for example `0.0.0.0:8444`. A separate server, never mounted on the public HTTP listener. Unset means no listener.                                                                             |
| `ANT_FEDERATION_PUBLIC_URL` | `https://host:port` other members use to reach the listener. Required to found or join. The member certificate's SAN is this host. It cannot be changed after joining.                                                                 |
| `ANT_FEDERATION_JOIN_DIR`   | Optional directory (a mounted Secret) with files `authority_url`, `ca_sha256`, `invitation` and optionally `authority_cert_sha256`. An instance that was never federated joins automatically at startup and retries until it succeeds. |
| `ANT_PUBLIC_URL`            | The member's API URL as published in membership. Other members use it for redirects, URL rebasing of proxied metadata and to match existing replication peers during adoption.                                                         |
| `ANT_FEDERATION_ROUTE_*`    | Routing abuse limits; see [Routing limits](#routing-limits).                                                                                                                                                                           |

### Helm values

```yaml
publicURL: https://ant-a.example.com
federation:
  enabled: true
  port: 8444
  publicURL: https://ant-federation-a.example.com:8444
  service:
    type: LoadBalancer # LoadBalancer | NodePort | ClusterIP
    port: 8444
    loadBalancerSourceRanges: [10.0.0.0/24]
  # Alternative 1: ingress-nginx started with --enable-ssl-passthrough
  ingressPassthrough:
    enabled: false
    className: nginx
    host: ""
  # Alternative 2: Gateway API TLSRoute on a TLS listener in Passthrough mode
  tlsRoute:
    enabled: false
    hostname: ""
    parentRefs: [] # gateway plus the TLS Passthrough listener sectionName
  join:
    existingSecret: "" # Secret with authority_url, ca_sha256, invitation (automatic join)
  networkPolicyFrom: [] # peers allowed to reach the federation port when networkPolicy.enabled
# Resolve another member's federation name that only its own site resolves:
hostAliases:
  - ip: 10.0.0.20
    hostnames: [ant-federation-b.example.com]
```

- Use the federation Service directly (LoadBalancer or NodePort), ingress-nginx SSL passthrough, or a Gateway API `TLSRoute` on a listener with protocol TLS and mode Passthrough. In every case mTLS ends in the pod. With a `TLSRoute`, `federation.service.type` can be `ClusterIP`.
- A `ClusterIP` federation Service without a route is reachable only inside the cluster. If another site must reach this member, add a reachable companion Service (for example a LoadBalancer on the local network) and make the other site resolve the name, through DNS or `hostAliases`.
- When a cloud load balancer does not preserve client IPs, restrict the source allowlist at the load balancer or its security list, not in the gateway.
- Size the federation load balancer for replication traffic: after adoption, federation-mode jobs transfer blobs over the federation listener, not the public ingress.

See [Helm chart](../install/helm.md) and [Ingress, Gateway API and TLS](../install/ingress-tls.md).

## Found and join

All operations are available on the console Federation page and the management API (`/api/v1/federation...`). Reads need `read` on the instance; changes need instance `admin`.

### 1. Found (on the intended authority)

```sh
curl -fsS -X POST $A/api/v1/federation -H "$H" -H "$J" -H 'Idempotency-Key: fed-found-1' \
  -d '{"name":"myfed","member_name":"site-a","labels":{"site":"a"}}'
```

The response contains the **recovery key once** (`recovery_key`), its public key and the CA fingerprint `ca_sha256`. Store the recovery key offline (password manager or sealed envelope) on the machine where you will run the offline signing commands. It is never stored on the instance and is required for forced recovery and for rotating the recovery key.

### 2. Invite (on the authority)

```sh
curl -fsS -X POST $A/api/v1/federation/invitations -H "$H" -H "$J" -H 'Idempotency-Key: fed-invite-b-1' \
  -d '{"name":"site-b","labels":{"site":"b"},"ttl_seconds":3600}'
```

The answer contains `secret` (shown once), `authority_url`, `ca_sha256` and `authority_cert_sha256`. Hand all four to the joining administrator out of band.

### 3. Join (on the new member)

```sh
curl -fsS -X POST $B/api/v1/federation/join -H "$HB" -H "$J" -H 'Idempotency-Key: fed-join-1' -d '{
  "authority_url":"https://ant-federation-a.example.com:8444",
  "ca_sha256":"<64 hex>",
  "authority_cert_sha256":"<64 hex>",
  "invitation":"<secret>"}'
```

Or mount a Secret with the same values as files and set `ANT_FEDERATION_JOIN_DIR` (Helm `federation.join.existingSecret`) for automatic join at startup.

The join connection is pinned to the CA fingerprint; the system trust store is not used. `authority_cert_sha256` also pins the authority's certificate before the invitation is sent. The member verifies the whole signed membership chain and refuses an answer from a server whose certificate is not the one membership publishes for the authority. The authority refuses a member whose federation URL host is already used by another member.

Joining is two-phase: the join claims the invitation and adds the member; the joiner verifies the answer and confirms with a statement signed by its member key, which consumes the invitation. An unconfirmed claim (interrupted or relayed join) is released after 10 minutes: the provisional member is removed and the invitation can be used again.

### Leave, suspend, remove

- **Leave** (on a member): `POST /api/v1/federation/leave {"confirm":true}`; `"force":true` leaves locally if the authority is unreachable. Content and local configuration are kept. The authority cannot leave; hand the role over first.
- **Suspend / resume / remove** (on the authority): `POST .../members/{id}/suspend|resume`, `DELETE .../members/{id}?confirm=true`. Removal revokes the certificate serial.

## Shared identity and policy

The authority publishes the policy document: draft with `PUT /api/v1/federation/policy/draft {"policy":{...}}`, publish with `POST /api/v1/federation/policy/publish {"base_generation":N,"confirm":true}` (a stale base answers `409 stale_generation`). History and diffs: `GET /api/v1/federation/policy` and `GET /api/v1/federation/policy/diff?from=&to=`.

On each member:

- **Bind feed keys**: `POST /api/v1/federation/feed-bindings {"feed_key":"myfed/npm","feed_id":"<local feed id>"}`. Grants on an unbound key stay inert.
- **Local overrides**: local deny roles beat federated allows; local allows add to federated ones; federated denies beat local allows. Federated instance-admin allows, and artifact or feed allows without a feed key, are inert until `PUT /api/v1/federation/settings {"accept_admin_grants":true}`.
- **Principals**: federated service accounts are materialized on every member (issue their tokens per member; tokens are never valid across members). Federated users are materialized when bound or at their first sign-in through a matching identity domain. Revocations disable the principal and revoke its local tokens and sessions on apply.
- **Stale policy**: members keep enforcing the last good policy when the authority is unreachable or a document is rejected; alerts `policy_stale` (15 minutes, critical after 24 hours), `policy_rejected` and `membership_stale` mark health `degraded`. The optional `policy_max_age_seconds` setting denies federated principals beyond that age; local principals are unaffected.

Cross-member requests carry a signed, 60-second, request-bound assertion of the caller over the mTLS listener; the receiving member authorizes it under its own policy.

## Coordinated replication

The authority publishes a **topology** document with feed keys, their homes, and replication rules.

```sh
# On the home member (here the authority): bind the feed, then publish the key with its home
curl -fsS -X POST $A/api/v1/federation/feed-bindings -H "$H" -H "$J" -H 'Idempotency-Key: bind-npm-a' \
  -d "{\"feed_key\":\"myfed/npm\",\"feed_id\":\"$NPM_FEED_A\"}"
curl -fsS -X POST $A/api/v1/federation/feeds -H "$H" -H "$J" -H 'Idempotency-Key: key-npm' \
  -d "{\"feed_key\":\"myfed/npm\",\"home_member_id\":\"$MEMBER_A\"}"

# A rule: every member labelled site=b replicates myfed/npm from its home
curl -fsS -X POST $A/api/v1/federation/replication/rules -H "$H" -H "$J" -H 'Idempotency-Key: rule-npm-b' -d '{
  "feed_key":"myfed/npm","targets":{"labels":{"site":"b"}},
  "mode":"continuous","interval_seconds":30,"include_cached":false,
  "deletion_policy":"propagate","destination_read_only":true}'
```

Rule fields and defaults: `mode` (`continuous`), `interval_seconds` (300), `include_cached` (`true`), `filters`, `deletion_policy` (`ignore`), `destination_read_only` (`true`), `create_destination_feed` (`true`), `adopt_existing` (`true`), `max_lag_seconds` (900). Targets are `{"labels":{...}}` or `{"members":[...]}`; a rule cannot explicitly target the home member.

Every member's **reconciler** (under the writer lease, after every topology apply and every 30 seconds) ensures, for each rule that targets it: the destination feed (the bound feed, or a new feed created from the key's descriptor when `create_destination_feed` is set and no feed with that slug exists; an existing unbound feed is never taken over), a federation-mode peer for the home (`fed-<member>`, no token) and one job with `origin=federation`. Federation-mode jobs pull over the mTLS listener with the member certificate and request signatures. Federation-managed peers and jobs are read-only through `/api/v1/replication/...` (`409 federation_managed`); manual runs are allowed. A removed rule disables its job and releases it to `origin=local`, keeping content.

**Adoption without reseed.** An existing Phase 1 job that already replicates the home feed into the bound feed (its peer is the home member, or a token peer whose URL equals the home's API URL, which must be the home's `ANT_PUBLIC_URL`) is adopted in place: checkpoint, phase, counters and replica source stay, only origin, rule and peer change. With `adopt_existing: false`, adopt explicitly with `POST /api/v1/federation/replication/adopt {"job_id":...,"rule_id":...}`.

Status: `GET /api/v1/federation/topology`, `GET /api/v1/federation/replication/status` (per rule `in_sync|lagging|seeding|failed|pending|missing_feed|conflict|disabled`, `adopted`), and `GET /api/v1/federation/replication/rules/{id}/compliance` (the rule on every target).

Cache feeds never take part: bindings, topology keys and rules refuse them.

### Home transfer

`POST /api/v1/federation/feeds/{fed}/{slug}/transfer {"to_member_id":...,"mode":"planned"|"forced","confirm":true}` on the authority. The target must be an active member that a rule for the key targets (it holds a replica). One transfer per key at a time.

- **Planned.** Freeze (the home answers publishes and deletes with `503 feed_transferring`), drain (the target's job reaches the frozen head), promote (the target converts its replica source into a local source), complete (topology names the new home; the old home demotes its local source into a replica of the new home, deduplicating every blob). Freeze and drain abort after `drain_timeout_seconds` (default 900) or with `POST .../transfer/abort {"confirm":true}`. Promotion cannot be aborted.
- **Forced** (`"accept_data_loss": true`), for a lost home: the target is promoted immediately. An old home that returns fences itself, detaches its local sources as **quarantined** sources (with the number of changes after the target's checkpoint) and seeds a fresh replica. Nothing is merged. Alert `feed_home_conflict` stays until `POST /api/v1/federation/quarantines/{id}/resolve {"confirm":true}`.

## Placement-aware routing

A read (`GET` or `HEAD`) of a federated feed is answered locally first when a local feed is bound to the key. A local `404`, or a key without a local feed (the client uses `/feeds/<key-slug>/<format>/...` or `/v2/<key-slug>/...` on any member), is routed to a member that holds the key.

- **Candidates**: active members holding the key, replicas first and the home last. Replicas not `in_sync` (or lagging beyond `max_lag_seconds`) are skipped. Ordering: label preference (`prefer`), replica state, then RTT from status pings. At most four members are tried; a `404` or a refusal moves on. All down or excluded: `503 route_unavailable` with `Retry-After`.
- **Circuit breaker**: three consecutive failures open a member's circuit for 30 seconds, doubling up to 4 minutes.
- **Writes are never routed.** Publishes and deletes are answered locally with `Ant-Feed-Home: <home feed endpoint>`.
- **Who is routed**: federated principals and anonymous callers. A local principal gets the local answer with `X-Kuvryn-Route: not_federated`. Directory passwords are verified only for local feeds; reaching a feed this member does not hold needs a token or a browser session.

Configure per key on the authority:

```sh
curl -fsS -X PUT $A/api/v1/federation/feeds/myfed/npm/routing -H "$H" -H "$J" -H 'Idempotency-Key: route-npm-1' \
  -d '{"mode":"proxy","blob_urls":false,"cache":false,"cache_ttl_seconds":0,"max_lag_seconds":0,"prefer":["site"]}'
```

| Mode / option     | Behaviour                                                                                                                                                                                                                                                                                                                                                                                                        |
| ----------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `proxy` (default) | The entry member streams the holder's answer over the mTLS listener with the caller's signed assertion. The holder applies its own policy. Range, conditional, `Accept` and `User-Agent` headers pass through; credentials, cookies and client addresses do not. Feed URLs in textual metadata and `Location`/`Link` headers are rebased onto the entry member; content-addressed documents are never rewritten. |
| `redirect`        | `307` to the first candidate's public feed endpoint; the client must authenticate there itself. Only for entry members without a local copy.                                                                                                                                                                                                                                                                     |
| `off`             | Local answers only.                                                                                                                                                                                                                                                                                                                                                                                              |
| `blob_urls`       | Proxy mode: digest-addressed reads (OCI blobs and manifests by digest) get a `307` to a signed, five-minute URL on the holder's public listener. `blob_url_bind_client` binds it to the client address.                                                                                                                                                                                                          |
| `cache`           | Proxy mode: entry members with a bound feed keep immutable proxied answers for `cache_ttl_seconds` (default 86400). Non-digest answers are revalidated with the holder on every hit.                                                                                                                                                                                                                             |

Responses carry `X-Kuvryn-Route: proxy; member=<name>`, `redirect; ...`, `blob_url; ...` or `cache`. Explain a decision with `GET /api/v1/federation/routes/{fed}/{slug}` on any member, or `GET /api/v1/feeds/{id}/resolve` (see [Read order and resolve explain](../concepts/resolution.md)).

### Routing limits

| Variable                                     | Default                                      | Helm (`federation.routing.*`) |
| -------------------------------------------- | -------------------------------------------- | ----------------------------- |
| `ANT_FEDERATION_ROUTE_MAX_STREAMS`           | 64 concurrent proxied reads per entry member | `maxStreams`                  |
| `ANT_FEDERATION_ROUTE_STREAMS_PER_PRINCIPAL` | 8                                            | `streamsPerPrincipal`         |
| `ANT_FEDERATION_ROUTE_STREAMS_PER_CLIENT`    | 16                                           | `streamsPerClient`            |
| `ANT_FEDERATION_ROUTE_PRINCIPAL_RATE`        | 50 routed reads/s per principal (burst 2x)   | `principalRate`               |
| `ANT_FEDERATION_ROUTE_MIN_TRANSFER_RATE`     | 16384 bytes/s after the grace period         | `minTransferRate`             |
| `ANT_FEDERATION_ROUTE_TRANSFER_GRACE`        | `30s`                                        | `transferGrace`               |
| `ANT_FEDERATION_ROUTE_TRANSFER_TIMEOUT`      | `2h`                                         | `transferTimeout`             |

Unset or `0` keeps the default; invalid values stop startup. Signed blob URLs carry a ticket in the query string: configure ingress access logs in front of the holder to log the path without the query for `/federation/blob/`.

## Cross-instance search

`GET /api/v1/federation/search?q=&format=&namespace=&digest=&feed_key=&members=&limit=&cursor=` searches this member and fans out to the other active members with a signed assertion of the caller; each member applies its own policy. Only feeds bound to a feed key take part, and only federated principals can search. A member over its deadline (1.5 s per member, 2 s overall) never fails the query; the answer is then `partial`. `digest=sha256:<hex>` is the checksum lookup. The console Packages page has a "This instance" / "Federation" scope toggle.

## Authority operations

### Planned handover

`POST /api/v1/federation/authority/handover {"to_member_id":"<id>","confirm":true}` on the current authority. The target generates its own epoch key; the current authority certifies it and sends the federation CA key over the mutually authenticated channel; the target signs its acceptance and publishes the first document of the next epoch. The old authority deletes its authority and CA keys and becomes a member. Refused while a home transfer is active or when the target has not applied the latest documents. Publishing is paused for at most 10 minutes; `POST .../authority/handover/abort` clears it. Step by step: [Federation runbooks](federation-runbooks.md#planned-handover).

### Forced recovery

When the authority is lost or compromised, a member takes over with the offline recovery key:

1. `POST /api/v1/federation/authority/recovery-template` on that member (creates the pending authority key and returns a template).
2. Offline: `ant federation recovery sign-authority-cert --template template.json --key recovery.key > request.json`.
3. `POST /api/v1/federation/authority/recover` with the signed certificate and `"confirm": true` (optionally `"remove_previous_authority": true`).

The new authority creates a new federation CA; both CAs are trusted. A returning old authority learns of the higher epoch, replaces its voided generations, deletes its keys and raises `authority_fenced`. Members accept the highest epoch whose chain verifies back to the pinned recovery key. See [Federation runbooks](federation-runbooks.md#forced-recovery).

### The recovery key

The recovery key is created at founding, shown once, and never stored on any instance. Every recovery-key operation is signed **offline** with the `ant federation recovery` commands, which need neither the database nor the network; the key is read from a file (the `ant-recovery-v1:` export), never from arguments or standard input. See [CLI](../reference/cli.md).

| Operation                                                                                             | Steps                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                   |
| ----------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Drill** (prove the key still exists; after founding, after every rotation, at least every 180 days) | `POST /api/v1/federation/recovery-key/challenge` on the authority (one-time, valid one hour) → offline `ant federation recovery sign-challenge --challenge challenge.json --key recovery.key > proof.json` → `POST /api/v1/federation/recovery-key/verify` with `proof.json`. `GET /api/v1/federation/authority` then shows `recovery.verified_at`. Alert `recovery_key_unverified` after 180 days (30 days after founding if never done).                                                                                                                                                                              |
| **Rotate**                                                                                            | `GET /api/v1/federation/recovery-key/rotation` → save as `template.json` → offline `ant federation recovery sign-rotation --template template.json --key current.key --new-key-out new.key > rotation.json` → offline `ant federation recovery sign-authority-cert --template template.json --key new.key --rotation rotation.json > request.json` → `POST /api/v1/federation/recovery-key/rotate` with `request.json`. A membership change after the template answers `409 stale_generation`: fetch a new template and sign again. Store `new.key` offline and destroy the old key once the new generation is applied. |
| **Forced recovery**                                                                                   | `recovery-template` → `sign-authority-cert` → `recover`, as above.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |

`new.key` is created with mode 0600 and never overwritten.

## Status, alerts and audit

- `GET /api/v1/federation/status`: state, role, epoch, applied generations and their age, certificate expiry, last sync, member counts, `alerts[]`.
- `GET /api/v1/federation/members`: per-member RTT, clock skew, last seen.
- `GET /api/v1/federation/authority`: epoch history, handover in progress, fence record, recovery drill state.
- `GET /api/v1/federation/audit`: audit events of all members, newest first (requires instance administration on each member).
- Metrics: `ant_federation_active`, `ant_federation_document_generation{kind}`, `ant_federation_authority_epoch`, `ant_federation_members{status}`, `ant_federation_alert{code,severity}`, `ant_federation_member_up{member}`, `ant_federation_member_rtt_seconds{member}`, routing and search counters.

Alert when health is `degraded` for more than 15 minutes. The full alert list and procedures are in [Federation runbooks](federation-runbooks.md). MCP exposes federation status, policy, topology, routing explain and the destructive operations with `confirm: true` (see [MCP tools](../reference/mcp.md)).

## Offline maintenance

Run with the serving process stopped:

- `ant federation status` prints the local federation view from the database.
- `ant federation re-identify --confirm` gives a restored copy of an instance a new instance ID and resets its federation state, so it can join with a new invitation.
- `ant federation purge --confirm` deletes all federation rows of an instance that left or was removed. Irreversible.

::: warning Limits

- A member's federation URL (`ANT_FEDERATION_PUBLIC_URL`) cannot be changed after joining; other members must keep resolving that name (DNS or `hostAliases`).
- Planned and forced authority handover, forced recovery, the recovery-key drill, intercepted joins, revocation pruning and leave/rejoin were qualified in the repository's multi-instance test harness, not on live production sites. Adoption of existing jobs, federated search, routed reads, planned home transfer in both directions and a network partition were qualified on two live sites.
- The independent security review of the proxy, blob-ticket and epoch code is listed as an open item in the design document.
- Feed descriptors carry format and anonymous-read default only; other feed settings are not federated.
- Feed keys have no update operation: a home changes only by transfer.
  :::
