---
title: Certification
description: CRD reference for the Certification resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`Certification` is the top-level resource that defines a suite of certification categories to run against a GPU node pool.

## Example

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-cert
  namespace: cluster-readiness-engine
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  enableMNNVL: false
  imagePullSecrets:
    - name: ngc-secret
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron4-15b
      options:
        maxSteps: 50
        nodesPerJob: 8
  options:
    adaptiveFaultIsolation: true
```

## Spec fields

_Generated from CRD schema — coming soon._

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | InProgress, Succeeded, Failed (mutually exclusive) |
| `categoryStatuses` | []CertificationCategoryStatus | Per-category status including `domain`, `variant`, `status`, and `failedNodes` |

Each `categoryStatuses` entry includes a `failedNodes` list. Each entry in that list has a `name` (node name) and a `reason` (`HardwareFailureDetected`, `ThresholdViolation`, or `WorkloadFailed`).

## Lifecycle

1. Controller creates one `Workflow` per entry in `spec.categories`.
2. Workflows run sequentially or in parallel depending on orchestration config.
3. When all Workflows complete, Certification is marked `Succeeded` or `Failed`.
4. Failed nodes are recorded at `status.categoryStatuses[].failedNodes`. CRE does not taint or cordon nodes.
