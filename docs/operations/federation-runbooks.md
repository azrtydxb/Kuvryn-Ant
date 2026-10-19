# Federation runbooks (P2-M6)

Procedures for the failure modes of `docs/planning/phase-2-federation.md` section 13. Each one names what stays up, how you notice, what to do and how to confirm. The data plane (serving, publishing, replication from checkpoints) never depends on federation: every runbook below is about the control plane.

API paths are `/api/v1/federation/...` on the instance named in each step; all mutations need instance `admin` and, where marked, `confirm: true`. The console (Federation page, "Authority" and "Federated audit" sections) and the MCP tools (`get_federation_authority`, `handover_federation_authority`, `federated_audit`, `explain_route`) use the same endpoints.

Recovery-key commands run **offline** (no database, no network) on the machine that holds the recovery key; the key is read from a file, never from the command line, and is never sent to a server:

| Command                                                                                       | Purpose                                                    |
| --------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `ant federation recovery sign-authority-cert --template t.json --key recovery.key`            | certify the authority key of a forced recovery             |
| `ant federation recovery sign-challenge --challenge c.json --key recovery.key`                | recovery-key drill                                         |
| `ant federation recovery sign-rotation` / `sign-authority-cert --rotation`                     | rotate the recovery key (see `federation.md`, Procedures)  |

## Alerts at a glance

`GET /api/v1/federation/status` (`alerts[]`), metric `ant_federation_alert{code,severity}`, audit events `federation.alert.raised|cleared`.

| Alert                     | Meaning                                                                              | Runbook                  |
| ------------------------- | ------------------------------------------------------------------------------------ | ------------------------ |
| `membership_stale`        | membership not confirmed current for 15 min (critical after 24 h)                    | Partition, authority loss |
| `policy_stale`            | policy not confirmed current for 15 min (critical after 24 h)                        | Stale policy             |
| `policy_rejected`, `topology_rejected` | a document failed verification; the last good one is enforced             | Stale policy             |
| `member_unreachable`      | an active member has not answered a status ping for 15 min (critical after 24 h)     | Member loss, partition   |
| `authority_fenced`        | this instance was the authority and learned of a higher epoch (7 days)               | Split brain              |
| `handover_pending`        | a handover is in progress; publishing is paused (at most 10 minutes)                 | Planned handover         |
| `recovery_key_unverified` | the recovery key was not proven in 180 days (30 days after founding if never)        | Recovery-key drill       |
| `feed_home_conflict`      | content written on an old home after a forced transfer is quarantined                | `federation.md`, transfer |

## Planned handover (the current authority is healthy)

Moves the authority role to another active member. Nothing is exported: the target generates its own epoch key and the current authority certifies it with its epoch key; only the federation CA key travels, over the mutually authenticated channel, because the new authority must keep issuing member certificates. The target also signs its own acceptance, so nobody can appoint a member that did not agree.

Preconditions: the target is an active member that has applied the latest membership, policy and topology (the call answers `409 stale_generation` and retries briefly itself), and no feed home transfer is in progress (`409 feed_transferring`).

1. On the authority: `POST /authority/handover {"to_member_id": "<id>", "confirm": true}`. Publishing on the authority is paused until the new epoch is applied (alert `handover_pending`, at most 10 minutes).
2. The response reports `epoch` and whether the old authority applied the new document (`applied`). If `applied` is false the next sync does.
3. Confirm: `GET /authority` on any member shows the new epoch with `certified_by: previous_epoch`; the old authority is a plain member, its authority and CA keys are deleted, and its API answers `409 not_authority` for authority operations. Members follow within one poll or nudge.
4. Outstanding invitations were revoked with the old authority: issue them again on the new one.

If the handover stalls (target down, process killed): wait for the pause to expire, or `POST /authority/handover/abort`. If the target had already published the new epoch, the old authority still applies it on its next sync and becomes a member (the highest valid epoch wins), so abort is safe either way.

## Forced recovery (the authority is lost or compromised)

Needs the offline recovery key. Run on the member that takes over (usually the one with the best connectivity and the most complete replicas).

1. `POST /authority/recovery-template` on that member. It generates the authority key of the next epoch and returns a template (`federation_id`, `generation`, the current recovery public key and the unsigned authority certificate). Repeating the call returns the same key.
2. Offline: `ant federation recovery sign-authority-cert --template template.json --key recovery.key > request.json`. The command refuses a key that is not the current recovery key.
3. `POST /authority/recover` on that member with the contents of `request.json` plus `"confirm": true`. Add `"remove_previous_authority": true` when the old authority is compromised: it is removed from the membership and its certificate revoked in the same document.
4. The member creates a **new federation CA** (the old CA key died with the old authority), publishes the first document of the next epoch signed by its new key and takes the role. Both CAs are trusted, so every existing member certificate keeps working; certificates issued from now on come from the new CA, and the authority keeps presenting its old listener certificate until it renews (members learn the new CA from the recovery document). Invitations created after recovery pin the CA of the listener certificate (`ca_sha256` in the invitation), which is what joiners need.
5. Members follow the new epoch from any member (sync asks a member that reports a higher epoch first, even while the old authority still answers). Documents of the lower epoch are rejected as `stale_epoch`.
6. Reissue invitations and re-check `GET /authority` (epoch history). Schedule the recovery-key drill and consider rotating the recovery key if it was exposed.

Limits to know: a second recovery displaces the oldest CA, and members still holding certificates from it must renew first or rejoin; a member that renewed its certificate through the lost authority during a partition (its new certificate is only in voided documents) is refused with `certificate_not_current` after recovery and must `leave --force` and rejoin; policy or topology documents the old authority published at its own epoch after the fork are voided, so republish them on the new authority.

