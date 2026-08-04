---
title: API Reference Overview
description: Kubernetes CRD reference for the Cluster Readiness Engine.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


The Cluster Readiness Engine defines the following custom resources under the `cre.nvidia.com/v1alpha1` API group.

| Resource | Scope | Purpose |
|----------|-------|---------|
| [Certification](./certification.md) | Namespaced | Top-level certification suite |
| [Workflow](./workflow.md) | Namespaced | Single-category test run |
| [Job](./job.md) | Namespaced | Workload executor and health monitor |
| [WorkloadRun](./workloadrun.md) | Namespaced | Simplified ad-hoc workload API |
| [GoodputMeasurement](./goodput-measurement.md) | Namespaced | Log-based training throughput measurement |
| [BandwidthMeasurement](./bandwidth-measurement.md) | Namespaced | NCCL bandwidth measurement |
| [LogProfile](./logprofile.md) | Cluster-scoped | Regex patterns for log parsing |

## Field reference

The per-resource pages below are generated from the CRD OpenAPI schemas using [`crd-ref-docs`](https://github.com/elastic/crd-ref-docs). To regenerate locally:

```bash
make api-docs
```

_Generated field reference coming soon — CRD schemas are defined in `api/v1alpha1/`._
