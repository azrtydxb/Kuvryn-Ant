# Roadmap

This is a summary. The detailed plans live in [docs/planning](docs/planning/README.md)
and the documentation site has a [roadmap page](https://azrtydxb.github.io/Kuvryn-Ant/about/roadmap).
Priorities can change; open an issue to discuss them.

## Done (unreleased, on `main`)

- Artifact core with atomic, audited publication, read resolution, publish and read
  policies, deletes and pull-through caching.
- 26 package formats with publish, consume and remote pull-through (see the
  [format matrix](https://azrtydxb.github.io/Kuvryn-Ant/formats/)).
- Disk and S3-compatible blob storage with online migration between them.
- Identity: local users with TOTP, OIDC, SAML, LDAP, persisted roles and grants,
  scoped service tokens, personal access tokens and GitHub Actions OIDC exchange.
- Replication between instances and Phase 2 federation (signed membership, policy,
  placement-aware routing, authority handover and recovery).
- Retention, two-phase garbage collection, storage-pressure eviction, integrity
  scans, quotas and limits.
- Build-cache feeds for buildx registry caches and sccache over WebDAV.
- Backup and restore, Nexus migration, prewarm jobs, webhooks, audit sink, metrics,
  tracing and an MCP endpoint. React console embedded in the binary.

## Next

- First tagged release: published multi-architecture container image, Helm chart
  repository and release notes.
- Live-vendor qualification of identity providers and S3 providers beyond the ones
  used in development.
- Performance qualification for large objects and high concurrency.
- More real-client conformance coverage in CI.

## Later

- SCIM 2.0 user and group provisioning (allowed as a later release by the product
  specification).
- Multi-pod active-active serving of one instance. It is excluded until conflict
  semantics are designed; today one writer serves each instance and independent
  instances replicate or federate.
- Further formats requested by users (open a format-support request).
