# Replication and multi-site

Replication copies the content of a feed from one Kuvryn Ant instance (the **authority** for that feed) into a feed on another instance. It is pull-based and per feed: the destination polls the authority, verifies every blob by digest and serves the result locally. There is no shared database and no multi-writer merge. A destination that is fully seeded keeps serving its clients when the authority is down.

This page covers Phase 1 replication between independent instances. [Federation](federation.md) builds coordinated replication, shared identity and cross-site routing on top of the same jobs.

## Concepts

### Change sequence (source side)

Every change to assets, tags, artifact subjects (indexes) and component metadata on the authority is recorded by database triggers in a per-instance change log. Sequence numbers are assigned at commit time under a per-instance lock, so the sequence is **durable, gapless and in commit order**: a reader that has seen change N never later sees a lower number, and aborted transactions consume nothing.

The authority exports only its own content: `local` sources and, unless excluded, cached pull-through content of `remote` sources. Content of `replica` sources is never recorded or exported, so replicas do not re-export what they received. Cache feeds ([build caches](../build-caches/index.md)) are never recorded or replicated.

Attaching a source that already holds content to a feed changes what the feed exports, so it is recorded as an `attachment` change; the destination then back-fills that source. Detaching a source is not recorded.

### Peers and jobs (destination side)

| Object             | What it is                                                                                                                                                                                                                                                           |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Peer**           | The authority as seen from the destination: its base HTTPS URL (plain HTTP only for loopback), an optional operator CA reference, and a bearer token for the authority's export API. The token is stored encrypted and never returned. Redirects are never followed. |
| **Job**            | One per destination feed: which peer, which feed on the peer (`remote_feed_id`), which local feed (`dest_feed_id`), mode, filters and policies.                                                                                                                      |
| **Replica source** | Creating a job attaches a new `replica`-kind source at the **lowest precedence** of the destination feed. The job writes replicated content into it. Replica sources cannot be published to.                                                                         |
| **Checkpoint**     | The last change sequence number the job applied.                                                                                                                                                                                                                     |

### Seed, incremental, reseed and back-fill

- **Seed.** The job records the peer's head, pages the asset manifest, then the tag manifest, then switches to incremental changes from the recorded head.
- **Incremental.** Changes are applied in sequence order. Modes: `continuous` (poll every `interval_seconds`), `scheduled` (interval) or `manual` (triggered with `POST /api/v1/replication/jobs/{id}/runs`).
- **Reseed.** The authority's change history is bounded. When the peer's low-water mark is above the job's checkpoint (seen on the head, or as a `410` on a change page), the job restarts the manifest walk automatically (`reseeding=true`, `reseeds` increments). It upserts only; blobs already present are deduplicated, not fetched. Assets of this job that the peer no longer lists were deleted during the purged window and become **held deletions**.
- **Back-fill.** An `attachment` change queues the attached source; before the next change page the job walks the manifest restricted to that source, with its own durable cursor.

### Durability and verification

Every page (metadata, cursor/checkpoint and counters) commits in one transaction fenced by the instance writer lease and the job claim; a restart resumes at the last committed cursor and applying a change twice is harmless. A blob already present (same digest and size) is never fetched again. Arriving bytes are staged and finalized against the manifest digest and size: a mismatch stores nothing and the checkpoint does not move. On local-disk storage a broken blob transfer resumes with `Range` from the bytes already staged; S3 storage restarts the blob from zero.

### Filters

`filters` restrict a job to `namespaces`, exact `packages` and `version_prefixes`. They apply to seed and incrementals and are evaluated on the authority. Filter changes affect only future changes.

### Deletions

Per job, `deletion_policy`:

- `ignore` (default): tombstones from the authority are recorded as **held** and nothing is deleted.
- `propagate`: deletions are applied, but only to rows in the job's own replica source that carry this job's provenance. Anything else is classified `protected` and kept.

`GET /api/v1/replication/jobs/{id}/deletions` is a dry run: it lists held and pending tombstones and what each would do, and changes nothing. Run it before switching a job to `propagate`.

### Conflicts and read-only destinations

The authority wins over its own earlier replicas. Content from a higher-precedence source of the destination feed (its own `local` or `remote` sources), or destination-only content inside the replica source, is never overwritten; such cases are counted in `conflicts`.

With `destination_read_only` (default `true` for new jobs) the destination feed rejects publishes with `409 replica feed — publish to <peer URL>` (the OCI and npm adapters show the text; other adapters return a plain `409`), unless an explicit publish rule routes the request to a separate local source. Destination policy, credentials and identity settings are never touched by replication.

