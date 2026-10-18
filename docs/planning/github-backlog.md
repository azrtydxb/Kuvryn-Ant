# GitHub backlog index

Planning is complete; C01 and C01-T1 are the first ready implementation items. Other work follows its published prerequisites. Milestones have no due dates or assignees.

Delivery plan: https://github.com/azrtydxb/Kuvryn-Ant/issues/1. Format ledger: https://github.com/azrtydxb/Kuvryn-Ant/issues/2.

## [M00 — Planning and readiness](https://github.com/azrtydxb/Kuvryn-Ant/milestone/1)

### [P01](https://github.com/azrtydxb/Kuvryn-Ant/issues/3) — Review architecture and unblock implementation

- [P01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/4) — Record architecture decisions and threat boundaries
- [P01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/5) — Close planning gate and audit backlog coverage

### [P02](https://github.com/azrtydxb/Kuvryn-Ant/issues/6) — Define reference workloads and deployment compatibility

- [P02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/7) — Define benchmark profiles and acceptance thresholds
- [P02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/8) — Select supported infrastructure and provider test matrix

## [M01 — Shared platform and secure administration](https://github.com/azrtydxb/Kuvryn-Ant/milestone/2)

### [C01](https://github.com/azrtydxb/Kuvryn-Ant/issues/9) — Run and evolve the service on PostgreSQL

- [C01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/10) — Establish Go service lifecycle and CI
- [C01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/11) — Implement schema migrations and domain invariants

### [C02](https://github.com/azrtydxb/Kuvryn-Ant/issues/12) — Operate a stable management API

- [C02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/13) — Define and validate OpenAPI and generated clients
- [C02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/14) — Implement mutation retry and concurrent-update semantics

### [C03](https://github.com/azrtydxb/Kuvryn-Ant/issues/15) — Use scoped machine identities and consistent permissions

- [C03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/16) — Implement service accounts and token lifecycle
- [C03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/17) — Enforce resource and operation authorization

### [C04](https://github.com/azrtydxb/Kuvryn-Ant/issues/18) — Audit changes and deliver durable events

- [C04-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/19) — Persist audit and outbox records with mutations
- [C04-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/20) — Deliver webhooks and integration events reliably

### [C05](https://github.com/azrtydxb/Kuvryn-Ant/issues/21) — Diagnose service health and capacity

- [C05-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/22) — Expose health, metrics and structured logs
- [C05-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/23) — Add tracing and safe diagnostics bundles

## [M02 — Artifact integrity, storage, and local publishing](https://github.com/azrtydxb/Kuvryn-Ant/milestone/3)

### [S01](https://github.com/azrtydxb/Kuvryn-Ant/issues/24) — Store immutable bytes safely on disk

- [S01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/25) — Implement streaming blob contract and disk backend
- [S01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/26) — Prove large-object and HTTP transfer semantics

### [S02](https://github.com/azrtydxb/Kuvryn-Ant/issues/27) — Use S3 without changing artifact semantics

- [S02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/28) — Implement S3 configuration and multipart lifecycle
- [S02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/29) — Run backend equivalence and failure tests

### [S03](https://github.com/azrtydxb/Kuvryn-Ant/issues/30) — Publish atomically through deterministic feed policies

- [S03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/31) — Implement feed policy validation and explain output
- [S03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/32) — Commit artifact publication and lifecycle changes

### [S04](https://github.com/azrtydxb/Kuvryn-Ant/issues/33) — Discover packages and inspect provenance

- [S04-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/34) — Implement indexed browse and search queries
- [S04-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/35) — Expose provenance and client setup details

## [M03 — Remote caching and build acceleration](https://github.com/azrtydxb/Kuvryn-Ant/milestone/4)

### [K01](https://github.com/azrtydxb/Kuvryn-Ant/issues/36) — Fetch upstream content within explicit trust policy

- [K01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/37) — Implement upstream transports and credentials
- [K01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/38) — Implement bounded upstream failure handling

### [K02](https://github.com/azrtydxb/Kuvryn-Ant/issues/39) — Cache dependencies with predictable freshness

- [K02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/40) — Implement cache identity, freshness and revalidation
- [K02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/41) — Coalesce misses and survive concurrent failures

### [K03](https://github.com/azrtydxb/Kuvryn-Ant/issues/42) — Prewarm and govern runner traffic

