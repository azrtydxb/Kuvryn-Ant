# Reference workloads and release gates — revision 1

Status: Selected engineering acceptance targets, 2026-10-18. These are targets to measure during implementation, not measured results or commercial SLO promises. No calendar/team commitments and no new test infrastructure are created by this plan.

## Environment reuse

Use the existing OKE and MinIO-on-TrueNAS deployment as the primary integration target after normal deployment setup. Run small local/CI PostgreSQL and protocol fixtures only when needed. One reusable backend suite supports disk, MinIO, AWS S3 and other compatible endpoints; AWS/other-provider environments are optional targeted runs, not prerequisites for starting development or a requirement to build a vendor fleet.

Inventory exact control-plane/node versions, TrueNAS version, MinIO image digest/version, bucket policy/TLS, network path and PostgreSQL at setup. Do not assume the latest advertised upstream release is installed. Adapt fixture scale down for developer checks while retaining full release profiles for qualification.

## Defined profiles

| ID | Dataset and traffic | Reference resources and measurement |
|---|---|---|
| W1 small site | 100,000 asset records; 10,000 packages; 100 GiB blob working set; 16 clients; 100 metadata requests/sec | App 4 vCPU/8 GiB; PostgreSQL 2 vCPU/4 GiB; NVMe; 1 Gbit/s local link. Capture actual hardware if using different resources. |
| W2 build fan-out | 64 clients; 1,000,000 assets; 1 TiB working set; cold and warm runs, 90% popular/10% unique objects | App 8 vCPU/16 GiB; PostgreSQL 4 vCPU/8 GiB; 10 Gbit/s local reference network; disk and existing MinIO tested separately. |
| W3 large objects | 1, 10 and 100 GiB deterministic fixtures; 1, 4 and 16 concurrent transfers; mid-transfer cancellation and range resume | Same W2 resources; charge bounded buffers; observe resident memory, checksum and disk/network throughput. Sparse fixtures must be materialized for realistic throughput tests. |
| W4 metadata/publish | 100 metadata requests/sec plus 10 publishes/sec for 10 minutes; duplicate coordinate writers and mutable tags/indexes | W1 resources; compare read visibility, contention, audit/outbox consistency and index generations. |
| W5 multi-site | Two logical instances; 10,000 assets/100 GiB seed; incremental batches of 100 updates; 50 ms RTT and 100 Mbit/s transfer cap | May reuse the same cluster with isolated databases/buckets. Interrupt seed/incremental and disconnect source. No extra cloud tenancy required. |
| W6 outages | DB unavailable, storage full, S3 timeout/throttling, upstream failure, pod termination, lost job lease | Fault injection against fixtures/authorized test data, not production faults. Verify recovery and absence of corruption. |

## Pass/fail targets

- W1 metadata at 100 requests/sec: p95 ≤100 ms and p99 ≤300 ms for warm indexed metadata, excluding client network latency. Record authorization and DB query costs.
- Warm large-object throughput: ≥80% of direct backend throughput measured on the same path and payload, rather than promising a fixed cross-site bandwidth. W2 metadata error rate <0.1% for valid, non-fault-injected requests.
- W2 identical-key cold fan-out: one upstream fetch per key while the serving instance is healthy; restart may refetch uncommitted partial data, never publish it. Warm runs perform no upstream blob fetch for immutable already-cached objects.
- W3 streaming: increasing one object from 1 to 100 GiB must add ≤64 MiB peak application RSS at fixed concurrency after warmup. Charged transfer buffers ≤1 GiB globally; report base RSS separately. Digest/size correctness is mandatory at every concurrency level.
- W4 immutable conflict: exactly one accepted content value; conflicting writes cannot silently overwrite. Every successful mutation has a matching committed audit/outbox event and no partial package is readable.
- W5 correctness: resume retransfers no already-verified immutable blobs; checkpoint processing resumes ≤60 seconds after dependencies recover. Incremental metadata lag p95 ≤30 seconds for 100 metadata-only updates with no backlog; blob transfer time is measured separately against the capped-link budget. Source outage must not break seeded local reads.
- W6 retry bound: at most three attempts per safe operation; circuits limit repeated outages; cancelled or denied operations cannot poison caches. No acknowledged publication may reference unverifiable bytes.
- Recovery exercise: restore a 100 GiB/100,000-asset backup into an isolated instance in ≤60 minutes when sustained restore-path throughput is at least 100 MiB/s. RPO is the chosen consistent backup boundary; backup scheduling default is 24 hours, explicitly not zero data loss. Record achievable RPO/RTO rather than claim synchronous disaster recovery.
- Security/integrity/authorization tests have zero tolerated failures. Performance targets can be revised only with a documented measured baseline and rationale before release qualification; silently relaxing a failed target is prohibited.

## Execution and evidence

For each run record exact server commit, client binaries/image digests, backend and DB versions, hardware/limits, network, data generator seed, warmup (2 minutes), measurement (10 minutes; large transfers until complete) and three repetitions. Preserve distributions and peak memory, not only averages. Report median throughput across repetitions and worst correctness outcome.

Measure warm/cold separately; distinguish bytes actually avoided from estimated download time saved. No time-saving estimate becomes a product promise. Backend/vendor failures must not be masked as protocol success. Protocol suites use the exact selected client baselines in the contracts; add a compatibility smoke for the preceding supported minor when it exercises a distinct behavior, without requiring every client/version/provider Cartesian product.

Planning is complete when targets and methods are defined. Passing these tests remains Q01 and domain-task work; no benchmark result has been fabricated to close planning.
