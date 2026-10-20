# Planning index

Engineering planning is complete and implementation is well advanced: Phase 1 platform and all 26 format adapters,
plus Phase 2 federation P2-M1 to P2-M6, are merged and deployed on OKE and kw. Current state, deployments, evidence per
acceptance criterion and next work are in [STATE-AND-HANDOVER.md](../../STATE-AND-HANDOVER.md); milestone status is at
the top of each milestone in the [delivery plan](delivery-plan.md). The GitHub backlog was reconciled with code and
evidence on 2026-10-19: finished issues are closed with links to PRs and evidence, partly finished ones carry the
`partial` label and a comment listing what remains, and new gaps are #288 to #293.

Planning-time documents (historical where noted):

- [Delivery plan](delivery-plan.md) — scope, decisions, 11 milestone gates with current status; [GitHub #1](https://github.com/azrtydxb/Kuvryn-Ant/issues/1).
- [Architecture decisions](architecture-baseline.md) — domain, storage, routing, identity, secrets, jobs and replication.
- [Compatibility baseline](compatibility-matrix.md) — OKE + existing MinIO on TrueNAS primary validation; generic AWS/other S3 compatibility without a provider test fleet.
- [Workloads and numerical release gates](release-workloads.md) — targets; measured results are in [evidence/](../../evidence/README.md) (AC8 misses tracked in #289).
- [26 format contracts](contracts/README.md) and [ledger](format-capabilities.md) (revision 1, planning-time); real-client results are in [conformance/results/](../../conformance/results/); [GitHub #2](https://github.com/azrtydxb/Kuvryn-Ant/issues/2).
- [Backlog manifest](backlog.json) and [GitHub issue index](github-backlog.md) — 56 stories, 164 tasks as published; live state is on GitHub.
- [Phase 2 federation design](phase-2-federation.md) — model, threat model, milestones P2-M1…P2-M6 (all implemented) with deviations; owner decisions answered in `.procoder/ask/answers.md`.
- [Spec coverage](coverage.md) — functional requirements and all ten release acceptance criteria.
- [Verification](verification.json) and [readiness review](readiness-review.md) — historical planning audit, not implementation status.

PostgreSQL only; Apache-2.0 licensed (decided 2026-10-20); OCI/Docker → Go → Helm → Cargo → Raw first; all 26 formats in Phase 1; OIDC/SAML/LDAP in first GA. No calendar or staffing promises.

GitHub body checklists and links provide the task hierarchy. No separate Project board is implied. The one-shot Python publishers that created the GitHub backlog were removed (planning is frozen; see `implementation-started.json`); finalized issue bodies/states are preserved in `planning-resolutions.json`. The recorded manifests are kept as history.