- [K03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/43) — Implement durable prewarm jobs and schedules
- [K03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/44) — Enforce quotas and report cache effectiveness

## [M04 — First ecosystem delivery wave](https://github.com/azrtydxb/Kuvryn-Ant/milestone/5)

### [F08](https://github.com/azrtydxb/Kuvryn-Ant/issues/87) — Docker / OCI: publish, resolve and validate supported clients

- [F08-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/88) — Docker / OCI: finalize protocol and client acceptance contract
- [F08-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/89) — Docker / OCI: implement local protocol and metadata lifecycle
- [F08-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/90) — Docker / OCI: implement proxy resolution and cache semantics
- [F08-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/91) — Docker / OCI: certify client and replica workflows

### [F10](https://github.com/azrtydxb/Kuvryn-Ant/issues/92) — Go modules: publish, resolve and validate supported clients

- [F10-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/93) — Go modules: finalize protocol and client acceptance contract
- [F10-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/94) — Go modules: implement local protocol and metadata lifecycle
- [F10-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/95) — Go modules: implement proxy resolution and cache semantics
- [F10-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/96) — Go modules: certify client and replica workflows

### [F11](https://github.com/azrtydxb/Kuvryn-Ant/issues/97) — Helm: publish, resolve and validate supported clients

- [F11-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/98) — Helm: finalize protocol and client acceptance contract
- [F11-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/99) — Helm: implement local protocol and metadata lifecycle
- [F11-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/100) — Helm: implement proxy resolution and cache semantics
- [F11-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/101) — Helm: certify client and replica workflows

### [F23](https://github.com/azrtydxb/Kuvryn-Ant/issues/102) — Rust / Cargo: publish, resolve and validate supported clients

- [F23-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/103) — Rust / Cargo: finalize protocol and client acceptance contract
- [F23-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/104) — Rust / Cargo: implement local protocol and metadata lifecycle
- [F23-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/105) — Rust / Cargo: implement proxy resolution and cache semantics
- [F23-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/106) — Rust / Cargo: certify client and replica workflows

### [F21](https://github.com/azrtydxb/Kuvryn-Ant/issues/107) — Raw: publish, resolve and validate supported clients

- [F21-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/108) — Raw: finalize protocol and client acceptance contract
- [F21-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/109) — Raw: implement local protocol and metadata lifecycle
- [F21-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/110) — Raw: implement proxy resolution and cache semantics
- [F21-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/111) — Raw: certify client and replica workflows

### [F16](https://github.com/azrtydxb/Kuvryn-Ant/issues/112) — OCI artifacts: publish, resolve and validate supported clients

- [F16-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/113) — OCI artifacts: finalize protocol and client acceptance contract
- [F16-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/114) — OCI artifacts: implement local protocol and metadata lifecycle
- [F16-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/115) — OCI artifacts: implement proxy resolution and cache semantics
- [F16-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/116) — OCI artifacts: certify client and replica workflows

## [M05 — Remaining ecosystem breadth](https://github.com/azrtydxb/Kuvryn-Ant/milestone/6)

### [F01](https://github.com/azrtydxb/Kuvryn-Ant/issues/117) — Alpine APK: publish, resolve and validate supported clients

- [F01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/118) — Alpine APK: finalize protocol and client acceptance contract
- [F01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/119) — Alpine APK: implement local protocol and metadata lifecycle
- [F01-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/120) — Alpine APK: implement proxy resolution and cache semantics
- [F01-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/121) — Alpine APK: certify client and replica workflows

### [F02](https://github.com/azrtydxb/Kuvryn-Ant/issues/122) — Ansible Galaxy: publish, resolve and validate supported clients

- [F02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/123) — Ansible Galaxy: finalize protocol and client acceptance contract
- [F02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/124) — Ansible Galaxy: implement local protocol and metadata lifecycle
- [F02-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/125) — Ansible Galaxy: implement proxy resolution and cache semantics
- [F02-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/126) — Ansible Galaxy: certify client and replica workflows

### [F03](https://github.com/azrtydxb/Kuvryn-Ant/issues/127) — APT / Debian: publish, resolve and validate supported clients

