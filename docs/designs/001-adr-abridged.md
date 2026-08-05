# ADR-001: Architecture — CRE for GPU Cluster Certification

**Status:** Proposed
**Date:** 2026-02-10
**Deciders:** CRE Maintainers

## Context

GPU clusters used for distributed training need hardware burn-in before accepting production workloads. Faulty GPUs, degraded NVLink interconnects, or misconfigured networking cause silent training corruption, stalls, or cascading failures that waste expensive compute hours. Catching these faults requires real distributed training workloads that cycle GPUs between intense compute and idle pauses, stress the full infrastructure (VRAM, NVLink, storage I/O) simultaneously, and verify mathematical integrity, catching Silent Data Errors that synthetic tools miss. These workloads run for hours or days and need an orchestrator that continuously monitors the run, detects failures as they happen, and acts without human intervention.

Running burn-in today is painful and inadequate:

- **Manual and tedious.** Burn-in is done by hand: operators launch jobs, watch logs, track pass/fail per node in spreadsheets, and repeat across every cluster. No structured metrics, no programmatic status.
- **Existing Go test validation falls short.** Tests run short workloads that exit in minutes. They don't exercise sustained training over hours, don't handle mid-run failure and checkpoint restart, and don't use PVCs for model-data interaction.
- **No automation path.** New models and recipes must be added manually. A controller with a catalog API enables downstream systems (e.g., downstream orchestration systems) to register and trigger certifications programmatically.

The problem breaks down into three parts:

1. **Running workloads.** Certification requires real distributed training jobs (NCCL benchmarks, Nemotron pre-training, etc.) across groups of nodes. Multiple workload frameworks exist (Kubeflow Trainer, Training Operator) and the system must work with all of them.
2. **Orchestrating campaigns.** A full certification involves multiple categories, each potentially running across different node groups (e.g., by NVLink clique), with multiple iterations per group, and with ordering and isolation constraints.
3. **Detecting and acting on failures.** Hardware failures must be detected during execution, not just after. Failed nodes must be identified and the cluster state updated to prevent scheduling production workloads on bad hardware.

## Decision

Build the cluster-readiness-engine as layered Kubernetes CRDs following the Deployment → ReplicaSet → Pod composition pattern: **Certification** composes a burn-in suite, **Workflow** orchestrates iterations across node groups, and **Job** runs a single workload with health monitoring. Measurement APIs (GoodputMeasurement, BandwidthMeasurement) are opt-in child resources that Jobs create when configured, keeping measurement concerns out of the core orchestration path. A Remediation CRD handles post-failure node isolation.

## Implementation

Single Go binary built on controller-runtime with five reconcilers: Certification, Workflow, Job, GoodputMeasurement, and Remediation.

### CRD hierarchy

Certification creates Workflows from a catalog, Workflow creates Jobs, Job creates workloads via an adapter pattern supporting five frameworks (TrainJob, PyTorchJob, MPIJob, TFJob, JAXJob). Status propagates upward; failed node lists accumulate at each tier.

**Certification** is the push-button entry point. The controller taints target nodes for isolation, looks up each category in the catalog, and creates one Workflow per category. On failure, it creates a Remediation resource listing failed nodes.

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-certification
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB300
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron
```

**Workflow** orchestrates multi-node, multi-iteration execution. Takes a `jobTemplate`, applies orchestration targets to each Job's workload, and creates N sequential Jobs per group. Failed nodes accumulate across iterations. Supports topology-aware node grouping and dependency resources with configurable cleanup policies.

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Workflow
metadata:
  name: nemotron-6-8b-burnin
spec:
  jobTemplate:
    spec:
      workload:
        trainJob:
          runtimeRef: {kind: TrainingRuntime, name: nemotron-6-8b-runtime}
          trainer: {numNodes: 8, numProcPerNode: 4}
      nodeHealthMonitor:
        cel:
          expression: "node.spec.unschedulable == true"
      goodputMeasurement:
        logProfileRef: megatron-training
        sampleInterval: 30s
  orchestration:
    iterations: 3
    target:
      nodeSelector:
        nvidia.com/gpu.product: NVIDIA-GB200
    topology:
      topologyKey: nvidia.com/gpu.clique
```

