# Format capability ledger — revision 1

All 26 contracts are defined for implementation. No adapter has been implemented or conformance-tested. Shared rules and each contract govern local, remote and replica sources. Native publish and administrative ingestion are explicitly distinguished.

[Shared contract rules](contracts/common.md) · [Compatibility baseline](compatibility-matrix.md)

| ID | Ecosystem | Contract / planning evidence | Resolution policy |
|---|---|---|---|
| F01 | Alpine APK | [Contract](contracts/F01.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/118) | snapshot-owner per channel/architecture |
| F02 | Ansible Galaxy | [Contract](contracts/F02.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/123) | package-owner per namespace.collection |
| F03 | APT / Debian | [Contract](contracts/F03.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/128) | snapshot-owner per distribution/component/architecture |
| F04 | CocoaPods | [Contract](contracts/F04.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/133) | package-owner per pod name |
| F05 | Composer / PHP | [Contract](contracts/F05.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/138) | package-owner per vendor/package |
| F06 | Conan | [Contract](contracts/F06.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/143) | package-owner per recipe reference |
| F07 | Conda | [Contract](contracts/F07.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/148) | snapshot-owner per channel/subdir |
| F08 | Docker / OCI | [Contract](contracts/F08.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/88) | package-owner per OCI repository |
| F09 | Git LFS | [Contract](contracts/F09.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/153) | object-owner per LFS oid within feed |
| F10 | Go modules | [Contract](contracts/F10.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/93) | package-owner per module path |
| F11 | Helm | [Contract](contracts/F11.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/98) | package-owner per chart; coherent classic index generation |
| F12 | Hugging Face | [Contract](contracts/F12.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/158) | package-owner per repo type/name; revision-bound file tree |
| F13 | Maven | [Contract](contracts/F13.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/163) | package-owner per groupId/artifactId; version-bound snapshot generation |
| F14 | npm | [Contract](contracts/F14.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/168) | package-owner per normalized scoped/unscoped name |
| F15 | NuGet / Chocolatey | [Contract](contracts/F15.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/173) | package-owner per case-normalized package ID |
| F16 | OCI artifacts | [Contract](contracts/F16.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/113) | package-owner per OCI repository; digest-bound subject graph |
| F17 | Eclipse p2 | [Contract](contracts/F17.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/178) | snapshot-owner per p2 repository |
| F18 | Pub / Flutter | [Contract](contracts/F18.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/183) | package-owner per package name |
| F19 | PyPI | [Contract](contracts/F19.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/188) | package-owner per normalized project name |
| F20 | R / CRAN | [Contract](contracts/F20.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/193) | snapshot-owner per platform/R-version repository |
| F21 | Raw | [Contract](contracts/F21.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/108) | path-owner per normalized path |
| F22 | RubyGems | [Contract](contracts/F22.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/198) | package-owner per gem name |
| F23 | Rust / Cargo | [Contract](contracts/F23.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/103) | package-owner per canonical crate name |
| F24 | Swift Package Manager | [Contract](contracts/F24.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/203) | package-owner per scope.name |
| F25 | Terraform | [Contract](contracts/F25.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/208) | package-owner per provider or module address |
| F26 | Yum / RPM | [Contract](contracts/F26.md) · [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues/213) | snapshot-owner per RPM repository |

## Delivery and support evidence

OCI/Docker → Go → Helm → Cargo → Raw, then remaining formats. OCI artifacts retain a separate conformance record after Raw. All 26 remain Phase 1 release requirements.

T1 records protocol decisions, selected clients, source/composition behavior, non-goals and positive/negative cases. T2–T4 deliver protocol implementation, cache/lifecycle integration and real-client evidence. Closing T1 does not imply support. A supported workflow must have passing retained results and exact artifact versions.

Run shared backend tests against disk and primary MinIO. AWS and other S3 providers remain compatibility targets using the same suite; do not create environments for every vendor or a full client×backend×provider matrix.
