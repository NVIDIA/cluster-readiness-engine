---
title: WorkloadRun
description: CRD reference for the WorkloadRun resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`WorkloadRun` is a user-facing simplified API for running ad-hoc distributed workloads without going through the full Certification pipeline.

## Example

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nccl-all-reduce
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
      mpirunPath: /usr/local/mpi/bin/mpirun
  numNodes: 4   # nodes per job group; all eligible nodes are partitioned into 4-node jobs
  gangScheduler:
    schedulerName: kai-scheduler
    queue: high-priority
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
  goodputMeasurement:
    logProfileRef: megatron-training
```

## Spec fields

<Warning>
The entire `spec` is **immutable** after the WorkloadRun is created (a `self == oldSelf` transition rule on the CRD rejects every update with `spec is immutable after creation`). Once `status.workflowRef` is set the controller only mirrors the existing Workflow and never rebuilds it, so mutable fields would be silently ignored. To run with different inputs, delete the WorkloadRun and create a new one.
</Warning>

_Generated from CRD schema — coming soon. Fields documented so far:_

| Field | Type | Description |
|-------|------|-------------|
| `gangScheduler` | GangSchedulerSpec | Optional. Opts workload pods into a gang-aware scheduler such as KAI Scheduler. When set, the scheduler name is injected as `schedulerName` into every workload pod template (for MPI, both launcher and worker pods) and the queue is applied as the `kai.scheduler/queue` label on the pod template metadata, so the scheduler holds all pods until the entire gang can be placed |
| `gangScheduler.schedulerName` | string | Required; minimum length 1. Name of the gang-aware scheduler to use (e.g., `kai-scheduler`). Injected as `schedulerName` in each workload pod spec |
| `gangScheduler.queue` | string | Optional. Scheduler queue to submit the workload to; defaults to `default-queue` when unset. When non-empty, must be a valid Kubernetes label value: at most 63 characters, beginning and ending with an alphanumeric character, and containing only alphanumerics, hyphens, underscores, or dots (pattern `^$\|^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`) |

`numNodes` (shown in the example above) is the number of nodes **per job group**, not a total: the orchestrator partitions all eligible nodes into groups of that size.

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Exclusive set: `InProgress`, `Succeeded`, `Failed`. Independent (additive): `ValidationFailed` — mirrored from the Workflow whenever any Job violated a performance threshold. When the threshold miss is the failure cause, `Failed` carries reason `WorkflowValidationFailed` so a threshold miss is distinguishable from an execution failure |
| `workflowRef` | WorkflowReference | Reference to the underlying `Workflow` resource |
| `detectedGPUArchitecture` | string | Auto-detected GPU type (e.g., `h100`, `gb200`) |
| `detectedPlatform` | string | Auto-detected CSP platform (e.g., `aws`, `gcp`, `azure`) |
| `resolvedGpusPerNode` | int32 | Final GPU count per node used for the workload |
| `succeededNodesRef` | TypedLocalObjectReference | ConfigMap reference for the succeeded-nodes list |
| `failedNodesRef` | TypedLocalObjectReference | ConfigMap reference for the failed-nodes list |

Bandwidth and goodput measurement results are on the `BandwidthMeasurement` and `GoodputMeasurement` child resources, which reference this WorkloadRun's underlying Job via `spec.jobRef`.
