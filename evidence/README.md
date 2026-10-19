# Evidence

Versioned, dated evidence for the acceptance criteria in `kuvryn-ant-product-spec.md` section 11. Each directory is one
qualification session; nothing in it is an estimate unless the text says so, and every number can be traced to a raw
file in the same directory.

| Session                               | Covers                                                                                                                                | Summary                        |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------ |
| [`2026-10-18/`](2026-10-18/README.md) | AC5 two-instance replication on the real clusters, AC6 backend equivalence, AC8 benchmarks, AC9 backup/restore and upgrade rehearsals | [README](2026-10-18/README.md) |

Conformance evidence for individual ecosystems (AC1/AC2) lives under `conformance/results/`.

Conventions: evidence directories contain the exact scripts and Go tools that produced them (`scripts/`, `tools/`,
and the harness in `bench/`); credentials are read from `~/.kuvryn-ant/` at run time and never recorded; the
server image digest and the Helm revision window are stated per run because the shared deployments are redeployed
frequently by other work.
