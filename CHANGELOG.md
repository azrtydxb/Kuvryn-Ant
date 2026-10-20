# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

No version has been tagged yet. This section summarizes the work on `main` so far.

### Added

- **Foundation:** PostgreSQL service lifecycle with forward-only, checksummed,
  advisory-locked migrations; authenticated feed management; disk and S3 blob
  storage with digest and size verification; database writer lease per instance.
- **Artifact core:** staged, digest-verified, atomic publication with audit and
  outbox in one transaction; package discovery, artifact subjects and OCI
  referrers; policy-gated delete, unlist and relist for every format.
- **Formats:** adapters for OCI/Docker and OCI artifacts, Go modules with the
  checksum database, Helm, Cargo, Raw with WebDAV, npm, PyPI, Maven, NuGet,
  RubyGems, Composer, pub, Conan, Conda, CRAN, Ansible Galaxy, Git LFS, Terraform,
  Swift, CocoaPods, APT, RPM (signed atomic repodata generations), APK, P2 and
  Hugging Face models and datasets.
- **Pull-through caching:** remote-source configuration contract with encrypted
  per-source credentials and operator CA references; coalesced, verified,
  cached materialization; metadata and negative caches with stale-if-error;
  remote pull-through for all 26 formats; npm and pnpm audit forwarding; source
  health with format-aware probes.
- **Feeds and policies:** ordered read sources with owner-first resolution and a
  resolve explain endpoint; publish policies with prioritized selectors;
  `cache` feed purpose for buildx layer caches and sccache over WebDAV, with idle
  and size limits, tag-overwrite release and prefix cleanup.
- **Identity:** local accounts with TOTP, OIDC, SAML (including single logout) and
  LDAP sign-in; claim and group to role mapping with just-in-time provisioning;
  persisted roles and grants; scoped service tokens, personal access tokens and
  GitHub Actions OIDC exchange; break-glass account and SSO-mandatory policy;
  client authentication policies and network groups.
- **Limits:** rate limits, quotas and policy templates.
- **Replication:** gapless change sequence, peer pull with digest verification,
  per-page checkpoints, history retention with reseed, resumable blobs,
  server-side filters, configuration templates and read-only replica sources.
- **Federation (Phase 2):** signed membership documents and mTLS listener; shared
  identity and policy; coordinated replication; cross-instance search;
  placement-aware routing; authority handover, recovery key and federated audit.
- **Storage maintenance:** retention policies, two-phase mark and sweep garbage
  collection, integrity scans, instance default retention policy and
  storage-pressure eviction with watermarks; online disk and S3 migration.
- **Operations:** backup and restore to object storage; import and export bundles
  and Nexus migration; durable prewarm jobs and schedules; webhooks with
  redelivery; audit sink and audit retention; Prometheus metrics; OpenTelemetry
  tracing; diagnostics bundle; metadata repair; resumable root-key rotation.
- **Interfaces:** versioned `/api/v1` management API with OpenAPI contract and
  generated clients; embedded MCP server over the same handlers; React console
  embedded in the binary.
- **Deployment:** container image, Docker Compose development stack and Helm chart
  with disk or S3 storage, Ingress or Gateway API, NetworkPolicy, tracing,
  maintenance and federation values.
- **Quality:** real-client conformance harness with pinned clients for all
  26 formats.
- **Project:** Apache-2.0 license, contribution guide, code of conduct, security
  policy, issue and pull request templates, and a documentation site published to
  GitHub Pages.

[Unreleased]: https://github.com/azrtydxb/Kuvryn-Ant/commits/main