### Configuration templates

Content and configuration are separate. The authority can export a reviewed, non-secret template of a feed: the feed retention policy (without `allow_release_delete`) and the cache/timeout tunables of its remote sources, matched by name. It never contains origins, credentials, CA references, identity or access policy. On the destination, `POST /api/v1/replication/jobs/{id}/template/preview` returns a classified diff and a digest; `POST .../template/apply` with that digest writes it in one transaction, or refuses (`409`) if anything changed or is blocked.

### History purge and the low-water mark

A background purge on the authority trims the change log to the more restrictive of a maximum age and a maximum row count. It first raises and commits the **low-water mark** and only then deletes rows, so a reader is always told about a gap instead of silently missing changes. A cursor below the mark gets `410` with `X-Replication-Low-Water`, and the job reseeds.

| Variable (authority)               | Default    | Meaning                                                |
| ---------------------------------- | ---------- | ------------------------------------------------------ |
| `ANT_REPLICATION_HISTORY_MAX_AGE`  | `336h`     | Keep at least this much change history (1m to 87600h). |
| `ANT_REPLICATION_HISTORY_MAX_ROWS` | `10000000` | Keep at most this many changes (at least 1000).        |
| `ANT_REPLICATION_PURGE_INTERVAL`   | `1h`       | Purge interval; `0` disables the purge.                |

The low-water mark is global per instance, not per consumer: a destination that lags past these bounds reseeds. Size them for the slowest peer you want to serve incrementally.

### Lag alert

On the destination, `ANT_REPLICATION_LAG_ALERT_CHANGES` (default `10000`, `0` disables) is the number of changes a job may trail its peer's head. When a job crosses it, Ant emits a `replication.job.lag` event (with `job_id`, `lag_changes` and `threshold`) once per excursion; it is delivered through webhooks like other events. The flag resets when the job is back under the threshold.

## Set up replication

The example replicates the `npm` feed of `https://ant-a.example.com` (authority) into the `npm` feed of `https://ant-b.example.com` (destination).

### 1. On the authority: a replication identity

Create a service principal whose role grants `read` and `replicate` on the feeds to replicate (grant kind `feed`; the export API authorizes `replicate` on the feed), and issue a token with the same scopes. Tokens expire after at most one year.

```sh
A=https://ant-a.example.com
H="Authorization: Bearer $ANT_ADMIN_TOKEN_A"; J='Content-Type: application/json'

FEED=$(curl -fsS "$A/api/v1/feeds?prefix=npm" -H "$H" | jq -r '.items[]|select(.slug=="npm").id')
P=$(curl -fsS -X POST $A/api/v1/principals -H "$H" -H "$J" -H 'Idempotency-Key: site-b-replicator' \
  -d '{"subject":"site-b-replicator","kind":"service"}')
R=$(curl -fsS -X POST $A/api/v1/roles -H "$H" -H "$J" -H 'Idempotency-Key: site-b-replicate-role' \
  -d '{"name":"site-b-replicate"}')
curl -fsS -X PUT $A/api/v1/roles/$(jq -r .id <<<"$R")/grants -H "$H" -H "$J" \
  -H 'Idempotency-Key: site-b-replicate-grants' -H "If-Match: \"$(jq -r .revision <<<"$R")\"" \
  -d "{\"grants\":[{\"kind\":\"feed\",\"effect\":\"allow\",\"feed_id\":\"$FEED\",\"actions\":[\"read\",\"replicate\"]}]}"
curl -fsS -X PUT $A/api/v1/principals/$(jq -r .id <<<"$P")/roles -H "$H" -H "$J" \
  -H 'Idempotency-Key: site-b-replicate-assign' -H "If-Match: \"$(jq -r .revision <<<"$P")\"" \
  -d "{\"role_ids\":[\"$(jq -r .id <<<"$R")\"]}"
PEER_TOKEN=$(curl -fsS -X POST $A/api/v1/principals/$(jq -r .id <<<"$P")/tokens -H "$H" -H "$J" -d @- <<JSON | jq -r .secret
{"scopes":[{"feed_id":"$FEED","actions":["read","replicate"]}],"expires_at":"2027-06-30T00:00:00Z"}
JSON
)
echo "remote feed id: $FEED"
```

### 2. On the destination: the peer

The destination feed must exist (same format, purpose `artifacts`). Peers need instance `admin`.

