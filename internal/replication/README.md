# internal/replication

Independent-instance replication (spec 6.7, architecture A09, delivery M07).
One instance is the **authority** for a feed; any number of other instances pull
that feed into a destination feed of their own. There is no multi-writer merge,
no shared database and no federation service: a destination that is fully seeded
keeps serving local clients when the source is down.

## Source side (authority)

Every change to assets, tags, artifact subjects (indexes) and component metadata
is recorded by database triggers (migration `020_replication.sql`) in
`replication_changes`. Sequence numbers are assigned by a deferred constraint
trigger at commit time under a per-instance head-row lock, so the sequence is
**durable, gapless and in commit order** and a reader that has seen N can never
later see a lower number. Aborted transactions consume nothing. Replica-kind
sources are never recorded or exported (an authority exports only its own
content: `local`, plus cached pull-through `remote` content).

Peer-facing API, all requiring a bearer token that is authorized for the
`replicate` action on the feed (role grant on the feed resource **and** token
scope):

| Route                                                                                                        | Purpose                                                                                                                               |
| ------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| `GET /api/v1/replication/export/feeds/{feed}/head`                                                           | feed format, change high-water mark                                                                                                   |
| `GET .../manifest?kind=asset\|tag&after=&limit=&include_cached=&namespace=&package=&version_prefix=&source=` | current-state manifest, keyset paged; `source` restricts it to one attached source (back-fill)                                        |
| `GET .../changes?after=&limit=&include_cached=&namespace=&package=&version_prefix=`                          | ordered publish / metadata / tombstone / attachment changes, filtered on the server; **410** when `after` is below the low-water mark |
| `GET .../blobs/{digest}`                                                                                     | blob by digest (single `Range` supported)                                                                                             |
| `GET .../template`                                                                                           | non-secret feed configuration template (see below)                                                                                    |

A blob is served only if an exportable asset of that feed references it.
Tombstones carry identity only; puts carry the item state at export time.

### Change-history retention and the low-water mark

The change log is bounded. A background purge (under the instance writer lease)
trims `replication_changes` to the more restrictive of a maximum age and a
maximum row count (`ANT_REPLICATION_HISTORY_MAX_AGE`, default 336h;
`ANT_REPLICATION_HISTORY_MAX_ROWS`, default 10,000,000;
`ANT_REPLICATION_PURGE_INTERVAL`, default 1h, 0 disables). The age bound never
removes a row younger than the cutoff. The purge first **raises and commits the
low-water mark** (`replication_heads.low_water`, monotonic, reported by `head`
and every change page) and only then removes rows, so a reader can never see a
hole it was not told about. A cursor below the mark is answered with 410 and
`X-Replication-Low-Water`; a cursor at the mark is served.

### Source attachments

Attaching a source that already holds content to a feed changes what the feed
exports without touching any asset row, so the attachment is recorded as an
`attachment` change (empty sources record nothing; detaching is not recorded).

## Destination side

- **Peers** (`replication_peers`): HTTPS URL (plain HTTP only for loopback), an
  optional operator-configured CA reference (`ANT_UPSTREAM_CA_MAP_FILE` IDs, never
  paths) and a per-peer token stored through `internal/secrets` with a purpose
  bound to the peer. Tokens are never returned. Redirects are never followed.
- **Jobs** (`replication_jobs`): one per destination feed. Creating a job attaches
  a new `replica`-kind source at the lowest precedence of the destination feed.
  Replica sources cannot be published to. The job option `destination_read_only`
  (default true for new jobs) additionally makes the destination feed reject
  publishes with 409 `replica feed — publish to <peer URL>` (OCI and npm show the
  text; other adapters return a plain 409) unless an explicit publish rule routes
  the request to a separate local source (the default route never qualifies);
  destination policy, credentials and identity settings are never touched.
- **Seed**: records the peer head, pages the asset manifest, then the tag
  manifest, then switches to incremental from the recorded head. Cached
  pull-through content is included by default (`include_cached=false` excludes it).
- **Incremental**: `continuous` (poll), `scheduled` (interval) or `manual`
  (trigger). Changes are applied in sequence order.
