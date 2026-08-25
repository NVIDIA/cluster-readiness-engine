---
title: WorkloadRun Quick Start
description: Run a distributed training or NCCL workload without writing a full Certification spec.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`WorkloadRun` is a simplified API for running a single workload against a set of nodes. It is useful for one-off benchmarks, smoke tests, and validating specific node groups without running the full certification suite.

**When to use WorkloadRun vs Certification:** Use `WorkloadRun` when you want to run a single workload — for example, a quick NCCL bandwidth check or a training smoke test — against a specific set of nodes. Use `Certification` when you need a full burn-in suite across all categories and node groups, with per-node pass/fail results and a structured report.

## Before you begin

[Install ncrectl and set up the cluster](./install.md) before continuing.

## Define a WorkloadRun

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

## Run it

```bash
ncrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  --wait \
  nccl-all-reduce.yaml
```

ncrectl auto-detects the platform and GPU architecture from the cluster's node labels, applies the appropriate overrides, and streams log output until the workload completes.

## View results

```bash
ncrectl workloadrun report nccl-all-reduce
```

For workloads with `bandwidthMeasurement` configured, the report includes per-bus bandwidth results parsed from the NCCL output.

## Next steps

- [Concepts: Workload Types](../concepts/workload-types.md)
- [How-to: Run a WorkloadRun](../how-to-guides/run-workloadrun.md)
- [API Reference: WorkloadRun](../api-reference/workloadrun.md)
