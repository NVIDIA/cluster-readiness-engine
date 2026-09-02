---
title: Platform Detection & Overrides
description: How the controller auto-detects cloud platform and GPU architecture, and how overrides are applied.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Auto-detection

The controller detects two dimensions at runtime:

| Dimension | Source |
|-----------|--------|
| Cloud platform | `spec.providerID` on the Node object (e.g. `aws://...`, `gce://...`) |
| GPU architecture | `nvidia.com/gpu.product` node label (e.g. `NVIDIA-GB200`, `NVIDIA-H100-80GB-HBM3`) |

On a mixed-architecture target, the detected GPU architecture is the one reported by the most nodes, with ties resolved to the architecture whose earliest node sorts first by name. Nodes missing the `nvidia.com/gpu.product` label do not participate in that vote, so an unlabeled node never outvotes labeled ones; `unknown` is detected only when no target node carries the label. Every path uses the same rule: the Certification, Workflow, and WorkloadRun controllers as well as the `nvcrectl` render, cluster info, and workloadrun commands.

The live controller writes detection results to `status.orchestration.detectedPlatform` and `status.orchestration.detectedGPUArchitecture` on the Workflow. When using `nvcrectl workflow render` (client-side), these values are also written as annotations (`nvcrectl.nvidia.com/detected-platform`, `nvcrectl.nvidia.com/detected-gpu-architecture`) on the rendered manifest for offline inspection.

## Override matching

Catalog entries define a base `WorkflowSpec`. Overrides are matched by platform + GPU architecture and applied in order. Two patch mechanisms are supported:

### `jobTemplate` (strategic merge patch)

Replaces entire arrays. Use when you want to set all values for a field (e.g. the full `env` list):

```yaml
jobTemplate:
  spec:
    workload:
      trainJob:
        trainer:
          env:
            - name: MY_VAR
              value: "value"
```

<Warning>
Arrays in `jobTemplate` overrides are **replaced entirely**, not merged by name. If the base spec has env vars, they will be wiped unless you include them in the override.
</Warning>

### `jobTemplatePatch` (RFC 6902 JSON Patch)

Appends or modifies individual fields without replacing arrays. Use `op: add` with path ending in `/-` to append to an array:

```yaml
jobTemplatePatch:
  - op: add
    path: /spec/workload/trainJob/trainer/env/-
    value:
      name: EXTRA_VAR
      value: "extra"
```

<Warning>
A `jobTemplatePatch` that appends to an array must come **after** any `jobTemplate` override that sets that array, or the appended values will be wiped.
</Warning>

## Architecture-specific resources

Different GPU architectures and cloud platforms require different Kubernetes resources:

| Architecture | Platform | Interconnect | Key resources |
|-------------|---------|-------------|--------------|
| GB200 | AWS | EFA | `hugepages-2Mi`, `vpc.amazonaws.com/efa`, EFA hostPath volume, ComputeDomain |
| GB200 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| GB300 | AWS | RoCE | `roce-channel` resource claim (DRA), no hugepages, no EFA |
| GB300 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| H100 | AWS | EFA | `vpc.amazonaws.com/efa: 32`, no hugepages |
| H100 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap |

The live controller tracks which overrides matched in `status.orchestration.appliedOverrides`. When using `nvcrectl workflow render`, the same information is also written to the `nvcrectl.nvidia.com/applied-overrides` annotation on the rendered manifest.
