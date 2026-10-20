---
layout: home

hero:
  name: Kuvryn Ant
  text: One artifact core, every package format
  tagline: A self-hosted artifact repository and caching proxy for 26 package ecosystems, with PostgreSQL metadata, disk or S3 storage, replication and federation.
  image:
    src: /ant-logo.png
    alt: Kuvryn Ant logo
  actions:
    - theme: brand
      text: Quick start
      link: /guide/quick-start
    - theme: alt
      text: Introduction
      link: /guide/introduction
    - theme: alt
      text: Supported formats
      link: /formats/

features:
  - title: 26 package formats
    details: OCI and Docker, Helm, npm, PyPI, Maven, Go, Cargo, NuGet, RubyGems, Composer, Conda, Conan, CRAN, pub, CocoaPods, Swift, Terraform, Ansible, APT, RPM, APK, Git LFS, P2, Hugging Face and raw files over WebDAV.
    link: /formats/
    linkText: Format guides
  - title: Pull-through caching that fails safe
    details: Remote sources cache upstream content on first read. The owner of a package wins, a remote 404 falls through and a remote error stops the search, so a private outage never resolves from a public source.
    link: /concepts/resolution
    linkText: Read order
  - title: Host-level mirrors and build caches
    details: Serve containerd, CRI-O, Docker, Podman and BuildKit as a registry mirror. Keep buildx layer caches and sccache objects in evictable cache feeds with idle and size limits.
    link: /build-caches/
    linkText: Build caches
  - title: Identity built in
    details: Local users with TOTP, OIDC, SAML and LDAP sign-in, persisted roles and grants, scoped service tokens, and GitHub Actions OIDC exchange for short-lived CI credentials.
    link: /identity/
    linkText: Identity
  - title: Replication and federation
    details: Gapless change feeds replicate between sites with digest verification. Federation adds signed membership, shared policy, placement-aware routing and authority handover.
    link: /replication/
    linkText: Multi-site
  - title: Operable by design
    details: Atomic audited publication, two-phase garbage collection, storage-pressure eviction, integrity scans, backup and restore, Prometheus metrics, OpenTelemetry tracing and an MCP endpoint.
    link: /operations/storage-maintenance
    linkText: Operations
---
