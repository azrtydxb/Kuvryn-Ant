# Federation runbooks

Procedures for the failure modes of a [federation](federation.md). Each runbook names what stays up, how you notice, what to do and how to confirm. The data plane (serving, publishing, replication from checkpoints) never depends on federation: every runbook below is about the control plane.

API paths are relative to `/api/v1/federation` on the instance named in each step; all mutations need instance `admin` and, where marked, `"confirm": true`. The console (Federation page, "Authority" and "Federated audit" sections) and the MCP tools (`get_federation_authority`, `handover_federation_authority`, `federated_audit`, `explain_route`) use the same endpoints.

Recovery-key commands run **offline** (no database, no network) on the machine that holds the recovery key. The key is read from a file, never from the command line, and is never sent to a server:

| Command                                                                            | Purpose                                                                    |
| ---------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `ant federation recovery sign-authority-cert --template t.json --key recovery.key` | Certify the authority key of a forced recovery                             |
| `ant federation recovery sign-challenge --challenge c.json --key recovery.key`     | Recovery-key drill                                                         |
| `ant federation recovery sign-rotation` / `sign-authority-cert --rotation`         | Rotate the recovery key (see [Federation](federation.md#the-recovery-key)) |

## Alerts at a glance

Alerts appear in `GET /status` (`alerts[]`), as the metric `ant_federation_alert{code,severity}` and as the audit and webhook events `federation.alert.raised` / `federation.alert.cleared`. They mark health `degraded`, never unready.

| Alert                                  | Meaning                                                                          | Runbook                                      |
| -------------------------------------- | -------------------------------------------------------------------------------- | -------------------------------------------- |
| `membership_stale`                     | Membership not confirmed current for 15 min (critical after 24 h)                | Network partition, forced recovery           |
| `policy_stale`                         | Policy not confirmed current for 15 min (critical after 24 h)                    | Stale policy                                 |
| `policy_rejected`, `topology_rejected` | A document failed verification; the last good one is enforced                    | Stale policy                                 |
| `member_unreachable`                   | An active member has not answered a status ping for 15 min (critical after 24 h) | Member loss, network partition               |
| `authority_fenced`                     | This instance was the authority and learned of a higher epoch (7 days)           | Split brain                                  |
| `handover_pending`                     | A handover is in progress; publishing is paused (at most 10 minutes)             | Planned handover                             |
| `recovery_key_unverified`              | The recovery key was not proven in 180 days (30 days after founding if never)    | Recovery-key drill                           |
| `feed_home_conflict`                   | Content written on an old home after a forced transfer is quarantined            | [Home transfer](federation.md#home-transfer) |

## Planned handover

Use this when the current authority is healthy and you want to move the authority role to another active member, for example before decommissioning the authority's site.

Nothing secret is exported except the federation CA key: the target generates its own epoch key and the current authority certifies it with its epoch key. The CA key travels over the mutually authenticated channel because the new authority must keep issuing member certificates. The target also signs its own acceptance, so nobody can appoint a member that did not agree.

Preconditions: the target is an active member that has applied the latest membership, policy and topology (otherwise `409 stale_generation`; the call retries briefly itself), and no feed home transfer is in progress (`409 feed_transferring`).

1. On the authority:

   ```sh
   curl -fsS -X POST $A/api/v1/federation/authority/handover -H "$H" -H "$J" \
     -H 'Idempotency-Key: handover-1' -d '{"to_member_id":"<target member id>","confirm":true}'
   ```

   Publishing on the authority is paused until the new epoch is applied (alert `handover_pending`, at most 10 minutes).

2. The response reports `epoch` and whether the old authority applied the new document (`applied`). If `applied` is false, the next sync does.
3. Confirm: `GET /authority` on any member shows the new epoch with `certified_by: previous_epoch`. The old authority is a plain member, its authority and CA keys are deleted, and its API answers `409 not_authority` for authority operations. Members follow within one poll or nudge.
4. Outstanding invitations were revoked with the old authority: issue them again on the new one.

If the handover stalls (target down, process killed): wait for the pause to expire, or `POST /authority/handover/abort`. If the target had already published the new epoch, the old authority still applies it on its next sync and becomes a member (the highest valid epoch wins), so aborting is safe either way.

## Forced recovery

Use this when the authority is lost or compromised. It needs the offline recovery key. Run it on the member that takes over, usually the one with the best connectivity and the most complete replicas.

1. On that member: `POST /authority/recovery-template`. It generates the authority key of the next epoch and returns a template (`federation_id`, `generation`, the current recovery public key and the unsigned authority certificate). Save it as `template.json`. Repeating the call returns the same key.
2. Offline:

   ```sh
   ant federation recovery sign-authority-cert --template template.json --key recovery.key > request.json
   ```

   The command refuses a key that is not the current recovery key.

3. On that member: `POST /authority/recover` with the contents of `request.json` plus `"confirm": true`. Add `"remove_previous_authority": true` when the old authority is compromised: it is removed from the membership and its certificate revoked in the same document.
4. The member creates a **new federation CA** (the old CA key is gone with the old authority), publishes the first document of the next epoch signed by its new key and takes the role. Both CAs are trusted, so every existing member certificate keeps working; certificates issued from now on come from the new CA. The new authority keeps presenting its old listener certificate until it renews (members learn the new CA from the recovery document). Invitations created after recovery pin the CA of the listener certificate (`ca_sha256` in the invitation), which is what joiners need.
5. Members follow the new epoch from any member (sync asks a member that reports a higher epoch first, even while the old authority still answers). Documents of the lower epoch are rejected as `stale_epoch`.
6. Reissue invitations and re-check `GET /authority` (epoch history). Schedule the recovery-key drill, and rotate the recovery key if it may have been exposed.

Limits:

- A second recovery displaces the oldest CA; members still holding certificates from it must renew first or rejoin.
- A member that renewed its certificate through the lost authority during a partition (its new certificate is only in voided documents) is refused with `certificate_not_current` after recovery and must leave with `force` and rejoin.
- Policy or topology documents the old authority published at its own epoch after the fork are voided: republish them on the new authority.

## Split brain (two authorities)

Only a forced recovery while the old authority is alive but partitioned can create two authorities. Members accept the highest epoch with a valid chain; the old authority fences itself when it hears of it.

1. **Detect**: `authority_fenced` on the old authority, `federation.authority.fenced` in its audit log, and `fenced` in its `GET /authority`. The metric `ant_federation_authority_epoch` differs between members until they converge.
2. The old authority replaces the generations it issued after the fork with the new chain, deletes its authority and CA keys, revokes its pending invitations and becomes read-only for federation administration. Nothing merges automatically.
3. **Reconcile by hand**: list what was done on the old authority after the fork (`GET /api/v1/audit?prefix=federation.` on it, or the federated audit) and repeat on the new authority what is still wanted.

## Authority key compromise

Treat it as a forced recovery with `"remove_previous_authority": true`. Then:

- rotate the recovery key if it may be exposed;
- review the `federation.member.added` and `federation.certificate.issued` events of the compromised epoch for members you do not recognize, and remove them;
- let members renew their certificates (they do within a third of their 30-day life); the old CA stays trusted until a later recovery displaces it.

## Recovery-key drill

Proves, without changing anything, that the offline key still exists and works. Do it after founding, after every rotation and at least every 180 days.

1. On the authority: `POST /recovery-key/challenge` (one-time, valid for one hour). Save the response as `challenge.json`.
2. Offline:

   ```sh
   ant federation recovery sign-challenge --challenge challenge.json --key recovery.key > proof.json
   ```

3. On the authority: `POST /recovery-key/verify` with `proof.json`. `GET /authority` then shows `recovery.verified_at`. A wrong signature answers `400` and is audited (`federation.recovery_key.drill`, result `denied`); the challenge is consumed either way.

## Member loss

A member is down, destroyed or has lost its keys.

1. **Notice**: `member_unreachable`; `last_seen_at` and `rtt_ms` in `GET /members`; search results with that member `timeout` or `error`; routed reads skipping it (`GET /routes/{fed}/{slug}` shows its circuit).
2. **Data it homed**: federated feeds homed there stay readable from replicas (routing prefers healthy replicas); publishing to them stops. Decide whether to wait or to move the home with a **forced** transfer to a replica ([Home transfer](federation.md#home-transfer); it accepts data loss and quarantines whatever the old home accepted later).
3. **Not coming back**: `DELETE /members/{id}?confirm=true` on the authority. Its certificate serial is revoked and listed until it expires.
4. **Restored from a backup**: a restored copy must not reuse the member identity. Run `ant federation re-identify --confirm` on the copy (with the serving process stopped), then join it with a new invitation. A restored copy that reuses the old identity is refused by peers (certificate and key mismatch).
5. **Lost only its database but kept the root key**: re-identify, rejoin, rebind its feeds (`POST /feed-bindings`); replication rules re-create its replicas.

## Network partition

A member (or the authority) cannot reach the others. Serving, publishing to feeds it homes and reading what it replicated continue; federation documents freeze at the last applied generation.

1. **Notice**: `membership_stale` / `policy_stale` after 15 minutes, `member_unreachable` on the others, replication `lag_seconds` growing.
2. **Short partitions**: do nothing. Policy and membership stay enforced as last known good (never fail open; revocations are delayed by the staleness). `PUT /settings` with `policy_max_age_seconds` (member-local, off by default) denies federated principals beyond that age when a hard limit is wanted.
3. **Heal**: members fetch the missing document chain from the authority or any other member, apply it in order, and replication resumes from its checkpoints; alerts clear on the next evaluation.
4. **Long partition of the authority** when administration is needed: use a planned handover if the authority can still reach one member, otherwise a forced recovery (a fenced authority comes back read-only).

::: warning Partition drills with NetworkPolicy
When you simulate a partition with a Kubernetes NetworkPolicy that blocks egress to another member's federation address, allow in-cluster destinations explicitly (for example an egress rule with `namespaceSelector: {}`). Some CNIs, Cilium among them, do not match cluster endpoints with `ipBlock: 0.0.0.0/0`, so an `ipBlock`-only egress policy also cuts PostgreSQL and the pod crash-loops. This was observed during the project's own qualification.
:::

## Stale policy

`policy_stale` means this member has not confirmed for 15 minutes that its policy generation is the newest. The last good policy keeps being enforced.

1. Check `GET /status`: `policy.generation`, `age_seconds`, `staleness` and `sync.last_error`.
2. `policy_rejected` (with `federation.document.rejected` in the audit log): a forged, stale-epoch, gapped or tampered document was refused; the reason is in the audit event. If the authority is legitimate and this member's chain diverged, the member resyncs from the authority automatically. Otherwise check that the authority and the member agree on `GET /authority` (epoch) and `GET /documents/policy`.
3. If the authority cannot be reached at all, see Network partition. Revocations take effect only when the new policy is applied: for an urgent revocation of a federated principal on one member, disable the principal locally (`PATCH /api/v1/principals/{id}`); local deny roles beat federated allows.

## Interrupted or intercepted join

A join claims its invitation when the authority adds the member; the joiner's confirmation (signed with the member key it submitted, sent after it verified that the answer came from the authority) consumes it. A join that was relayed by another member, or interrupted before the confirmation, never confirms: the invitation shows `claimed` and is held for that instance for 10 minutes. Then the authority removes the provisional member, revokes its certificate (`federation.join.abandoned`) and the invitation can be used again. The legitimate joiner can simply retry with the same invitation. Pin the authority's certificate (`authority_cert_sha256` from the invitation) so an interceptor is refused before the secret is sent.

Mixed versions: a joiner that predates two-phase join never confirms and is removed after 10 minutes; upgrade it first.

## Re-identify and purge

Both run with the serving process stopped (they refuse while the writer lease is held):

- `ant federation re-identify --confirm` gives a restored copy of an instance a new instance ID (every column holding the old ID is rewritten in one transaction, encrypted secrets are re-sealed) and resets its federation state, so it can join with a new invitation. Update `ANT_INSTANCE_ID` if it is set.
- `ant federation purge --confirm` deletes all federation rows of an instance that left or was removed, so a binary downgrade sees no federation data. It is irreversible.

## Certificate revocation list

The membership document lists revoked certificate serials (limit 4096). The authority publishes a pruned list once a serial is older than 32 days (a member certificate lives 30 days), audited as `federation.certificate.revocations_pruned`. A publication that would exceed the limit prunes first and fails with `409` only if nothing has expired.

## Upgrades of federated sites

Federation does not change the upgrade procedure, but take and verify a backup on every site first (`ant backup`, then `ant backup verify`), upgrade one site at a time and let migrations run once per site. Members of different versions interoperate within the protocol range published in membership; see [Upgrade](../operations/upgrade.md) and [Backup and restore](../operations/backup-restore.md).

::: warning Gap
The authority handover, forced recovery, split-brain fencing, recovery-key drill, intercepted joins and revocation pruning procedures were qualified in the repository's multi-instance test harness only; the recovery key was not used on the project's live sites.
:::
