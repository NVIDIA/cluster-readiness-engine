---
title: Adaptive Fault Isolation
description: Narrow down a failing node using bisection without re-running the full suite.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


When a certification run fails and multiple nodes are suspect, adaptive fault isolation bisects the node pool to identify the specific faulty node(s) with the minimum number of additional runs.

## How it works

1. The controller splits the suspect node pool in half.
2. It re-runs the failing category against each half.
3. The half that fails is bisected again.
4. This repeats until a single node (or domain boundary) is isolated.

For an N-node pool this requires at most log₂(N) additional runs instead of N−1.

## Enable

Set `testScale: diagnose` on the category you want to run in diagnose mode:

```yaml
spec:
  categories:
    - domain: communication
      variant: nccl-all-reduce
      options:
        testScale: diagnose
```

The diagnose algorithm runs in seven stages: `intra-screening`, `intra-screening-no-nvl`, `inter-screening`, `bisection`, `confirmation`, `cross-boundary`, and `complete`.

## Cross-boundary probing

When the fault lies at a network domain boundary (e.g. two nodes connected via a spine switch), standard bisection may not isolate it. Cross-boundary probing extends the algorithm to test node pairs that span domain boundaries.

## Reading the result

After isolation the `Certification` status records the isolated node(s) in the ConfigMap referenced by `status.categoryStatuses[].failedNodesRef`. See [Health Monitoring & Failed Node Attribution](../concepts/health-monitoring-remediation.md) for how to read and act on that data.
