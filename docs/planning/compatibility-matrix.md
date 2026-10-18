# Compatibility and validation baseline — revision 1

Status: Selected for implementation, 2026-10-18. The owner specified Oracle OKE and MinIO on TrueNAS SCALE and clarified that AWS and other providers remain product compatibility targets. Do not create test environments for every vendor. No infrastructure has been provisioned and no compatibility pass is claimed by this planning record.

## Three distinct meanings

- **Product contract:** Linux deployment, PostgreSQL, local disk and S3-compatible object storage, Compose/Helm, standard identity protocols and all 26 ecosystem contracts.
- **Primary validation:** reuse the owner's OKE and MinIO-on-TrueNAS environment when deployment testing is due, plus lightweight local/CI fixtures. This drives first integration evidence.
- **Additional compatibility:** AWS S3, OCI Object Storage's S3-compatible interface and other compatible S3 providers can use the same endpoint/credential/configuration contract. Record provider-specific evidence as exercised; do not call an untested provider certified, and do not make a separate environment for each a development prerequisite.

## Platform selections

| Area | Selected baseline | Validation boundary |
|---|---|---|
| Service OS/CPU | Linux amd64 and arm64; non-root OCI image; build toolchain Go 1.27.2 | Native developer macOS is allowed; production server certification is Linux. Cross-build plus target runtime smoke evidence required. |
| Metadata | PostgreSQL 18.6 primary; 17.11 compatibility/upgrade input | Same SQL/transaction suite, migrations and one documented major upgrade path. No embedded DB. Patch refreshes are recorded in the fixture lock. |
| Kubernetes | Oracle OKE; reference production target 1.36.4 | Inventory actual cluster/control-plane/node versions at setup. Oracle currently lists 1.37.0 as preview, so it is not the production planning default. Do not upgrade the user's cluster as part of planning. |
| Helm / Compose | Helm 4.3.1; Docker Compose 5.6.0; Docker engine reference 29.9.1 | Packaging tests in existing/local environments; no new EKS/GKE/AKS test clusters. Document resource limits, probes and one-writer topology. |
| Local disk | ext4/XFS on a local Linux block filesystem | Verify fsync/atomic publication, capacity/inode failure and cancellation. NFS/SMB/object-fuse mounts are not a substitute for the disk contract. |
| Primary S3 | Existing MinIO app on TrueNAS SCALE over HTTPS | Record installed server version and immutable image digest before actual test/deploy. Reference discovery found RELEASE.2025-10-15T17-29-55Z and TrueNAS 25.10.4; these are inventory references, not an instruction to install, downgrade or replace the user's instance. |
| Generic S3 | SigV4, configurable endpoint/region/bucket/prefix, path-style/virtual-host, TLS roots, static/session credentials or supported ambient credential chain | Endpoint features are probed/validated. AWS and other providers remain compatible targets; optional runs reuse the same suite. No claim of AWS-only APIs or provider certification without evidence. |
| Browser | Current stable Chromium, Firefox, Safari at UI test execution; versions locked with run | Keyboard/focus/a11y plus login/cookie/error behavior. Do not hard-code an obsolete browser release in a year-long product plan. |
| Native format clients | Exact selected versions in contracts/F01–F26 and reference-versions.json | Install and record binary/image digest as each conformance task begins. A baseline version does not imply backward compatibility with every older client. |

