# Multi-site topology: OKE and kw

Two independent Kuvryn Ant instances run side by side and are joined with Phase 1
replication (spec 6.7, `internal/replication/README.md`). Phase 2 coordinated
replication (`docs/planning/phase-2-federation.md`, P2-M3) adopts the jobs below
in place once both sites are federated; see the last section.

| Site | URL                                                                      | Storage            | Role                                                                      |
| ---- | ------------------------------------------------------------------------ | ------------------ | ------------------------------------------------------------------------- |
| OKE  | `https://ant.azrty.com` (public certificate)                             | OCI Object Storage | **Authority** for the hosted feeds                                        |
| kw   | `https://ant.kw.watteel.lab` (private CA `cluster-ca`, LAN 192.168.10.x) | TrueNAS MinIO      | Read-only replica of OKE hosted content, plus its own pull-through caches |

## Network

Replication is pull-based: the destination calls the authority. So the only
required path is **kw -> `ant.azrty.com`:443** (public DNS, public certificate,
no `ca_ref` needed). Verified from the kw pod.

The reverse path is not needed and not configured. From an OKE pod the kw ingress
IP (192.168.10.120) answers on 443, but `ant.kw.watteel.lab` does not resolve
there and the certificate is signed by the private CA, which OKE does not trust.
A kw-authoritative feed would need: DNS (or a host alias) for the name on OKE,
the `cluster-ca` registered in `ANT_UPSTREAM_CA_MAP_FILE` on OKE, and a peer plus
job created on OKE pointing at kw. There is no push mode. Until then, treat kw as
read-only for the shared feeds (see "Authority").

## Authority

OKE is authoritative for these feeds (same slug on both sites):
`oci-hosted npm go cargo pypi maven helm nuget sccache`.
A feed converted to purpose `cache` (see `build-caches.md`) leaves this set: cache feeds are never replicated, so
its rule, topology key, bindings and `oke-<slug>` job must be removed first.

kw has one continuous job per feed (`oke-<slug>`, 30s poll, peer `oke-prod`):

- `include_cached=false`: OKE's pull-through cache entries are not copied. Each
  site keeps its own cache.
- Each job added a `replica` source at the lowest priority of the kw feed, so
  kw's own `local`/`remote` sources win. Replica sources reject publishes.
- `deletion_policy=propagate`. Enabled after a dry run
  (`GET /api/v1/replication/jobs/{id}/deletions`) showed only the intended
  deletions. Deletes only touch rows the job itself replicated.
- kw `ev5*` feeds and their jobs belong to evidence testing; do not reuse them.

The jobs run with `destination_read_only=true`: the kw feeds reject publishes
with 409 `replica feed — publish to <peer URL>` (OCI and npm show the text). An
explicit publish rule routing to a separate local source is the only override.
Jobs created before this option existed default to `false` until enabled with a
job PATCH. Content published on kw directly (when not read-only) is kw-only,
never replicated, and shadows the replica of the same coordinate.

## Credentials

- OKE principal `kw-replicator` (service), role `kw-replicate`: `read` and
  `replicate` on the nine feeds only.
- Its token is scoped to the same grants, **expires after at most one year**
  (current expiry 2027-10-13), and is stored encrypted in the kw peer
  `oke-prod`. It is never returned by the API.

Rotation (before expiry; alert on `ant_replication_consecutive_failures`):

1. On OKE: `POST /api/v1/principals/{kw-replicator}/tokens` with the nine
   feed scopes and a new `expires_at`.
2. On kw: `PATCH /api/v1/replication/peers/{oke-prod}` with
   `{"token": "<new>"}` with `If-Match: "N"` (a body `revision` still works).
3. Confirm job status, then revoke the old token with `DELETE /api/v1/tokens/{id}`.

## Operations

Status: `GET /api/v1/replication/status` or the console Replication page on kw (incoming jobs); on OKE the same page lists the consumers pulling from it (`GET /api/v1/replication/outgoing`).
Healthy: `phase=incremental`, `changes_behind=0`, `consecutive_failures=0`,
`last_success_at` within a few poll intervals. Metrics on kw `/metrics`:
`ant_replication_seconds_since_last_success{job}`, `_changes_behind`,
`_consecutive_failures`, `ant_replication_lag_seconds`. The API and metric
`lag_seconds` grow from the last success while the peer is unreachable.

Authority outage: kw keeps serving everything already replicated; jobs back off
and resume from their checkpoint. Verified by pointing the peer at an
unreachable host (`PATCH` url) and pulling an npm package and an OCI image from
kw with fresh client caches, then restoring the URL.

Long outage: if kw lags past OKE's change history (`ANT_REPLICATION_HISTORY_MAX_AGE`,
default 336h), the job reseeds automatically; deletions during the gap appear as
held deletions in the dry run.

Pause: `PATCH` the job with `enabled=false`. Add a feed: create the feed on both
sites, grant the feed to the OKE role/token (re-issue the token), then create a job.

## Smoke test (repeatable)

1. Publish an npm package (`@kuvryn-multisite-smoke/...`) and
   `oci-hosted/replication-smoke:1` to OKE.
