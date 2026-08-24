---
title: Run NCCL Benchmarks
description: Measure collective operation bandwidth across your GPU cluster's interconnect fabric.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Via Certification (recommended)

```yaml
spec:
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: communication
      variant: nccl-all-gather
    - domain: communication
      variant: nccl-alltoall
```

The controller measures bus bandwidth for each collective and compares against per-architecture thresholds.

## Via WorkloadRun (ad hoc)

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
      mpirunPath: /usr/local/mpi/bin/mpirun
  numNodes: 4
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
```

## Interpreting results

The report shows measured bus bandwidth (GB/s) versus the expected threshold for the detected GPU architecture. Results below threshold indicate a network problem — degraded links, misconfigured EFA/RoCE, or a faulty NIC.

_Threshold table by architecture coming soon._
