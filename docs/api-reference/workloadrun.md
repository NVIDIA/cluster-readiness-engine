---
title: WorkloadRun
description: CRD reference for the WorkloadRun resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`WorkloadRun` is a user-facing simplified API for running ad-hoc distributed workloads without going through the full Certification pipeline.

## Example

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nccl-all-reduce
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
      mpirunPath: /usr/local/bin/mpirun
  numNodes: 4
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

_Generated from CRD schema — coming soon._

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Exclusive set: `InProgress`, `Succeeded`, `Failed`. Independent (additive): `ValidationFailed` (can be True alongside `Succeeded` — workload finished but violated a threshold) |
| `workflowRef` | WorkflowReference | Reference to the underlying `Workflow` resource |
| `detectedGPUArchitecture` | string | Auto-detected GPU type (e.g., `h100`, `gb200`) |
| `detectedPlatform` | string | Auto-detected CSP platform (e.g., `aws`, `gcp`, `azure`) |
| `resolvedGpusPerNode` | int32 | Final GPU count per node used for the workload |
| `succeededNodesRef` | TypedLocalObjectReference | ConfigMap reference for the succeeded-nodes list |
| `failedNodesRef` | TypedLocalObjectReference | ConfigMap reference for the failed-nodes list |

Bandwidth and goodput measurement results are on the `BandwidthMeasurement` and `GoodputMeasurement` child resources, which reference this WorkloadRun's underlying Job via `spec.jobRef`.
