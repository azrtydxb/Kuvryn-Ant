# Planning index

Engineering planning is complete. Publication and readiness evidence are in [verification.json](verification.json) and [readiness review](readiness-review.md). No application code or test environments were built during planning. Implementation has since started; see [foundation evidence](../implementation/foundation.md). `verification.json` is the historical planning audit, not a current implementation-status report.

- [Delivery plan](delivery-plan.md) — scope, decisions and 11 milestone gates; [GitHub #1](https://github.com/azrtydxb/Kuvryn-Ant/issues/1).
- [Architecture decisions](architecture-baseline.md) — domain, storage, routing, identity, secrets, jobs and replication.
- [Compatibility baseline](compatibility-matrix.md) — OKE + existing MinIO on TrueNAS primary validation; generic AWS/other S3 compatibility without a provider test fleet.
- [Workloads and numerical release gates](release-workloads.md) — engineering targets, not measured results.
- [26 format contracts](contracts/README.md) and [ledger](format-capabilities.md); [GitHub #2](https://github.com/azrtydxb/Kuvryn-Ant/issues/2).
- [Backlog manifest](backlog.json) and [GitHub issue index](github-backlog.md) — 56 stories, 164 tasks, acceptance criteria and dependencies.
- [Phase 2 federation design](phase-2-federation.md) — M10 deliverable: model, threat model, failure modes, milestones P2-M1…P2-M6, work packages and open owner decisions.
- [Spec coverage](coverage.md) — functional requirements and all ten release acceptance criteria.

PostgreSQL only; license undecided; OCI/Docker → Go → Helm → Cargo → Raw first; all 26 formats in Phase 1; OIDC/SAML/LDAP in first GA. No calendar or staffing promises. Installed infrastructure versions and secrets are recorded when deployment tasks need them, without blocking core development.

GitHub body checklists and links provide the task hierarchy. No separate Project board is implied. The one-shot Python publishers that created the GitHub backlog were removed (planning is frozen; see `implementation-started.json`); finalized issue bodies/states are preserved in `planning-resolutions.json`. The recorded manifests are kept as history.