2. Within ~30s `npm view` / `crane digest --insecure` against kw return them.
3. Delete on OKE; they disappear on kw within one interval (`deletions_applied`
   increments). A leftover empty component row on kw is reported `protected` in
   the dry run and is harmless.

## Client IP behind the ingress

Auth failure throttling and per-site limits key on the client address. Behind
ingress-nginx or Envoy every client shares the proxy address unless
`ANT_TRUSTED_PROXIES` (chart value `trustedProxies`, CIDRs) lists the proxy pod
network: forwarded headers (`X-Forwarded-For`, then `X-Real-IP`) are believed only
from those peers, walking the chain from the right. Failures are budgeted per
(client, Basic username) with a looser per-client aggregate, and credential-free
challenge probes are never counted. kw uses `10.42.0.0/16` (ingress-nginx pods),
OKE `10.244.0.0/16` (Envoy). kw's ingress Service has
`externalTrafficPolicy: Cluster`, so LAN clients still arrive SNAT'd to a node
address; set it to `Local` to preserve true client IPs.

## Federation: adopting the oke-* jobs (P2-M3)

Coordinated replication (`docs/operations/federation.md`, "Coordinated
replication") takes over the nine jobs in place, without a reseed. With OKE as
authority and kw joined with label `site=kw`:

1. On OKE, bind each of the nine OKE feeds to its key
   (`POST /api/v1/federation/feed-bindings {"feed_key":"watteel/<slug>","feed_id":…}`)
   and publish the key with OKE as home
   (`POST /api/v1/federation/feeds {"feed_key":"watteel/<slug>","home_member_id":<OKE>}`).
2. On kw, bind the existing kw feed of the same slug to the same key. The
   reconciler never takes over an unbound feed (status `conflict` until bound).
3. On OKE, create one rule per key that reproduces today's job settings, so
   adoption changes nothing but the transport:
   `{"feed_key":"watteel/<slug>","targets":{"labels":{"site":"kw"}},"mode":"continuous","interval_seconds":30,"include_cached":false,"deletion_policy":"propagate","destination_read_only":true}`
   (`adopt_existing` defaults to true).
4. kw's reconciler matches each `oke-<slug>` job: its peer `oke-prod` URL is
   OKE's API URL in the membership (`https://ant.azrty.com`, which must equal
   OKE's `ANT_PUBLIC_URL`), its remote feed is OKE's home feed and its
   destination is the bound kw feed. The job keeps its name, checkpoint, phase,
   counters and replica source; `origin` becomes `federation`, the peer becomes
   the federation-mode peer `fed-oke` (mTLS on the federation listener, no
   token). `GET /api/v1/federation/replication/status` on kw shows each rule
   `in_sync` and `adopted: true`; `reseeds` stays unchanged on the job status.
5. After all nine are adopted, `oke-prod` and the `kw-replicator` token are no
   longer used. Keep them until the federation path has run for a while, then
   revoke the token on OKE and delete the peer on kw.

The kw → OKE federation path must work (it is the one kw already uses to
sync documents). If adoption is not wanted yet, create the rules with
`"adopt_existing": false`: kw reports `conflict` and keeps the Phase 1 jobs
untouched; adopt them one at a time with `POST /api/v1/federation/replication/adopt`.

## Federation as run (P2-M6)

OKE is the federation authority and home of the nine shared feeds, kw a member
labelled `site=kw`; the nine `oke-*` jobs were adopted in place (see
`docs/implementation/federation-evidence.md` for the evidence and
`docs/operations/federation-runbooks.md` for procedures).

- **Both directions must work.** kw -> OKE (`ant-federation.azrty.com:8444`, the
  OKE LoadBalancer, source ranges restricted) carries document sync, replication
  and kw's search and routed reads. OKE -> kw carries OKE's status pings and
  nudges, searches and proxied reads started at OKE, and replication of any feed
  whose home moves to kw (a planned home transfer to kw makes OKE a replica that
  pulls from kw). kw's chart-managed federation Service is ClusterIP, so kw
  publishes a second Service with its own LAN address
  (`deploy/examples/kw-federation-lan.yaml`, `192.168.10.150:8444`) and OKE's pod
  resolves `ant-federation.kw.watteel.lab` through `hostAliases`
  (`deploy/examples/azrty-oke.yaml`). OKE reaches the LAN over the site link;
  only port 8444 on that address is added.
- **Partition drills on kw.** A NetworkPolicy that blocks egress to the OKE
  federation address must still allow in-cluster destinations explicitly
  (`namespaceSelector: {}`): Cilium does not match cluster endpoints with
  `ipBlock: 0.0.0.0/0`, so an `ipBlock`-only egress policy cuts PostgreSQL and
  crash-loops the pod (observed, about 80 s of downtime). The tested policy is
  `evidence/2026-10-19/federation/10-partition-networkpolicy.yaml`; it also
  closes kw's federation port for ingress so OKE cannot nudge kw during the drill.
- **Upgrades.** Take and verify `ant backup` on both sites first (about 5 and
  10 minutes for the current data), roll with
  `helm upgrade ... --reset-then-reuse-values --set image.tag=... --set image.digest=sha256:<build output>`,
  and let the migrations run once per site.