- **Reseed**: when the peer's low-water mark is above the job's checkpoint (seen
  on the head, or via a 410), the job restarts the manifest walk automatically
  (`phase` returns to `seed_assets`, `reseeding=true`, `reseeds` increments). It
  is the seed protocol again and as restartable: the cursor commits per page and
  the old checkpoint stays until the walk completes. It only upserts, never
  deletes; blobs already present are deduplicated, not fetched. When the walk
  finishes, replicated assets of this job the peer no longer lists (their
  stamped `seed_generation` is older) were deleted during the purged window and
  are recorded as **held deletions**, exactly as an `ignore`-policy tombstone:
  the dry run lists them and the deletion policy decides. Destination-only
  content is never touched.
- **Back-fill**: an `attachment` change queues the attached source in
  `replication_backfills` in the same transaction that advances the checkpoint.
  Before the next change page the job walks the manifest restricted to that
  source (assets, then tags) with its own durable cursor, so a restart resumes
  where it stopped. Cached (`remote`) sources are skipped when
  `include_cached=false`; flipping it later does not back-fill by itself.
- **Resumable blob transfer**: stores that implement `blob.Resumable` (local
  disk) keep a partial transfer in a private staging file named by digest. The
  next attempt re-hashes it and continues with `Range: bytes=<offset>-`, so
  completed bytes are not fetched again; the full digest and size are verified
  at the end, a corrupt partial is discarded (`ErrCorrupt`), a peer that ignores
  `Range` restarts from zero, and abandoned partials are swept after 7 days.
  Status shows `resumed_bytes`. S3 stores fall back to a plain restart.
- **Selective sync**: filters on namespace, package (exact) and version prefix
  apply to seed and incrementals and are evaluated on the peer for both the
  manifest and change pages (tombstones of packages that no longer exist cannot
  be resolved to a namespace and are passed through; consumers only delete what
  they replicated). Filter changes only affect future changes.
- **Durability and idempotency**: every page (metadata, cursor/checkpoint and
  counters) commits in one transaction fenced by the instance writer lease and
  the job claim. A restart resumes at the last committed cursor. Applying a
  change twice is harmless.
- **Dedup and verification**: a blob already present in the replica store (digest
  and size) is never fetched again, which also covers blobs stored by an
  interrupted page. Arriving bytes are staged and finalized against the manifest
  digest and size; mismatch is `ErrCorrupt`, nothing is stored, no metadata
  commits and the checkpoint does not move.
- **Deletions** are a per-job policy: `ignore` (default; tombstones are recorded
  as held) or `propagate`. Deletes only touch rows in the job's own replica source
  that carry this job's provenance; anything else is classified `protected`.
  `GET .../deletions` is the dry run: it lists held and not-yet-applied
  tombstones and what each would do, and changes nothing.
- **Conflicts**: the authority wins over its own earlier replicas. An item from a
  higher-precedence origin source, or destination-only content inside the replica
  source, is never overwritten (counted in `conflicts`).
- **Status**: phase, seed progress, checkpoint, remote head, changes behind, lag
  seconds, last attempt/success, next run, last error, consecutive and total
  failures, reseeds, the peer's low-water mark, pending/completed back-fills,
  resumed bytes and transfer/dedup/deletion/conflict counters. Failures retry
  with capped exponential backoff.
- **Metrics** (`/metrics`, read from the durable job rows at scrape time, label
  `job` only): `ant_replication_lag_seconds`, `_changes_behind`,
  `_seconds_since_last_success`, `_running`, `_consecutive_failures`,
  `_failures_total`, `_bytes_transferred_total`, `_blobs_transferred_total`,
  `_blobs_deduplicated_total`, `_resumed_bytes_total`, `_reseeds_total`,
  `_backfills_pending`, `_conflicts_total`, plus the process-wide
  `ant_replication_transfer_bytes_total`.

### Configuration templates