[Oracle OKE version support](https://docs.oracle.com/en-us/iaas/Content/ContEng/Concepts/contengaboutk8sversions.htm) supplies the OKE distinction. [PostgreSQL release policy](https://www.postgresql.org/support/versioning/) supplies selected DB versions. Other tool versions were resolved through their official release endpoints listed in reference-versions.json.

## OKE → TrueNAS network and storage acceptance

Record DNS, endpoint port, certificate chain, bucket/prefix, credential reference, region setting and measured RTT/bandwidth. Network topology (private link/VPN/public TLS) is an operator deployment choice, not something the application silently provisions. Use an explicitly allowed destination and network policies; do not expose the MinIO console as the application's object API.

Validate small PUT/GET/HEAD/DELETE, range/conditional reads, streaming and multipart upload/resume/abort, credential expiry/rotation, bucket policy denial and integrity after restart. Verify full-object checksums independently of ETags. Test storage outages and bandwidth limits against test buckets only. Request retries must be safe across the OKE-to-NAS path.

Use the TrueNAS instance as S3 object storage, not as an assumption that the application has direct access to ZFS snapshots or filesystem paths. Application backup/restore pins a consistent PostgreSQL/blob reference set. TrueNAS snapshots may complement it but are not alone a complete application backup. No MinIO/AIStor migration, app upgrade or tenancy change is included in this work.

MinIO's upstream repository is currently archived, and TrueNAS app delivery can differ from upstream releases; therefore inventory the actual deployed binary/image rather than infer it from a mutable catalog label. This does not change the user's selected storage. The adapter depends on tested S3 operations, not MinIO administrative APIs. References: [MinIO release](https://github.com/minio/minio/releases/tag/RELEASE.2025-10-15T17-29-55Z), [upstream repository](https://github.com/minio/minio), [TrueNAS apps](https://apps.truenas.com/), [TrueNAS release artifacts](https://update.truenas.com/).

## Identity acceptance scope

| Capability | Concrete reference fixture | Required cases |
|---|---|---|
| OIDC | Zitadel 4.19.5 and Keycloak 26.8.1 | Code+PKCE, issuer/audience/state/nonce, JWKS rotation, group mapping, logout, expired/revoked sessions and recovery. |
| SAML 2.0 | Keycloak 26.8.1 SAML IdP | SP-initiated login, signed assertions, replay/recipient/time failures, metadata and certificate rollover, attribute mapping and supported logout. |
| LDAP | OpenLDAP LTS 2.6.15 | LDAPS/StartTLS, escaped filters, bind/search authorization, nested-group limits and outage behavior. |
| AD-compatible directory | Samba 4.24.5 for repeatable AD schema/group fixtures; Windows Server AD integration scenario before a specific Windows AD certification claim | Bind/group mapping and TLS; Samba evidence is not relabeled as Microsoft Windows AD evidence. Use an existing AD target if available when the feature is tested. |
| Other providers | Entra ID, Okta, Authentik and generic standards-compatible providers | Configuration/discovery/mapping contract is supported by design; optional provider smoke when requested, using existing accounts. Do not create a permanent IdP fleet. |

Fixtures are reusable, on-demand conformance inputs. Their versions are targets, not claims that environments already exist. Sources: [Zitadel releases](https://github.com/zitadel/zitadel/releases), [Keycloak releases](https://github.com/keycloak/keycloak/releases), [OpenLDAP releases](https://www.openldap.org/software/download/), [Samba releases](https://www.samba.org/samba/history/).

## Protocol and client test hosts

- Linux fixture runner for the shared core and most package protocols.
- macOS runner only when CocoaPods, Swift/macOS or R/macOS workflows are implemented.
- Windows runner only for Chocolatey/NuGet symbol and R/Windows cases; Windows AD is a separately identified provider scenario.
- A client executable or pinned container is enough where possible. No dedicated permanent environment per ecosystem or cloud.
- Fixture acquisition records actual hashes before execution; security patch updates may replace a pinned reference with a documented compatible patch, never silently alter a retained test result.

## Import/export adoption baseline

Implement the generic manifest import/export first. Define a read-only inventory and dry-run mapper for Sonatype Nexus Repository 3 exports and documented Maven/npm/raw content layouts; the input manifest records the exact source export/version at execution. Credentials are references, never exported plaintext. Unsupported mappings are errors in the report, not silently coerced feeds. Other managers can emit the generic manifest; a manager-specific API compatibility promise is outside scope. Fixtures can be synthetic export samples; do not stand up a fleet of legacy managers.

## Planning completion versus execution prerequisites

The protocol, infrastructure family, version policy, source scope and test methods are selected. Exact installed endpoint addresses, secrets and image digests are gathered only when relevant implementation/deployment tasks need them. Missing deployment inventory does not block core implementation and is not mislabeled as a completed deployment test. Existing scope and source contracts remain the authority; no provider-specific certification is inferred from generic S3 or OIDC compatibility.
