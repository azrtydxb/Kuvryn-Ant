# Roadmap

Where Kuvryn Ant stands and what remains before a first general release. This page summarizes the delivery plan and the current evidence; the maintained list is [ROADMAP.md on GitHub](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/ROADMAP.md), and work items are tracked as [GitHub issues](https://github.com/azrtydxb/Kuvryn-Ant/issues).

## Phases

| Phase                                                     | Scope                                                                                                                                                  | Status                                                                                          |
| --------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------- |
| Phase 0: product and format foundation                    | Product specification, architecture decisions, per-format contracts, backlog                                                                           | Complete                                                                                        |
| Phase 1: full breadth and independent-instance operations | All 26 formats, local publishing, remote caching, feed resolution and publish routing, disk and S3, identity, API/console/MCP, replication, operations | Implemented; release qualification in progress                                                  |
| Phase 2: federation                                       | Shared identity and policy, cross-instance search, placement-aware routing, coordinated replication, federation operations                             | Implemented (milestones P2-M1 to P2-M6) and qualified on a two-site deployment; follow-ups open |

No dates are committed. Version 0.1.0 has no published release image yet.

## Phase 1 milestones

| Milestone                                         | Outcome                                                                   | Status                                                                                                                                                                           |
| ------------------------------------------------- | ------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| M00 Planning and readiness                        | Traceable, executable backlog                                             | Complete                                                                                                                                                                         |
| M01 Shared platform and secure administration     | Go service, PostgreSQL metadata, authenticated and audited management API | Nearly complete; upstream and cache metrics, failure drills and worker tracing open                                                                                              |
| M02 Artifact integrity, storage, local publishing | Correct local lifecycle on disk and S3                                    | Partial; backend equivalence passes on disk, MinIO and Oracle Cloud Object Storage; disk-full and restart tests, S3 server-side encryption and ambient credentials open          |
| M03 Remote caching and build acceleration         | Predictable, observable pull-through                                      | Partial; coalesced misses proven (one upstream fetch per key) and prewarm delivered; upstream stall tests and cache-effectiveness reporting open                                 |
| M04 First ecosystem wave                          | OCI, Go, Helm, Cargo, Raw, OCI artifacts                                  | Real-client conformance passes locally and against two deployments; certification tasks (per-format replica integrity, failure cases) open                                       |
| M05 Remaining ecosystems                          | The other 20 formats                                                      | All adapters implemented with remote pull-through; a fresh full conformance run, the Swift native client and npm mutable dist-tags open                                          |
| M06 Enterprise identity and operator interfaces   | OIDC, SAML, LDAP, console, MCP                                            | Partial; break-glass, secret rotation, OIDC claim-to-role mapping and local accounts done; real-provider evidence, console accessibility audit and MCP identity-admin tools open |
| M07 Independent-instance replication              | Seed, incremental, read-only replicas                                     | Nearly complete; two-instance acceptance passed; disk-to-S3 pair and large-blob re-run open                                                                                      |
| M08 Adoption and operational lifecycle            | Import/export, migration, backup, upgrade, GC, repair                     | Partial; import/export done; restore and upgrade re-runs, restore throughput, production Compose and sizing guidance, real Nexus migration open                                  |
| M09 Phase 1 release qualification                 | Reproducible evidence for all ten acceptance criteria                     | Partial; see below                                                                                                                                                               |
| M10 Federation (Phase 2)                          | Federation design and implementation                                      | Implemented; federation client-conformance profile and push replication from a non-home site open                                                                                |

## Release acceptance criteria

The product specification defines ten acceptance criteria for a general release. Current state:

| Criterion                                             | State                | Remaining                                                                                            |
| ----------------------------------------------------- | -------------------- | ---------------------------------------------------------------------------------------------------- |
| 1. Client conformance per ecosystem                   | Partial              | 16 formats pass on all targets; fixes for 10 formats need a fresh run                                |
| 2. Local, remote, routing and same-endpoint behaviour | Partial              | Real-upstream runs for the apk, Git LFS, p2 and Swift remotes (tested against local fixtures so far) |
| 3. Identity (OIDC with two providers, SAML, LDAP)     | Partial              | Recorded real-provider evidence                                                                      |
| 4. UI, API and MCP authorization and audit parity     | Partial              | MCP identity-admin tools, UI parity tests                                                            |
| 5. Two-instance replication                           | Pass with follow-ups | Large-blob re-run, disk-to-S3 pair                                                                   |
| 6. Disk and S3 equivalence                            | Pass                 | Throttling and visibility tests                                                                      |
| 7. Import/export inventory and dry run                | Partial              | A run against a real Nexus instance                                                                  |
| 8. Performance                                        | Partial              | Publish throughput, large-scale (1M assets) and large-object (10 GiB) workloads                      |
| 9. Backup/restore, upgrade, recovery                  | Partial              | Re-run restore, upgrade and rollback; record RTO/RPO                                                 |
| 10. No claim without evidence, documented limitations | Partial              | Publish release evidence and a known-limitations document                                            |

## Known gaps

- A fresh conformance run across all 26 formats; the Swift native client fails against HTTPS targets.
- Real-client, real-upstream evidence for the apk, Git LFS, p2 and Swift remote sources.
- Per-format replica integrity checks beyond raw and OCI.
- Prewarm covers npm, Cargo, Go, PyPI, Maven, raw and OCI only; cache-effectiveness reporting is missing.
- Observability: upstream and cache metrics, failure drills, tracing of background workers.
- Identity: real-provider evidence for a second OIDC provider, SAML and LDAP; group management in the console.
- Console accessibility audit; capacity and freshness on the overview page.
- Performance targets not yet met or not yet measured; memory use after large seeds.
- Restore throughput and end-to-end rollback rehearsal.
- Nexus migration against a real instance, and decommissioning guidance.
- Federation: push replication from a non-home site, a federation profile in the client conformance harness, and an independent security review of the assertion, proxy and blob-URL code.
- Release engineering: published images, tagged releases and release notes.

## Next steps

1. Run the full 26-format conformance against the current code and close the format tasks it clears.
2. Fix the publish throughput bottleneck and re-run the benchmarks, including the large-scale and large-object workloads.
3. Re-run restore, upgrade and rollback without workarounds and document RTO and RPO.
4. Record real-provider identity evidence.
5. Run the apk, Git LFS, p2 and Swift remotes against their real upstreams.
6. Add upstream and cache metrics, and prewarm for more formats.
7. Qualify Nexus migration on a real instance; add push replication for site-owned feeds.

## Explicitly out of scope

From the product specification: build execution and runner orchestration, active-active multi-writer feeds, claiming a format as supported merely because it can store files, and (for now) SCIM provisioning and an embedded database.