```sh
B=https://ant-b.example.com
HB="Authorization: Bearer $ANT_ADMIN_TOKEN_B"
PEER=$(curl -fsS -X POST $B/api/v1/replication/peers -H "$HB" -H "$J" -H 'Idempotency-Key: peer-site-a-1' \
  -d "{\"name\":\"site-a\",\"url\":\"$A\",\"token\":\"$PEER_TOKEN\"}" | jq -r .id)
```

If the authority's certificate is issued by a private CA, register that CA on the destination in `ANT_UPSTREAM_CA_MAP_FILE` and pass its ID as `ca_ref`. CA references are operator-configured IDs, never paths.

### 3. On the destination: the job (attaches the replica source)

```sh
DEST=$(curl -fsS "$B/api/v1/feeds?prefix=npm" -H "$HB" | jq -r '.items[]|select(.slug=="npm").id')
JOB=$(curl -fsS -X POST $B/api/v1/replication/jobs -H "$HB" -H "$J" -H 'Idempotency-Key: job-site-a-npm-1' -d @- <<JSON | jq -r .id
{"name":"site-a-npm","peer_id":"$PEER","remote_feed_id":"$FEED","dest_feed_id":"$DEST",
 "mode":"continuous","interval_seconds":30,"include_cached":false,
 "deletion_policy":"ignore","destination_read_only":true}
JSON
)
curl -fsS $B/api/v1/replication/jobs/$JOB/status -H "$HB" | jq
```

| Field                   | Default      | Notes                                                                                                                                                                   |
| ----------------------- | ------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `mode`                  | `continuous` | `continuous`, `scheduled` or `manual`                                                                                                                                   |
| `interval_seconds`      | `300`        | 1 to 604800                                                                                                                                                             |
| `include_cached`        | `true`       | `false` leaves out the authority's pull-through cache entries, so each site keeps its own cache. Flipping it to `true` later does not back-fill earlier cached content. |
| `filters`               | none         | `{"namespaces":[],"packages":[],"version_prefixes":[]}`                                                                                                                 |
| `deletion_policy`       | `ignore`     | `propagate` after a dry run                                                                                                                                             |
| `destination_read_only` | `true`       |                                                                                                                                                                         |

Job creation attaches the replica source to the destination feed at the lowest precedence; you do not attach it yourself. Jobs need `configure` on the destination feed to create or change and `replicate` to run or inspect. Every `POST`, `PATCH` and `DELETE` needs an `Idempotency-Key`; `PATCH` on peers and jobs needs the revision (`If-Match: "N"`).

**Console.** Replication page: "Add peer" (Peer name, Peer HTTPS URL, Operator CA ID, Access token), then "Create job" (Job name, Peer, Remote feed ID, Local destination feed, Mode, Interval, Include cached content, Propagate deletions, Read-only replica).

### 4. Verify and operate

