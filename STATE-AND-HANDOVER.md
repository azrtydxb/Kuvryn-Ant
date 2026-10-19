# Kuvryn Ant: state and handover

As of 2026-10-19, `main` at `6c81cfe` (PR #287). Everything described here is merged; there is no uncommitted or
branch-only work. The GitHub backlog (#12 to #219) was reconciled against code and evidence on the same date: each
issue carries a "Backlog sync 2026-10-19" comment, finished issues are closed with links to PRs and evidence, and
partly finished ones are open with the `partial` label and a list of what remains.

## 1. Implemented scope

**Phase 1 platform** (PRs #223 to #267, fixes #271, #279, #280, #282 to #287)

- Go service on PostgreSQL (migrations up to 040), disk and S3 blob storage with multipart resume, disk/S3 migration,
  writer lease, and two-phase GC, retention and integrity scans.
- All 26 format adapters mounted under `/feeds/{slug}/<format>/` (OCI under `/v2/` too). Remote pull-through works for
  22 of them; apk, git-lfs, p2 and swift serve local content only.
- Feed read policies with explain (`GET /feeds/{id}/resolve`), publish policies, policy-gated delete and unlist,
  rate limits, quotas and policy templates.
- Management API `/api/v1` (OpenAPI, generated Go and TS clients, idempotency keys, `If-Match`, transactional audit and
  outbox), webhooks, MCP at `/mcp` with API parity, audit sink and retention, tracing and diagnostics bundle.
- Identity: scoped service tokens, PATs, GitHub Actions OIDC exchange, OIDC/SAML/LDAP browser login, provider
  administration with diagnostics and preview, OIDC claim-to-role mapping with JIT provisioning (#287), local user
  accounts and username/password sign-in (#286), break-glass account and SSO-mandatory policy, envelope-encrypted
  secrets with resumable root-key rotation.
- Replication: peers, manifests, seed, incremental, continuous jobs, filters, deletion dry runs, reseed and back-fill,
  read-only replicas.
- Adoption and operations: import/export bundles, Nexus 3 mapping with dry run and `ant compare`, `ant backup` /
  `restore` (including to object storage, #285), metadata repair, Helm chart and Compose.
- React console (`web/`, embedded from `internal/console/dist`) covering every area above.

**Phase 2 federation, P2-M1 to P2-M6** (#259, #265, #268, #266, #272, #274; security hardening #270, #277;
qualification #276): membership and signed documents, shared identity and policy, coordinated replication with
in-place adoption of Phase 1 jobs, federated search, placement-aware routing (proxy, redirect, signed blob URLs),
authority handover and recovery, federated audit, runbooks. Design and per-milestone deviations are in
`docs/planning/phase-2-federation.md`.

## 2. Deployments

|                                       | OKE                                                              | kw                                                                                                     |
| ------------------------------------- | ---------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| URL                                   | `https://ant.azrty.com` (public certificate)                     | `https://ant.kw.watteel.lab` (private CA `cluster-ca`)                                                 |
| Role                                  | Federation authority; home of the nine shared feeds              | Federation member (`site=kw`); read-only replica of the shared feeds, plus its own pull-through caches |
| kube context / namespace              | `azrty-oke` / `kuvryn-ant`, Helm release `ant` (revision 24)     | `kw` / `kuvryn-ant`, Helm release `ant` (revision 20)                                                  |
| Image                                 | `dxb.ocir.io/axmcgpqk0w0z/azrty/kuvryn-ant`                      | `192.168.10.131/azrtydxb/kuvryn-ant`                                                                   |
| Running digest (2026-10-19 13:14 +04) | `sha256:a89c1dacfdf3...`                                         | same digest                                                                                            |
| Storage                               | OCI Object Storage (S3 compatibility), CloudNativePG 3 instances | TrueNAS MinIO, CloudNativePG 2 instances                                                               |
| Values                                | `deploy/examples/azrty-oke.yaml`                                 | `deploy/examples/kw.yaml`, `deploy/examples/kw-federation-lan.yaml`                                    |

- Data plane on OKE goes through the shared Envoy Gateway `edge` (HTTPRoute; `/` redirects to `/console/`, #283).
  Federation traffic uses TLS passthrough on port 8444 of the shared edge gateway (chart TLSRoute, #281). kw exposes
  its federation listener on the LAN (`192.168.10.150:8444`); OKE reaches it through `hostAliases`.
- Shared feeds (`oci-hosted npm go cargo pypi maven helm nuget sccache`) are homed on OKE and replicated to kw by the
  adopted `oke-*` federation jobs (in sync at the end of qualification, `evidence/2026-10-19/federation/13-final-state.md`).
- Node container mirrors on both clusters pull through Ant (`ANT_OCI_MIRROR_MAP`: docker.io, ghcr.io, quay.io,
  registry.k8s.io, gcr.io, public.ecr.aws).
- GitHub Actions ARC runners are unified on Ant and configure clients with `azrtydxb/ant-setup-action`.
- Console sign-in: Zitadel (OIDC, claim-to-role mapping) plus username/password local accounts; API tokens behind
  "Use an API token". Break-glass procedure: `docs/operations/break-glass.md`.
- Upgrades: verified `ant backup` on both sites first, then
  `helm upgrade --reset-then-reuse-values --set image.tag=... --set image.digest=...` (`docs/operations/upgrade.md`,
  `docs/operations/multi-site.md`).

## 3. Evidence per acceptance criterion (spec §11)

| AC                                              | State                | Evidence                                                                                                                                      | Gap                                                                                                |
| ----------------------------------------------- | -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| 1 Conformance per ecosystem                     | Partial              | `conformance/results/{local,oke,kw}/2026-10-19` (harness #273): 16 formats pass on all three targets                                          | Failures in 10 formats fixed in #280 but not re-run (#288)                                         |
| 2 Local/remote/routing/same-endpoint            | Partial              | Same runs: publish, consume, same-endpoint, remote miss and upstream-down hit per format                                                      | apk, git-lfs, p2, swift have no remote; pending re-run                                             |
| 3 Identity (Zitadel + generic OIDC, SAML, LDAP) | Partial              | Unit/integration tests in `internal/{oidclogin,samllogin,directory}`; Zitadel used in production                                              | No recorded real-provider evidence; SAML SLO unrouted; LDAP group mapping not applied (#46 to #50) |
| 4 UI/API/MCP authz + audit parity               | Partial              | `internal/mcp/parity_test.go`, management deny/audit tests                                                                                    | MCP lacks identity-admin tools; no UI parity tests (#65)                                           |
| 5 Two-instance replication                      | Pass with follow-ups | `evidence/2026-10-18` AC5 (seed, interrupt/resume, digests, lag, corruption, outage); federation adoption in `evidence/2026-10-19/federation` | Re-run with large blobs after #271/#279; disk-to-S3 pair (#68)                                     |
| 6 Disk vs S3 equivalence                        | Pass                 | `evidence/2026-10-18/ac6/*.json`: 46/46 steps on disk, MinIO, OCI Object Storage                                                              | Throttling/visibility tests (#29)                                                                  |
| 7 Import/export inventory and dry run           | Partial              | #260 tests (synthetic Nexus fixtures)                                                                                                         | No real Nexus run (#83, #290)                                                                      |
| 8 Performance                                   | Partial              | `evidence/2026-10-18` AC8, `bench/`                                                                                                           | Targets missed or not run (#289)                                                                   |
| 9 Backup/restore, upgrade, recovery             | Partial              | `evidence/2026-10-18/ac9`; restore trigger fix #279, object-storage backups #285                                                              | Restore and rollback not re-run since the fixes (#76, #77)                                         |
| 10 No claim without evidence + limitations      | Partial              | Per-adapter READMEs, harness `unsupported` records                                                                                            | Release evidence and known-limitations doc not yet published (#219)                                |

The 2026-10-18 qualification defects 1 to 11 were fixed in #271 and #279 (export timeout, source and job deletion,
anonymous raw read, restore triggers, serving after bootstrap, raw spool, lag reporting). Defects 12 (publish
throughput), 13 (data-plane metrics labelled `unmatched`, no upstream/cache metrics) and 14 (RSS after seeds) remain.

## 4. Known gaps

- Fresh 26-format conformance run after #280 (#288); swift native client aborts on the https targets (#204).
- Remote sources for apk, git-lfs, p2 and swift (#120, #155, #180, #205); npm mutable dist-tags (#169).
- Per-format replica integrity checks: only raw and OCI were verified by AC5 (all `-T4` issues).
- Prewarm jobs and schedules do not exist (#43); cache effectiveness reporting is incomplete (#44).
- Observability: data-plane route labels and upstream/cache metrics (#22); failure drills (#23).
- Identity: real-provider evidence for Zitadel, a second OIDC provider, SAML and LDAP; SAML single logout route; LDAP
  group-to-role mapping; group management (#46 to #50, #61).
- Console accessibility audit and capacity/freshness on the overview (#55, #56).
- AC8 targets (#289); restore throughput and re-runs of restore/upgrade/rollback (#76, #77).
- Nexus migration and decommissioning (#290).
- kw-authoritative feeds / push replication (#291); federation client conformance profile (#293).
- Owner decision pending: kw ingress `externalTrafficPolicy` (#292).
- License remains undecided by owner direction.

## 5. Runbooks and references

| Topic                                              | Document                                                                     |
| -------------------------------------------------- | ---------------------------------------------------------------------------- |
| Deployment and chart values                        | `deploy/README.md`, `deploy/examples/`                                       |
| Upgrade and rollback                               | `docs/operations/upgrade.md`                                                 |
| Backup and restore                                 | `docs/operations/backup-restore.md`                                          |
| Storage migration (disk/S3)                        | `docs/operations/storage-migration.md`                                       |
| Import, export, Nexus migration                    | `docs/operations/import-export-migration.md`                                 |
| Observability, diagnostics                         | `docs/operations/observability.md`                                           |
| Break-glass access                                 | `docs/operations/break-glass.md`                                             |
| OKE/kw topology, replication, credentials rotation | `docs/operations/multi-site.md`                                              |
| Federation setup and procedures                    | `docs/operations/federation.md`, `docs/operations/federation-runbooks.md`    |
| Client setup                                       | `docs/clients/` (docker, go, cargo, pnpm) and the console feed view          |
| Conformance harness                                | `conformance/README.md` (`conformance/run.sh <local\|oke\|kw> [formats...]`) |
| Benchmarks                                         | `bench/README.md`                                                            |
| Evidence sessions                                  | `evidence/README.md`, `docs/implementation/federation-evidence.md`           |

## 6. Credential locations (paths only)

All under `~/.kuvryn-ant/` on the operator workstation, never committed: `azrty-oke-root.key`, `kw-root.key`
(instance root keys); `azrty-oke-s3.json`, `kw-s3.json` (object storage); `azrty-oke-bootstrap.txt`,
`kw-bootstrap.txt`; `azrty-oke-automation-token.txt`, `kw-automation-token.txt`; `azrty-oke-console-admin.txt`,
`kw-console-admin.txt`; `federation-recovery.key` (offline federation recovery key); `truenas-minio-admin.txt`;
`backup-manifests.txt` (backup manifest digests). In-cluster: Secrets `ant-root-key`, `ant-s3`, `ant-postgres`,
`ant-pg-rootcert` in namespace `kuvryn-ant` on each cluster. The kw peer token for OKE is stored encrypted in Ant
(see `docs/operations/multi-site.md`, expiry 2027-10-13).

## 7. Next work

1. Deploy main to both sites and run the fresh 26-format conformance (#288); close the format T2/T3 issues it clears.
2. AC8: fix the publish throughput bottleneck and re-run the benchmarks, including W2 1M-asset and W3 10 GiB (#289).
3. Re-run restore, upgrade and rollback on kw without workarounds; write RTO/RPO (#76, #77).
4. Real-provider identity evidence (Zitadel plus one more OIDC provider, SAML, LDAP) and the SAML SLO and LDAP group
   mapping gaps (#45 to #50).
5. Remote support for apk, git-lfs, p2, swift and npm mutable dist-tags.
6. Prewarm (#43) and cache/upstream metrics (#22, #44).
7. Nexus migration and decommissioning (#290); kw-owned feeds (#291); owner decision on #292.

Delivery rules are unchanged: branch, verify locally (build, vet, touched tests, `scripts/check-generated.sh`), PR,
`gh pr merge --merge --admin` (CI bypassed by owner decision, AGENTS.md).
