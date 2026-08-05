# ADR-016: NCCL All-Reduce Certification Catalog Entry

## Context

GPU cluster burn-in requires validating multi-node communication fabric in addition to training workloads. NCCL `all_reduce_perf` is the standard benchmark for measuring collective communication bandwidth across GPU nodes connected via NVLink/MNNVL. A working MPI-based NCCL all-reduce test exists as a UAT example (`test/uat/aws-gb300-all-reduce.yaml`) but is not yet integrated into the certification flow.

The certification catalog (ADR-010) maps `{domain, variant}` to pre-configured WorkflowSpec builders. Currently, only training entries exist. Adding NCCL all-reduce requires a new catalog entry that follows the same patterns.

## Decision

Add a `communication/nccl-all-reduce` catalog entry in `pkg/catalog/communication_nccl_all_reduce.go` that produces a self-contained WorkflowSpec with MPI-based NCCL all-reduce testing. The entry follows the training entry pattern: TrainJob workload with a namespaced TrainingRuntime dependency and a ComputeDomain dependency.

## Implementation

### Catalog Registration

```go
func init() {
    Register("communication", "nccl-all-reduce", Entry{
        Build: buildCommunicationNCCLAllReduce,
    })
}
```

### WorkflowSpec Structure

- **Workload**: TrainJob referencing `nccl-all-reduce-runtime` (namespaced TrainingRuntime)
  - Image: `nvcr.io/nvidia/pytorch:26.01-py3`
  - Command: `/usr/local/mpi/bin/mpirun` with NCCL all_reduce_perf_mpi binary
  - 8 nodes x 4 GPUs = 32 processes, message sizes 8B to 16GB
  - NCCL env vars passed via mpirun `-x` flags (required for MPI worker propagation)
- **Dependencies**: TrainingRuntime (MPI framework with SSH) + ComputeDomain (MNNVL scheduling)
- **Orchestration**: per-rack via `nvidia.com/gpu.clique` topology, 1 iteration
- **Health Monitor**: CEL `node.spec.unschedulable == true`
- **No GoodputMeasurement**: NCCL tabular output has no existing LogProfile; pass/fail is based on workload exit code

### Dependencies

1. **TrainingRuntime** (`nccl-all-reduce-runtime`) — MPI framework with:
   - OpenMPI mlPolicy with SSH auth mount
   - Launcher replicatedJob: SSH key permission fix init container
   - Node replicatedJob: OpenSSH server, ComputeDomain resource claim, GPU resources, readiness probe
   - Workflow-scoped, auto-cleanup

2. **ComputeDomain** (`nccl-all-reduce-compute-domain`) — MNNVL GPU scheduling:
   - Single allocation mode with channel template
   - numNodes: 8
   - Job-scoped (per-job copy), auto-cleanup

### Domain/Variant Naming

`communication/nccl-all-reduce` opens the `communication` domain for future entries (e.g., `communication/nccl-all-gather`, `communication/nccl-reduce-scatter`) while clearly distinguishing from `training/*` entries.

## Rationale

- **Namespaced TrainingRuntime** (not ClusterTrainingRuntime): Self-contained per-workflow lifecycle with auto-cleanup, consistent with other training entries. No cluster-admin prerequisites.
- **mpirun `-x` flags** (not Trainer.Env): MPI requires explicit environment forwarding from launcher to workers. Setting env on the launcher container does not propagate to workers.
- **emptyDir for dshm** (not hostPath): More portable across environments. The UAT uses hostPath for `/dev/shm`, but emptyDir with `medium: Memory` achieves the same shared memory without requiring host access.
- **No GoodputMeasurement**: NCCL `all_reduce_perf` outputs tabular bandwidth data (message size vs. bus bandwidth), which is structurally different from training step logs. Adding NCCL bandwidth parsing would require a new LogProfile definition and potentially changes to the goodput calculation model. Deferred to a future ADR.

## Consequences

### Positive
- Enables NCCL all-reduce communication benchmarks in the certification flow
- Opens `communication` domain for future collective operation entries
- Self-contained: single catalog file, no controller changes

### Negative
- No performance metric collection initially (pass/fail only via exit code)
- MPI TrainingRuntime dependency is complex (SSH setup, two replicatedJobs)

## Alternatives Considered

### Pre-deployed ClusterTrainingRuntime
**Rejected**: Requires cluster-admin setup before certification can run. Not self-contained. Inconsistent with training entries which create all dependencies as Workflow-managed resources.

### NCCL-specific LogProfile for GoodputMeasurement
**Deferred**: NCCL output format (tabular bandwidth) is fundamentally different from training step logs. Would require a new LogProfile with different capture group semantics. Better addressed as a separate effort.

### PyTorchJob or MPIJob adapter instead of TrainJob
**Rejected**: The TrainJob + TrainingRuntime approach is the standard pattern in this codebase. It provides MPI support via the Kubeflow Trainer MPI plugin while maintaining a consistent adapter interface.

## References

- `test/uat/aws-gb300-all-reduce.yaml` — source UAT example
- `pkg/catalog/entries/training/` — pattern followed
- ADR-010 — certification catalog architecture
- ADR-011 — workflow dependency lifecycle