**Job** is the lowest level. The controller creates a workload via the adapter pattern, evaluates CEL health expressions against nodes running the Job's pods, and manages checkpoint restart and stall detection.

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Job
metadata:
  name: nemotron-6-8b-burnin
spec:
  workload:
    trainJob:
      runtimeRef:
        kind: TrainingRuntime
        name: nemotron-6-8b-runtime
      trainer:
        image: nvcr.io/nv-ngc-devops/nemo:25.07
        command: [/bin/bash, -c]
        args:
          - exec torchrun --nnodes 8 --nproc-per-node 4
            /workspace/pretrain.py --save /checkpoints
        numNodes: 8
        numProcPerNode: 4
  nodeHealthMonitor:
    cel:
      expression: >-
        node.spec.unschedulable == true ||
        node.status.conditions.exists(c,
          c.type == 'GpuMemWatch' && c.status == 'True')
  goodputMeasurement:
    logProfileRef: megatron-training
    sampleInterval: 30s
  checkpoint:
    pvcName: training-checkpoint
    maxRestarts: 3
  stallMultiplier: 10
```

**GoodputMeasurement** tracks training efficiency for a referenced Job. The controller parses pod logs using a cluster-scoped LogProfile, computes goodput ratio, and publishes stall detection metrics that the Job controller reads.

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: GoodputMeasurement
metadata:
  name: nemotron-6-8b-burnin-goodput
spec:
  jobRef:
    apiGroup: cre.nvidia.com
    kind: Job
    name: nemotron-6-8b-burnin
  logProfileRef: megatron-training
  sampleInterval: 30s
```

**Remediation** is auto-created by Certification on failure. The controller taints each failed node (`cre.nvidia.com/preflight-failed:NoExecute`), cordons it, and sets a node condition. Deleting the Remediation reverses all actions, enabling re-certification after repair.

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Remediation
metadata:
  name: gpu-cluster-certification-remediation
spec:
  reason: "Hardware failure detected during burn-in"
  evidence: "Certification gpu-cluster-certification failed"
  nodes:
    - gpu-node-042
    - gpu-node-117