Content sync and feed configuration are separate (spec 6.7). The source can
export a reviewed, non-secret template of a feed: the feed-level retention
policy (without `allow_release_delete`), the cache/timeout tunables of its
remote sources (matched by name) and the source layout (review only). It never
contains endpoints/origins, credentials, CA references, identity or access
policy (grants, anonymous read, publish policy). On the destination,
`POST /jobs/{id}/template/preview` returns a diff (supplied template, or fetched
from the job's peer) with every entry classified `add`, `change`, `unchanged`,
`preserved` (destination-specific, never written), `blocked` or `info`, and a
digest binding template and diff. `POST /jobs/{id}/template/apply` requires that
digest (and an Idempotency-Key): the plan is recomputed under the feed lock and
nothing is written if it differs (409) or if anything is blocked; otherwise
retention and remote-source tunables are written in one transaction and audited.
Configure permission on the destination feed (and its remote sources) is
required.

## Management API (destination)

Peers need instance `admin`; jobs are authorized on the destination feed
(`configure` to change, `replicate` to run or inspect).

```
GET/POST        /api/v1/replication/peers
GET/PATCH/DELETE /api/v1/replication/peers/{id}        (PATCH: url, ca_ref, token rotation; revision CAS via If-Match "N" or body revision)
GET/POST        /api/v1/replication/jobs
GET/PATCH/DELETE /api/v1/replication/jobs/{id}         (PATCH: mode, interval, filters, include_cached, deletion_policy, destination_read_only, enabled)
GET             /api/v1/replication/jobs/{id}/status
POST            /api/v1/replication/jobs/{id}/runs     (manual trigger)
GET             /api/v1/replication/jobs/{id}/deletions (dry-run preview)
GET             /api/v1/replication/status             (all jobs)
POST            /api/v1/replication/jobs/{id}/template/preview
POST            /api/v1/replication/jobs/{id}/template/apply
```

**Idempotency**: every POST, PATCH and DELETE that changes state requires an
`Idempotency-Key` header (1-256 bytes), implemented with the shared
`database.ExecuteMutation` (24 hour window). A retry with the same key and
request replays the stored result (also for deletes of an already deleted job);
the same key with a different request is 409. Requests that carry a peer token
are fingerprinted with an HMAC keyed from the root key (never a plain hash of
the token) and responses never contain it. Authorization is checked on every
call, including replays.

## Federation mode (Phase 2, P2-M3)

When the instance is a federation member, `AttachFederation` (wired in
`cmd/ant/replication.go`) adds coordinated replication on top of the same jobs:

- **Federation-mode peers** (`auth_mode=federation`, `member_instance_id`) have
  no token. The client connects to the member's federation listener from the
  applied membership with mTLS (member certificate, server CN pinned to the
  member) and signs every request with the member key; the export API is
  served there as `/federation/v1/replication/export/feeds/{feed}/…`
  (`FederationExport`, same handlers and wire format). The home authorizes from
  the applied topology: the caller must be targeted by a rule for the feed's
  key, or be the other side of its transfer.
- **Reconciler** (`federated.go`, run by the federation worker under the writer
  lease): rule → destination feed (bound, or created from the descriptor) →
  federation peer → one job with `origin=federation`; rule changes patch the
  job; removed rules release it (disabled, `origin=local`); jobs whose feed
  changed home are re-pointed with a reseed; the transfer target promotes its
  replica source to local; the old home demotes (planned: its local source
  becomes the replica source, provenance re-stamped, manifest walk with blob
  dedup) or quarantines (forced). Existing matching jobs are **adopted in
  place**: checkpoint, phase, counters and replica source are kept.
- Federation-managed peers and jobs are read-only through this API (409
  `federation_managed`); runs can still be triggered.

See `docs/operations/federation.md` ("Coordinated replication").

## Wiring

`cmd/ant/replication.go` builds the runtime; `startWorkers` hosts `Service.Run`
under the instance lease context; `main.go` mounts both route sets.

## Limitations

- The history purge is global per instance (one low-water mark), not per
  consumer: a consumer that lags past the configured bounds reseeds. Size the
  bounds to the slowest peer you want to serve incrementally.
- Reseed detects vanished items for assets only; vanished tags and package
  metadata are not tracked (tags and components carry no provenance).
- Detaching a source from the source feed is not recorded or propagated.
  Flipping `include_cached` from false to true does not back-fill cached
  content seen earlier.
- Resumable transfer needs a store with resumable staging (local disk); S3
  stores restart a failed blob from byte zero.
- Templates cover retention and remote-source cache/timeout tunables only.
  Publish policy, grants, identities and endpoints are deliberately excluded,
  and remote sources are never created from a template.
- Unreferenced replicated blobs after a delete are left to retention/GC.
- Deleting a job keeps the replicated content attached and served, but renames
  its `replica:<name>` source to `replica-retired:<name>-<id>` so the job name
  can be reused. Remove the retired source with `DELETE /api/v1/sources/{id}`
  (`force=true` detaches it and removes its content).
