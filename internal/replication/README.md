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

| Route | Purpose |
|---|---|
| `GET /api/v1/replication/export/feeds/{feed}/head` | feed format, change high-water mark |
| `GET .../manifest?kind=asset\|tag&after=&limit=&include_cached=&namespace=&package=&version_prefix=` | current-state manifest, keyset paged |
| `GET .../changes?after=&limit=&include_cached=` | ordered publish / metadata / tombstone changes |
| `GET .../blobs/{digest}` | blob by digest (single `Range` supported) |

A blob is served only if an exportable asset of that feed references it.
Tombstones carry identity only; puts carry the item state at export time.

## Destination side

* **Peers** (`replication_peers`): HTTPS URL (plain HTTP only for loopback), an
  optional operator-configured CA reference (`ANT_UPSTREAM_CA_MAP_FILE` IDs, never
  paths) and a per-peer token stored through `internal/secrets` with a purpose
  bound to the peer. Tokens are never returned. Redirects are never followed.
* **Jobs** (`replication_jobs`): one per destination feed. Creating a job attaches
  a new `replica`-kind source at the lowest precedence of the destination feed.
  Replica sources cannot be published to, so replicas are read-only by default;
  destination policy, credentials and identity settings are never touched.
* **Seed**: records the peer head, pages the asset manifest, then the tag
  manifest, then switches to incremental from the recorded head. Cached
  pull-through content is included by default (`include_cached=false` excludes it).
* **Incremental**: `continuous` (poll), `scheduled` (interval) or `manual`
  (trigger). Changes are applied in sequence order.
* **Selective sync**: filters on namespace, package (exact) and version prefix
  apply to seed and incrementals. Filter changes only affect future changes.
* **Durability and idempotency**: every page (metadata, cursor/checkpoint and
  counters) commits in one transaction fenced by the instance writer lease and
  the job claim. A restart resumes at the last committed cursor. Applying a
  change twice is harmless.
* **Dedup and verification**: a blob already present in the replica store (digest
  and size) is never fetched again, which also covers blobs stored by an
  interrupted page. Arriving bytes are staged and finalized against the manifest
  digest and size; mismatch is `ErrCorrupt`, nothing is stored, no metadata
  commits and the checkpoint does not move.
* **Deletions** are a per-job policy: `ignore` (default; tombstones are recorded
  as held) or `propagate`. Deletes only touch rows in the job's own replica source
  that carry this job's provenance; anything else is classified `protected`.
  `GET .../deletions` is the dry run: it lists held and not-yet-applied
  tombstones and what each would do, and changes nothing.
* **Conflicts**: the authority wins over its own earlier replicas. An item from a
  higher-precedence origin source, or destination-only content inside the replica
  source, is never overwritten (counted in `conflicts`).
* **Status**: phase, seed progress, checkpoint, remote head, changes behind, lag
  seconds, last attempt/success, next run, last error, consecutive failures and
  transfer/dedup/deletion/conflict counters. Failures retry with capped
  exponential backoff.

## Management API (destination)

Peers need instance `admin`; jobs are authorized on the destination feed
(`configure` to change, `replicate` to run or inspect).

```
GET/POST        /api/v1/replication/peers
GET/PATCH/DELETE /api/v1/replication/peers/{id}        (PATCH: url, ca_ref, token rotation; revision CAS)
GET/POST        /api/v1/replication/jobs
GET/PATCH/DELETE /api/v1/replication/jobs/{id}         (PATCH: mode, interval, filters, include_cached, deletion_policy, enabled)
GET             /api/v1/replication/jobs/{id}/status
POST            /api/v1/replication/jobs/{id}/runs     (manual trigger)
GET             /api/v1/replication/jobs/{id}/deletions (dry-run preview)
GET             /api/v1/replication/status             (all jobs)
```

## Wiring

`cmd/ant/replication.go` builds the runtime; `startWorkers` hosts `Service.Run`
under the instance lease context; `main.go` mounts both route sets.

## Limitations

* Change history is not compacted; there is no purge/reseed protocol yet.
* A source attached to the source feed after the seed is not back-filled.
* Server-side filtering covers the manifest only; change pages are filtered by the destination.
* Blob transfers restart from byte zero (stores have no resumable staging).
* Unreferenced replicated blobs after a delete are left to retention/GC.
* Reviewed non-secret configuration templates are not implemented here.
* Management mutations are not idempotency-key cached; uniqueness constraints reject duplicates.