```

### Key components

- **Workload adapter pattern** (`pkg/workload/`): `Adapter` interface isolates framework-specific logic. `ForSpec()` factory selects the adapter based on which `WorkloadSpec` field is set (TrainJob, PyTorchJob, MPIJob, TFJob, JAXJob).
- **Catalog registration** (`pkg/catalog/`): Maps `{domain, variant}` to WorkflowSpec builders via `init()`. Adding a category means adding one Go file.
- **CEL-based health monitoring** (`pkg/nodemonitor/`): Evaluates CEL expressions against Node objects at reconciliation time. Consumes signals from NVSentinel, gpud, or any system that writes node conditions.
- **GoodputMeasurement** (`pkg/goodput/`): Tracks checkpoint save overhead and persists interruption state in CRD status to survive controller restarts.
- **Node isolation:** `NoExecute` taint on target nodes before creating Workflows. Finalizers guarantee cleanup on deletion.
- **Controller patterns:** Event-driven `Owns()` watches, exclusive conditions (InProgress/Succeeded/Failed), no in-memory state, leader election, least-privilege RBAC.

## Consequences

**Positive:** Users operate at the abstraction level that fits their needs. New workload types and categories require no controller code changes. Hardware failures are detected during execution, not just post-completion. Checkpoint restart and stall detection reduce wasted GPU-hours. Remediation closes the loop from detection to isolation without manual intervention.

**Negative:** Resource proliferation (~22 objects for a 3-category, 3-iteration Certification). Status propagation latency across reconciliation cycles. Log-based goodput depends on regex parsing. CEL correctness is the operator's responsibility.

**Mitigations:** Certification-level conditions aggregate the full picture. `Owns()` watches provide near-immediate propagation. LogProfile's `example` field serves as a contract. CEL is syntax-validated at admission.

## Alternatives Considered

**Single monolithic CRD.** Rejected: enormous spec, most fields irrelevant per use case, forces one abstraction level, harder to evolve independently.

**Pipeline/DAG system (Argo, Tekton).** Rejected: designed for sequential tasks, not continuous reconciliation. A 12-hour burn-in needs continuous health monitoring and stall detection throughout, not a "check health" step at the end.

**Go tests in a CI pipeline.** Rejected: burn-in runs for hours. A runner slot held that long is fragile. No CRD API surface for programmatic integration. Building continuous monitoring into a test harness converges on reimplementing controller-runtime.

**Hardcoded health checks.** Rejected: GPU health signals vary across hardware generations and monitoring setups. CEL enables composability with any health system without controller code changes.

## ClusterMAX™ 2.0 Alignment

The controller's catalog and adapter architecture supports all four ClusterMAX™ 2.0 performance validation categories:

| ClusterMAX Category | Controller Support | Status |
| --- | --- | --- |
| Provisioning & Cold-Start Efficiency | Catalog entries for package install latency, container pull timing, and storage-to-GPU bandwidth benchmarks | Supported |
| Networking & Fabric Validation | `communication/nccl-*` categories for collective benchmarks; GPUDirect RDMA checks via CEL or dedicated catalog entries | Supported |
| Raw Compute & Thermal Stress | GEMM benchmarks via TrainJob; thermal/power monitoring via CEL-evaluated node conditions from NVSentinel/gpud | Supported |
| Real-World Workload Simulation | Multi-node pretraining (with GoodputMeasurement). Single-node RL, multi-node inference, and finetuning planned | Partial — RL, inference, and finetuning planned |

## Notes

### Non-Goals

- **Replacing platform readiness checks.** The controller runs after infrastructure checks and node provisioning gates, not instead of them.
- **GPU monitoring or diagnostics.** Consumes signals from NVSentinel/gpud via node conditions and taints. Does not implement its own.
- **Workload scheduling.** Relies on Kubernetes scheduling. Does not implement its own scheduler.
- **Multi-cluster orchestration.** Single-cluster only. Cross-cluster campaigns are out of scope.
- **Workload framework installation.** Requires Kubeflow Trainer or Training Operator pre-installed.
- **Prescribing workloads or pass/fail criteria.** The controller orchestrates and monitors. What to run and what constitutes a pass is defined by SMEs via the catalog and CEL.

### Additional

- `cre.nvidia.com` API group with `v1alpha1`. Breaking changes expected before `v1beta1`.
- Catalog supports Go-code registration only (`init()`). Adding a category requires a new Go file and a controller rebuild.
- The controller is not in the critical path for tenant workloads. If unavailable, existing workloads and taints persist (fail-safe).
- Controller upgrades are non-disruptive. Brief reconciliation pause during pod replacement, then resumes from persisted state.
- GoodputMeasurement reads pod logs in real-time for metric extraction. Long-term log storage is the operator's responsibility.
- NVSentinel is experimental. Integration degrades gracefully if not deployed.

## References

- [Kubeflow Trainer v2](https://github.com/kubeflow/trainer)
- [Kubeflow Training Operator](https://github.com/kubeflow/training-operator)
- [CEL in Kubernetes](https://kubernetes.io/docs/reference/using-api/cel/)
- [NVSentinel](https://github.com/NVIDIA/NVSentinel)
- [gpud](https://github.com/leptonai/gpud)
- [ClusterMAX™ 2.0](https://newsletter.semianalysis.com/p/clustermax-20-the-industry-standard)

## Change Log

- 2026-02-10: Initial proposal
- 2026-02-16: Abridged from full architecture document