- [F03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/128) — APT / Debian: finalize protocol and client acceptance contract
- [F03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/129) — APT / Debian: implement local protocol and metadata lifecycle
- [F03-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/130) — APT / Debian: implement proxy resolution and cache semantics
- [F03-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/131) — APT / Debian: certify client and replica workflows

### [F04](https://github.com/azrtydxb/Kuvryn-Ant/issues/132) — CocoaPods: publish, resolve and validate supported clients

- [F04-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/133) — CocoaPods: finalize protocol and client acceptance contract
- [F04-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/134) — CocoaPods: implement local protocol and metadata lifecycle
- [F04-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/135) — CocoaPods: implement proxy resolution and cache semantics
- [F04-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/136) — CocoaPods: certify client and replica workflows

### [F05](https://github.com/azrtydxb/Kuvryn-Ant/issues/137) — Composer / PHP: publish, resolve and validate supported clients

- [F05-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/138) — Composer / PHP: finalize protocol and client acceptance contract
- [F05-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/139) — Composer / PHP: implement local protocol and metadata lifecycle
- [F05-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/140) — Composer / PHP: implement proxy resolution and cache semantics
- [F05-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/141) — Composer / PHP: certify client and replica workflows

### [F06](https://github.com/azrtydxb/Kuvryn-Ant/issues/142) — Conan: publish, resolve and validate supported clients

- [F06-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/143) — Conan: finalize protocol and client acceptance contract
- [F06-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/144) — Conan: implement local protocol and metadata lifecycle
- [F06-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/145) — Conan: implement proxy resolution and cache semantics
- [F06-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/146) — Conan: certify client and replica workflows

### [F07](https://github.com/azrtydxb/Kuvryn-Ant/issues/147) — Conda: publish, resolve and validate supported clients

- [F07-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/148) — Conda: finalize protocol and client acceptance contract
- [F07-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/149) — Conda: implement local protocol and metadata lifecycle
- [F07-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/150) — Conda: implement proxy resolution and cache semantics
- [F07-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/151) — Conda: certify client and replica workflows

### [F09](https://github.com/azrtydxb/Kuvryn-Ant/issues/152) — Git LFS: publish, resolve and validate supported clients

- [F09-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/153) — Git LFS: finalize protocol and client acceptance contract
- [F09-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/154) — Git LFS: implement local protocol and metadata lifecycle
- [F09-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/155) — Git LFS: implement proxy resolution and cache semantics
- [F09-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/156) — Git LFS: certify client and replica workflows

### [F12](https://github.com/azrtydxb/Kuvryn-Ant/issues/157) — Hugging Face: publish, resolve and validate supported clients

- [F12-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/158) — Hugging Face: finalize protocol and client acceptance contract
- [F12-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/159) — Hugging Face: implement local protocol and metadata lifecycle
- [F12-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/160) — Hugging Face: implement proxy resolution and cache semantics
- [F12-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/161) — Hugging Face: certify client and replica workflows

### [F13](https://github.com/azrtydxb/Kuvryn-Ant/issues/162) — Maven: publish, resolve and validate supported clients

- [F13-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/163) — Maven: finalize protocol and client acceptance contract
- [F13-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/164) — Maven: implement local protocol and metadata lifecycle
- [F13-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/165) — Maven: implement proxy resolution and cache semantics
- [F13-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/166) — Maven: certify client and replica workflows

### [F14](https://github.com/azrtydxb/Kuvryn-Ant/issues/167) — npm: publish, resolve and validate supported clients

- [F14-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/168) — npm: finalize protocol and client acceptance contract
- [F14-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/169) — npm: implement local protocol and metadata lifecycle
- [F14-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/170) — npm: implement proxy resolution and cache semantics
- [F14-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/171) — npm: certify client and replica workflows

### [F15](https://github.com/azrtydxb/Kuvryn-Ant/issues/172) — NuGet / Chocolatey: publish, resolve and validate supported clients

- [F15-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/173) — NuGet / Chocolatey: finalize protocol and client acceptance contract
- [F15-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/174) — NuGet / Chocolatey: implement local protocol and metadata lifecycle
- [F15-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/175) — NuGet / Chocolatey: implement proxy resolution and cache semantics
- [F15-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/176) — NuGet / Chocolatey: certify client and replica workflows

### [F17](https://github.com/azrtydxb/Kuvryn-Ant/issues/177) — Eclipse p2: publish, resolve and validate supported clients