## Split brain (two authorities)

Only a forced recovery while the old authority is alive and partitioned can create it. Members accept the highest epoch with a valid chain; the old authority fences itself when it hears of it.

1. Detect: `authority_fenced` on the old authority, `federation.authority.fenced` in its audit, and its `GET /authority` shows `fenced`. Metric `ant_federation_authority_epoch` differs between members until they converge.
2. The old authority replaces the generations it issued after the fork with the new chain, deletes its authority and CA keys, revokes its pending invitations and turns read-only for federation administration. Nothing merges automatically.
3. Reconcile by hand: list what was done on the old authority after the fork (`GET /audit?prefix=federation.` on it, or the federated audit) and repeat what is still wanted on the new authority.

## Authority key compromise

Treat as a forced recovery with `remove_previous_authority: true`. Then: rotate the recovery key if it may be exposed, review `federation.member.added` and `federation.certificate.issued` events of the compromised epoch for members you do not recognize and remove them, and re-issue member certificates by letting them renew (they do within a third of their 30-day life; the old CA stays trusted until a later recovery displaces it).

## Recovery-key drill

Proves, without changing anything, that the offline key still exists and works. Do it after founding, after every rotation and at least every 180 days.

1. On the authority: `POST /recovery-key/challenge` (one-time, valid for one hour) and save the response as `challenge.json`.
2. Offline: `ant federation recovery sign-challenge --challenge challenge.json --key recovery.key > proof.json`.
3. `POST /recovery-key/verify` with `proof.json`. `GET /authority` then shows `recovery.verified_at`; a wrong signature answers 400 and is audited (`federation.recovery_key.drill`, result `denied`); the challenge is consumed either way.

## Member loss

A member is down, destroyed or lost its keys.

1. Notice: `member_unreachable`, `ant_federation_member_up`-style pings in `GET /members` (`last_seen_at`, `rtt_ms`), search results with that member `timeout|error`, routed reads skipping it (`GET /routes/{fed}/{slug}` shows its circuit).
2. Data it homed: federated feeds homed there stay readable from replicas (routing prefers healthy replicas); publishing to them stops. Decide: wait, or move the home with a **forced** transfer to a replica (`federation.md`, Home transfer; accepts data loss and quarantines whatever the old home accepted later).
3. If it is not coming back: `DELETE /members/{id}?confirm=true` on the authority. Its certificate serial is revoked and listed until it expires (the list is pruned automatically once the certificate has expired, so it never reaches its limit).
4. If it comes back from a backup: a restored copy must not reuse the member identity. Run `ant federation re-identify --confirm` on the copy, then join it with a new invitation. A restored copy that already was a member is refused by peers (certificate and key mismatch).
5. A member that lost only its database but kept the root key: re-identify, rejoin, rebind its feeds (`POST /feed-bindings`); replication rules re-create its replicas.

## Network partition

A member (or the authority) cannot reach the others. Serving, publishing to feeds it homes and reading what it replicated continue; documents freeze at the last applied generation.

1. Notice: `membership_stale` / `policy_stale` after 15 minutes, `member_unreachable` on the others, replication `lag_seconds` growing.
2. Do nothing for short partitions. Policy and membership stay enforced as last known good (never fail open; revocations are delayed by the staleness). `PUT /settings` with `policy_max_age_seconds` (member-local, off by default) denies federated principals after that age when a hard limit is wanted.
3. Heal: members fetch the missing document chain (from the authority or any other member), apply it in order, and replication resumes from its checkpoints; alerts clear on the next evaluation.
4. If the partition isolates the authority for long and administration is needed, use planned handover when it can still reach one member, otherwise forced recovery (a fenced authority comes back read-only).

## Stale policy

`policy_stale` means this member has not confirmed its policy generation is the newest for 15 minutes; the last good policy keeps being enforced.

1. Check `GET /status`: `policy.generation`, `age_seconds`, `staleness`, and `sync.last_error`.
2. `policy_rejected` (with `federation.document.rejected` in audit): a forged, stale-epoch, gapped or tampered document was refused. The reason is in the audit event. If the authority is legitimate and the member's chain diverged, the member resyncs from the authority automatically; if not, check that the authority and the member agree on `GET /authority` (epoch) and `documents/policy`.
3. If the authority cannot be reached at all, see Partition. Revocations in the meantime take effect only when the new policy is applied: for an urgent revocation of a federated principal on one member, disable the principal locally (`PATCH /principals/{id}`); local deny roles beat federated allows.

## Interrupted or intercepted join

A join claims its invitation when the authority adds the member; the joiner's confirmation (signed with the member key it submitted, sent after it verified that the answer came from the authority itself) consumes it. A join that was relayed by another member, or interrupted before the confirmation, never confirms: the invitation shows `claimed` and is held for that instance for 10 minutes, then the authority removes the provisional member, revokes its certificate (`federation.join.abandoned`) and the invitation can be used again. The legitimate joiner may simply retry with the same invitation. Pin the authority's certificate (`authority_cert_sha256` in the invitation) to refuse an interceptor before the secret is sent.

Note for mixed versions: a joiner that predates two-phase join never confirms and is removed after 10 minutes; upgrade it first.

## Re-identify and purge

Unchanged: see `federation.md`, Offline maintenance (`ant federation re-identify --confirm`, `ant federation purge --confirm`).

## Certificate revocation list

The membership document lists revoked certificate serials (limit 4096). The authority publishes a pruned list once a serial is older than 32 days (a member certificate lives 30 days), audited as `federation.certificate.revocations_pruned`. A publication that would exceed the limit prunes first and fails with `409` only if nothing has expired.
