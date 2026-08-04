---
title: Run a WorkloadRun
description: Run an ad-hoc distributed workload against a specific set of nodes.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`WorkloadRun` lets you run any distributed workload — training, NCCL benchmark, or custom script — without the full certification pipeline.

## Basic example

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: my-workload
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
  numNodes: 4
```

```bash
ncrectl workloadrun run --image-pull-secret ngc-secret --wait my-workload.yaml
```

## Targeting specific nodes

```yaml
spec:
  target:
    nodeSelector:
      kubernetes.io/hostname: gpu-node-01
```

## With bandwidth measurement

```yaml
spec:
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
```

## With goodput measurement

```yaml
spec:
  goodputMeasurement:
    logProfileRef: nemo-training
```

## View results

```bash
ncrectl workloadrun report my-workload
```

See [API Reference: WorkloadRun](../api-reference/workloadrun.md) for the full spec.
