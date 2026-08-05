# ADR-018: NCCL Test Suite Catalog Entries

## Context

ADR-016 added a single `communication/nccl-all-reduce` catalog entry for NCCL bandwidth validation. However, `all_reduce` is only one of several NCCL collective operations. A thorough GPU cluster burn-in should validate all key collectives — all-gather, all-to-all, reduce-scatter, broadcast, reduce, and send-receive — since each exercises different communication paths in the NVLink/network fabric.

Additionally, the original all-reduce entry runs indefinitely (`-N 0` flag) and needs a 30-minute timeout. When multiple NCCL test types run in the same cluster, Prometheus metrics need a `nccl_test` label to distinguish results on dashboards.

## Decision

1. Refactor the single `communication/nccl-all-reduce` catalog entry into a parameterized builder that registers 7 NCCL collective test variants from a single file.
2. Add a `testType` field to `BandwidthMeasurementSpec` and `BandwidthMeasurementConfig` to identify which NCCL collective is being measured.
3. Add a `nccl_test` label to the `burnin_nccl_algbw_gbps` and `burnin_nccl_busbw_gbps` Prometheus gauges.
4. Wrap the mpirun command with `timeout 1800` for a 30-minute hard stop.

## Implementation

### Catalog Variants

All variants share the same MPI TrainingRuntime, ComputeDomain, SSH setup, image, env vars, and LogProfile. Only the binary name and resource name prefix differ.

| Variant | Binary | TestType Label |
|---------|--------|----------------|
| nccl-all-reduce | all_reduce_perf_mpi | all_reduce |
| nccl-all-gather | all_gather_perf_mpi | all_gather |
| nccl-alltoall | alltoall_perf_mpi | alltoall |

### TestType on Metrics

The `nccl_test` label enables Grafana dashboards to show per-collective bandwidth:
```
burnin_nccl_busbw_gbps{nccl_test="all_reduce", message_size_bytes="1073741824"} 769.45
burnin_nccl_busbw_gbps{nccl_test="alltoall", message_size_bytes="1073741824"} 412.33
```

### 30-Minute Timeout

The Trainer command is changed from `["/usr/local/mpi/bin/mpirun"]` to `["timeout", "1800", "/usr/local/mpi/bin/mpirun"]`. This uses GNU `timeout` to send SIGTERM after 30 minutes, ensuring the test completes even with `-N 0` (infinite sweep count). The `TimeoutPerJob` field on `OrchestrationSpec.Execution` is not yet enforced by the controller, so the command-level timeout is the most reliable approach.

## Rationale

- **Single file, parameterized builder**: All NCCL tests are structurally identical except for the binary name. A loop-based registration avoids 7 copies of the same 350-line file.
- **`testType` field**: Embedding the test type in the measurement spec (rather than inferring it from labels or names) is explicit, queryable, and survives name truncation.
- **Command-level timeout**: More reliable than controller-level enforcement since it works regardless of controller state. GNU `timeout` is available in the nvidia/pytorch container image.

## Consequences

### Positive
- One Certification resource can validate all 7 NCCL collectives across the cluster
- Dashboard metrics clearly distinguish test types
- Tests are guaranteed to complete within 30 minutes

### Negative
- 7 catalog entries from one file; adding a new NCCL test requires only a new entry in the `ncclTests` slice

## References

- ADR-010 — certification catalog
- ADR-016 — NCCL all-reduce catalog entry
- ADR-017 — bandwidth measurement CRD
- NCCL tests: https://github.com/NVIDIA/nccl-tests