| Endpoint                                      | Purpose                                                                                                                                                                                                    |
| --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /api/v1/replication/status`              | All jobs                                                                                                                                                                                                   |
| `GET /api/v1/replication/jobs/{id}/status`    | Phase, seed progress, checkpoint, remote head, changes behind, lag, last success, last error, failures, reseeds, peer low-water mark, back-fills, resumed bytes, transfer/dedup/deletion/conflict counters |
| `POST /api/v1/replication/jobs/{id}/runs`     | Trigger a run                                                                                                                                                                                              |
| `GET /api/v1/replication/jobs/{id}/deletions` | Deletion dry run                                                                                                                                                                                           |
| `PATCH /api/v1/replication/jobs/{id}`         | `mode`, `interval_seconds`, `filters`, `include_cached`, `deletion_policy`, `destination_read_only`, `enabled`                                                                                             |
| `PATCH /api/v1/replication/peers/{id}`        | `url`, `ca_ref`, token rotation                                                                                                                                                                            |

Healthy: `phase=incremental`, `changes_behind=0`, `consecutive_failures=0` and `last_success_at` within a few intervals. Failures retry with capped exponential backoff. Metrics on the destination's `/metrics` (label `job`): `ant_replication_lag_seconds`, `ant_replication_changes_behind`, `ant_replication_seconds_since_last_success`, `ant_replication_consecutive_failures`, `ant_replication_failures_total`, `ant_replication_bytes_transferred_total`, `ant_replication_blobs_deduplicated_total`, `ant_replication_reseeds_total`, `ant_replication_backfills_pending`, `ant_replication_conflicts_total` and more; see [Observability](../operations/observability.md). `lag_seconds` grows from the last success while the peer is unreachable.

Deleting a job keeps the replicated content attached and served, but renames its `replica:<name>` source to `replica-retired:<name>-<id>` so the name can be reused. Remove the retired source with `DELETE /api/v1/sources/{id}` (`force=true` detaches it and removes its content).

### Token rotation

1. On the authority: issue a new token for the replication principal (same scopes, new `expires_at`).
2. On the destination: `PATCH /api/v1/replication/peers/{id}` with `{"token":"<new>"}` and `If-Match: "N"`.
3. Confirm the job status, then revoke the old token on the authority with `DELETE /api/v1/tokens/{id}`.

Alert on `ant_replication_consecutive_failures` so an expired token is noticed.

## How reads are resolved across sites

Replication does not route requests between sites. Each site answers its own clients from its own feeds:

- The destination feed's [read order](../concepts/resolution.md) applies: its own `local` and `remote` sources first, the replica source last. A site can keep its own pull-through caches next to the replica.
- Content published directly on a destination (when it is not read-only) is local to that site, never replicated, and shadows the replica of the same coordinate.
- Clients choose a site by URL. Point each site's clients (runners, nodes, developers) at their nearest instance, with DNS or an external load balancer if you want one name.
- Publishes go to the authority. A read-only destination answers `409` naming the peer URL.

Cross-site reads of content a site does not hold (proxying or redirecting to the member that has it) are part of [federation](federation.md#placement-aware-routing).

## Multi-site topologies

### Hub and spokes

One instance is the authority for the shared, hosted feeds; each other site runs one continuous job per shared feed, with the same slug on every site.

- `include_cached=false`: each site keeps its own pull-through caches instead of copying the hub's.
- `destination_read_only=true`: the spokes reject publishes to shared feeds.
- `deletion_policy=propagate` after a dry run showed only the intended deletions.
- Spoke-local feeds (remote mirrors, team feeds) are ordinary feeds that are not replicated.

### Network direction

Replication is pull-based, so the only required path is **destination to authority** on 443. The reverse path is not needed. If the authority is publicly reachable with a public certificate, no CA configuration is needed on the destinations. A destination behind NAT or on a private network with a private CA works as long as it can reach the authority.

For a feed whose authority is the _other_ site, that site creates its own peer and job: the private site's name must resolve from the public site, its CA must be registered in `ANT_UPSTREAM_CA_MAP_FILE` there, and a peer plus job are created on the public site. There is no push mode.

### Several authorities

Authority is per feed. Site A can be authoritative for `npm` and site B for `maven`, with each site running jobs for the other's feeds. Avoid two instances publishing to the same coordinates of the same logical feed: there is no merge, and an instance's own higher-precedence sources always shadow its replica.

### Credentials

Use one replication principal per destination on the authority, granted `read` and `replicate` on exactly the replicated feeds, with a token that expires (at most one year) and is rotated before expiry. Adding a feed means: create the feed on both sites, add the grant and re-issue the token with the new scope, update the peer, then create the job.

### Outages

- **Authority outage.** Destinations keep serving everything already replicated; jobs back off and resume from their checkpoint.
- **Long outage.** If a destination lags past the authority's change history, the job reseeds automatically; deletions during the gap appear as held deletions in the dry run.
- **Pause.** `PATCH` the job with `enabled=false`.

### Smoke test

1. Publish a test npm package and a small OCI image to the authority.
2. Within one interval, `npm view` and `crane digest` against the destination return them.
3. Delete them on the authority; with `propagate` they disappear on the destination within one interval (`deletions_applied` increments).

### Client addresses behind an ingress

Failed-authentication throttling keys on the client address. Behind an ingress controller or gateway, set `ANT_TRUSTED_PROXIES` (Helm `trustedProxies`) to the proxy pod CIDRs on every site; see [Ingress, Gateway API and TLS](../install/ingress-tls.md).

## Limits

::: warning Limits

- The history purge has one low-water mark per instance, not per consumer.
- Reseed detects vanished items for assets only; vanished tags and package metadata are not tracked.
- Detaching a source from the authority's feed is not recorded or propagated.
- Resumable blob transfer needs local-disk storage on the destination; S3 restarts a failed blob from zero.
- Templates cover retention and remote-source cache/timeout tunables only; publish policy, grants, identities and endpoints are excluded, and remote sources are never created from a template.
- Unreferenced replicated blobs after a delete are left to retention and GC.
- Replication is implemented in the format-agnostic artifact core; the repository's live multi-site checks cover npm, OCI and the other hosted feeds of its own deployment. Per-format replica behaviour is not separately documented for every adapter.
  :::
