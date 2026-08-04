---
title: Platform Detection & Overrides
description: How the controller auto-detects cloud platform and GPU architecture, and how overrides are applied.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## Auto-detection

The controller detects two dimensions at runtime:

| Dimension | Source |
|-----------|--------|
| Cloud platform | `spec.providerID` on the Node object (e.g. `aws://...`, `gce://...`) |
| GPU architecture | `nvidia.com/gpu.product` node label (e.g. `NVIDIA-GB200`, `NVIDIA-H100-80GB-HBM3`) |

Detection results are stamped on Workflow objects as annotations (`detected-gpu-architecture`, `detected-platform`) so you can verify what matched.

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

Different GPU architectures require different Kubernetes resources:

| Architecture | Interconnect | Key resources |
|-------------|-------------|--------------|
| GB200 | EFA (AWS) | `hugepages-2Mi`, `vpc.amazonaws.com/efa`, EFA hostPath volume |
| GB300 | RoCE (AWS) | `roce-channel` resource claim, no hugepages, no EFA |
| H100 | EFA (AWS) | `vpc.amazonaws.com/efa: 32`, no hugepages |

The applied-overrides annotation on the Workflow lists which overrides matched, making it straightforward to debug unexpected configurations.
