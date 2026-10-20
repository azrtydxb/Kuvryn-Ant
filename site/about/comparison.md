# Comparison

How Kuvryn Ant relates to the repository managers people usually evaluate it against: Sonatype Nexus Repository, JFrog Artifactory and Harbor. The comparison sticks to well-known, stable characteristics of those products. Their feature sets change between versions and editions, so cells marked "varies by edition" or "see vendor documentation" mean exactly that: check the vendor's current documentation before deciding.

::: info
Kuvryn Ant is at version 0.1.0 with no published release yet (see the [FAQ](./faq.md)). The other three products are mature and widely deployed. This page compares scope and design, not maturity.
:::

## At a glance

|                               | Kuvryn Ant                                                                  | Sonatype Nexus Repository                                           | JFrog Artifactory                                                                     | Harbor                                                              |
| ----------------------------- | --------------------------------------------------------------------------- | ------------------------------------------------------------------- | ------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| Focus                         | Universal artifact repository and caching proxy for build factories         | Universal repository manager                                        | Universal repository manager and DevOps platform component                            | Cloud-native registry for container images and OCI artifacts        |
| Package formats               | 26 ecosystems (see [Formats](../formats/index.md))                          | Many (Maven, npm, NuGet, Docker, PyPI, and more); varies by edition | Many; the free OSS edition is limited mainly to Java build formats; varies by edition | OCI only: images, Helm charts as OCI artifacts, other OCI artifacts |
| Pull-through caching          | Yes, for all 26 formats (remote sources)                                    | Yes (proxy repositories)                                            | Yes (remote repositories)                                                             | Yes (proxy cache projects for supported registries)                 |
| Aggregated endpoints          | Feeds with ordered sources and owner-first resolution                       | Group repositories                                                  | Virtual repositories                                                                  | Not applicable (per-project registries)                             |
| Write routing                 | Publish policies route each write to exactly one local source               | Group repositories with a writable member; varies by edition        | Virtual repositories with a default deployment repository                             | Push to a project                                                   |
| Replication between instances | Pull replication of feeds to read-only replicas; Phase 2 federation         | Varies by edition                                                   | Push/pull replication and federated repositories; varies by edition                   | Push and pull replication rules, including to other registry types  |
| Build caches                  | Cache feeds for BuildKit registry cache (OCI) and sccache over WebDAV (raw) | See vendor documentation                                            | See vendor documentation                                                              | OCI registry cache images can be stored like any OCI artifact       |
| Vulnerability scanning        | None built in; stores external scanner verdicts                             | Separate Sonatype products                                          | Separate JFrog product (Xray)                                                         | Built in, pluggable scanners (Trivy by default)                     |
| Browser identity              | OIDC, SAML, LDAP, local accounts, break-glass                               | LDAP and others; SAML varies by edition                             | LDAP, SAML, OIDC and others; varies by edition                                        | Local database, LDAP/AD, OIDC                                       |
| Automation API                | OpenAPI management API and MCP server                                       | REST API                                                            | REST API and CLI                                                                      | REST API                                                            |
| Metadata database             | PostgreSQL (required)                                                       | Embedded or external database depending on version and edition      | External database depending on version and edition                                    | PostgreSQL                                                          |
| Blob storage                  | Local disk or S3-compatible                                                 | File and cloud blob stores; varies by edition                       | File system and cloud object storage; varies by edition                               | File system, S3-compatible and other drivers                        |
| Licence                       | Apache 2.0, single edition                                                  | Commercial editions plus a free edition; varies by edition          | Commercial editions plus free OSS and container-registry editions                     | Apache 2.0 (CNCF graduated project)                                 |

## Where Kuvryn Ant differs

- **One core, many protocols.** Every format adapter publishes through the same staged, digest-verified, atomic publication core with audit in the same transaction, instead of per-format storage logic. See [Feeds and sources](../concepts/feeds-and-sources.md).
- **Owner-first read resolution.** A package owned by a local source can never be completed from a public upstream, and an upstream error does not fall through to a lower-priority public source. `GET /api/v1/feeds/{id}/resolve` explains every decision. See [Read order and resolve explain](../concepts/resolution.md).
- **Separate read and publish policies.** One feed URL serves both reads and publishes; each write resolves to exactly one local source, and publishes never go upstream. See [Publish policies](../concepts/publish-policies.md).
- **Build-factory features.** Prewarm from lockfiles and image lists, coalesced cache misses, stale-if-error metadata, cache feeds with idle/size eviction, and storage-pressure eviction. See [Build caches](../build-caches/index.md) and [Prewarm](../operations/prewarm.md).
- **API, console and MCP parity.** The console and the MCP server use the same management handlers and authorization as the API. See [MCP tools](../reference/mcp.md).
- **Phase 2 federation** groups independent instances with shared identity and policy, federated search, coordinated replication and placement-aware routing, without a shared database. See [Federation](../replication/federation.md).

## What Kuvryn Ant does not do

- **No vulnerability or licence scanning engine.** `POST /api/v1/assets/{id}/security-results` records the verdict of an external scanner (`passed`, `failed` or `warning` with a summary), stores it with the asset and emits an `asset.security_result` webhook event. The result is informational; nothing blocks downloads based on it.
- **No active-active clustering.** One writer per instance; scale out with replicas and federation. See the [FAQ](./faq.md#can-i-run-more-than-one-replica-for-high-availability).
- **No multi-writer feeds.** In replication and federation each feed has one authoritative home; there is no write merge.
- **No build execution.** It serves and caches artifacts for build systems; it does not run builds or orchestrate runners.
- **No SCIM provisioning.** Users and groups come from OIDC, SAML or LDAP sign-in and local accounts.
- **No embedded database.** PostgreSQL is required even for development.
- **Migration tooling only for Nexus Repository 3**, with content copy for raw and Maven layouts. See [Import, export, Nexus migration](../operations/import-export.md).
- **No vendor support or published releases yet.** Support is community-based through GitHub issues.