- [F17-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/178) — Eclipse p2: finalize protocol and client acceptance contract
- [F17-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/179) — Eclipse p2: implement local protocol and metadata lifecycle
- [F17-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/180) — Eclipse p2: implement proxy resolution and cache semantics
- [F17-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/181) — Eclipse p2: certify client and replica workflows

### [F18](https://github.com/azrtydxb/Kuvryn-Ant/issues/182) — Pub / Flutter: publish, resolve and validate supported clients

- [F18-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/183) — Pub / Flutter: finalize protocol and client acceptance contract
- [F18-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/184) — Pub / Flutter: implement local protocol and metadata lifecycle
- [F18-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/185) — Pub / Flutter: implement proxy resolution and cache semantics
- [F18-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/186) — Pub / Flutter: certify client and replica workflows

### [F19](https://github.com/azrtydxb/Kuvryn-Ant/issues/187) — PyPI: publish, resolve and validate supported clients

- [F19-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/188) — PyPI: finalize protocol and client acceptance contract
- [F19-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/189) — PyPI: implement local protocol and metadata lifecycle
- [F19-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/190) — PyPI: implement proxy resolution and cache semantics
- [F19-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/191) — PyPI: certify client and replica workflows

### [F20](https://github.com/azrtydxb/Kuvryn-Ant/issues/192) — R / CRAN: publish, resolve and validate supported clients

- [F20-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/193) — R / CRAN: finalize protocol and client acceptance contract
- [F20-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/194) — R / CRAN: implement local protocol and metadata lifecycle
- [F20-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/195) — R / CRAN: implement proxy resolution and cache semantics
- [F20-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/196) — R / CRAN: certify client and replica workflows

### [F22](https://github.com/azrtydxb/Kuvryn-Ant/issues/197) — RubyGems: publish, resolve and validate supported clients

- [F22-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/198) — RubyGems: finalize protocol and client acceptance contract
- [F22-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/199) — RubyGems: implement local protocol and metadata lifecycle
- [F22-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/200) — RubyGems: implement proxy resolution and cache semantics
- [F22-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/201) — RubyGems: certify client and replica workflows

### [F24](https://github.com/azrtydxb/Kuvryn-Ant/issues/202) — Swift Package Manager: publish, resolve and validate supported clients

- [F24-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/203) — Swift Package Manager: finalize protocol and client acceptance contract
- [F24-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/204) — Swift Package Manager: implement local protocol and metadata lifecycle
- [F24-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/205) — Swift Package Manager: implement proxy resolution and cache semantics
- [F24-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/206) — Swift Package Manager: certify client and replica workflows

### [F25](https://github.com/azrtydxb/Kuvryn-Ant/issues/207) — Terraform: publish, resolve and validate supported clients

- [F25-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/208) — Terraform: finalize protocol and client acceptance contract
- [F25-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/209) — Terraform: implement local protocol and metadata lifecycle
- [F25-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/210) — Terraform: implement proxy resolution and cache semantics
- [F25-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/211) — Terraform: certify client and replica workflows

### [F26](https://github.com/azrtydxb/Kuvryn-Ant/issues/212) — Yum / RPM: publish, resolve and validate supported clients

