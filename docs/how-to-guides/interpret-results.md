---
title: Interpret Results
description: Read and act on certification and WorkloadRun reports.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Get a report

```bash
nvcrectl certification report <name>
nvcrectl workloadrun report <name>
```

## Report structure

| Section | Contents |
|---------|---------|
| Summary | Overall pass/fail, node count, run duration |
| Category results | Per-domain/variant measured vs. expected values with pass/fail |
| Node results | Per-node breakdown — which passed, which failed, and the failure reason |

## Status values

| Status | Meaning |
|--------|---------|
| `Passed` | All categories met thresholds on all node groups |
| `Failed` | One or more categories failed; affected nodes listed in ConfigMaps referenced by `status.categoryStatuses[].failedNodesRef` |
| `InProgress` | Still running |

## Bandwidth results

- **Measured bus bandwidth** (GB/s) per collective
- **Expected threshold** for the detected GPU architecture
- **Pass/fail** per operation

Below-threshold results indicate a network issue — degraded link, misconfigured EFA/RoCE, or faulty NIC.

## Goodput results

- **Goodput ratio** (0.0–1.0) — fraction of time the job was making useful forward progress
- **Expected minimum** from the catalog entry
- A ratio below ~0.95 suggests stalls, slow nodes, or framework overhead

## Failed nodes

Failed nodes are stored in a ConfigMap referenced by `status.categoryStatuses[].failedNodesRef` for each category. The Job tier records them inline on `status.failedNodes`; the Workflow and Certification tiers persist them to ConfigMaps.

```bash
# Get the ConfigMap name for the first category
kubectl get certification <name> -o jsonpath='{.status.categoryStatuses[0].failedNodesRef.name}'
# Read the ConfigMap contents
kubectl get configmap <ref-name> -o yaml
```

Or use the CLI for a formatted report:

```bash
nvcrectl certification report <name>
```

CRE records which nodes failed and why — it does not taint or cordon them. To quarantine a failed node:

```bash
kubectl cordon <node>    # prevent new workloads
kubectl drain <node>     # evict existing workloads
```

After repair, uncordon the node and re-run the relevant category to confirm it passes.
