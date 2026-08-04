---
title: WorkloadRun
description: CRD reference for the WorkloadRun resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


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
  numNodes: 4
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
  goodputMeasurement:
    logProfileRef: nemo-training
```

## Spec fields

_Generated from CRD schema — coming soon._

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `InProgress`, `Passed`, or `Failed` |
| `conditions` | []Condition | InProgress, Succeeded, Failed (mutually exclusive) |
| `bandwidthResult` | BandwidthResult | Measured bandwidth results (if configured) |
| `goodputResult` | GoodputResult | Measured goodput ratio (if configured) |
| `detectedGPUArchitecture` | string | Detected GPU architecture |
| `detectedPlatform` | string | Detected cloud platform |