- [F26-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/213) — Yum / RPM: finalize protocol and client acceptance contract
- [F26-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/214) — Yum / RPM: implement local protocol and metadata lifecycle
- [F26-T3](https://github.com/azrtydxb/Kuvryn-Ant/issues/215) — Yum / RPM: implement proxy resolution and cache semantics
- [F26-T4](https://github.com/azrtydxb/Kuvryn-Ant/issues/216) — Yum / RPM: certify client and replica workflows

## [M06 — Enterprise identity and complete operator interfaces](https://github.com/azrtydxb/Kuvryn-Ant/milestone/7)

### [I01](https://github.com/azrtydxb/Kuvryn-Ant/issues/45) — Sign in with OIDC and diagnose provider setup

- [I01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/46) — Implement OIDC sessions and PKCE
- [I01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/47) — Implement provider diagnostics and role mappings

### [I02](https://github.com/azrtydxb/Kuvryn-Ant/issues/48) — Authenticate enterprise users through SAML and LDAP

- [I02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/49) — Implement and validate SAML browser SSO
- [I02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/50) — Implement LDAP and Active Directory authentication

### [I03](https://github.com/azrtydxb/Kuvryn-Ant/issues/51) — Recover access and protect stored secrets

- [I03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/52) — Implement emergency administrator policy and recovery
- [I03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/53) — Encrypt and rotate application secrets

### [U01](https://github.com/azrtydxb/Kuvryn-Ant/issues/54) — Navigate an accessible React console and instance overview

- [U01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/55) — Build console session, navigation and API client foundation
- [U01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/56) — Build health and acceleration overview

### [U02](https://github.com/azrtydxb/Kuvryn-Ant/issues/57) — Configure feeds and investigate packages in the console

- [U02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/58) — Build feed/source/policy editor and explanation views
- [U02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/59) — Build package browse and cache diagnosis views

### [U03](https://github.com/azrtydxb/Kuvryn-Ant/issues/60) — Administer identity and governance through the console

- [U03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/61) — Build provider, principal and token administration
- [U03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/62) — Build audit browsing and governance exports

### [A01](https://github.com/azrtydxb/Kuvryn-Ant/issues/63) — Operate the product through scoped MCP tools

- [A01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/64) — Implement MCP discovery and diagnostic tools
- [A01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/65) — Implement administrative parity and destructive-action semantics

### [U04](https://github.com/azrtydxb/Kuvryn-Ant/issues/84) — Operate replication and lifecycle jobs visually

- [U04-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/85) — Build peer, seed and incremental sync screens
- [U04-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/86) — Build storage, maintenance and adoption screens

## [M07 — Independent-instance replication](https://github.com/azrtydxb/Kuvryn-Ant/milestone/8)

### [R01](https://github.com/azrtydxb/Kuvryn-Ant/issues/66) — Seed an independently operable replica

- [R01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/67) — Implement peer trust and complete feed manifests
- [R01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/68) — Implement restartable full seed transfers

### [R02](https://github.com/azrtydxb/Kuvryn-Ant/issues/69) — Keep replicas current without deleting local content

- [R02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/70) — Implement selective and incremental replication jobs
- [R02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/71) — Implement conflict and tombstone policy with dry runs

## [M08 — Adoption and operational lifecycle](https://github.com/azrtydxb/Kuvryn-Ant/milestone/9)

### [O01](https://github.com/azrtydxb/Kuvryn-Ant/issues/72) — Maintain references and repair storage safely

- [O01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/73) — Implement reference-safe retention and garbage collection
- [O01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/74) — Implement integrity scan and metadata/replication repair

### [O02](https://github.com/azrtydxb/Kuvryn-Ant/issues/75) — Back up, restore and migrate storage

- [O02-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/76) — Implement consistent backup and restore workflows
- [O02-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/77) — Implement disk/S3 migration and schema upgrade rehearsal

### [O03](https://github.com/azrtydxb/Kuvryn-Ant/issues/78) — Deploy securely with Compose and Kubernetes

- [O03-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/79) — Package non-root image and Compose installation
- [O03-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/80) — Provide Helm chart and managed-operation guidance

### [D01](https://github.com/azrtydxb/Kuvryn-Ant/issues/81) — Inventory and migrate existing repositories

- [D01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/82) — Implement generic configuration and content import/export
- [D01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/83) — Validate migration mappings and staged runner adoption

## [M09 — Phase 1 release qualification](https://github.com/azrtydxb/Kuvryn-Ant/milestone/10)

### [Q01](https://github.com/azrtydxb/Kuvryn-Ant/issues/217) — Qualify Phase 1 with reproducible release evidence

- [Q01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/218) — Run security, interoperability and interface parity qualification
- [Q01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/219) — Run performance, resilience and recovery release gates

## [M10 — Federation discovery (Phase 2, not a Phase 1 dependency)](https://github.com/azrtydxb/Kuvryn-Ant/milestone/11)

### [Z01](https://github.com/azrtydxb/Kuvryn-Ant/issues/220) — Define federation as a separate future phase

- [Z01-T1](https://github.com/azrtydxb/Kuvryn-Ant/issues/221) — Specify federation identity, discovery and consistency requirements
- [Z01-T2](https://github.com/azrtydxb/Kuvryn-Ant/issues/222) — Prepare Phase 2 architecture and acceptance backlog

