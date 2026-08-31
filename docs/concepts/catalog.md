---
title: Catalog
description: How the certification catalog maps domain/variant pairs to workload specs.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


The catalog is the registry of all supported certification categories. It maps `{domain, variant}` pairs to workload definitions — the concrete specs that run during certification.

## Structure

Each catalog entry is a YAML file at `pkg/catalog/entries/<domain>/<variant>.yaml`. The file defines the base workload spec (dependencies, job template, orchestration) plus platform- and GPU-specific overrides. The catalog loader discovers entries by scanning that directory tree at startup.

## Domains and variants

| Domain | Variant | What it tests |
|--------|---------|---------------|
| `communication` | `nccl-all-reduce` | All-reduce collective bandwidth |
| `communication` | `nccl-all-gather` | All-gather collective bandwidth |
| `communication` | `nccl-alltoall` | All-to-all collective bandwidth |
| `communication` | `nccl-loopback` | Loopback bandwidth (single-node NVLink/NVSwitch) |
| `communication` | `nccl-loopback-nvswitch` | Loopback bandwidth via NVSwitch fabric |
| `diagnostics` | `dcgm-level4` | DCGM level-4 diagnostics |
| `training` | `nemotron5-8b` | End-to-end training throughput (NeMo, Nemotron 5 8B) |
| `training` | `nemotron5-56b` | End-to-end training throughput (NeMo, Nemotron 5 56B) |

## Selecting categories

In a `Certification` spec, list any combination of domain/variant pairs under `categories`:

```yaml
spec:
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
```

To see all available categories from the CLI:

```bash
nvcrectl certification list-categories
```

## Per-node category options

`nccl-loopback`, `nccl-loopback-nvswitch`, and `dcgm-level4` run one Job per node. They support two additional options:

| Option | Description |
|--------|-------------|
| `maxConcurrent` | Maximum number of per-node Jobs to run simultaneously (default: unbounded) |
| `timeoutPerJob` | Timeout for each individual per-node Job |

```yaml
categories:
  - domain: diagnostics
    variant: dcgm-level4
    options:
      maxConcurrent: 4
      timeoutPerJob: 30m
```

## Training category options

The training entries (`nemotron5-8b`, `nemotron5-56b`) size their containers for DGX-class nodes by default: limits of `cpu: "128"` / `memory: 800Gi` and requests of `cpu: "64"` / `memory: 500Gi`. On smaller GPU nodes those defaults make training pods unschedulable (`Insufficient cpu` / `Insufficient memory`), so the CPU and memory values can be overridden with the `resources` option:

```yaml
categories:
  - domain: training
    variant: nemotron5-8b
    options:
      resources:
        limits:
          cpu: "6"
          memory: 48Gi
        requests:
          cpu: "4"
          memory: 32Gi
```

Each of the four values independently falls back to its default when unset. Values are standard Kubernetes resource quantities. The GPU count is not part of `resources` — it is controlled by `gpusPerNode`. Like all category options, `resources` can also be set at the `Certification` spec level as a global default for every category; a per-category `resources` block replaces the global one entirely (the two are not merged field by field). Non-training entries ignore it.

Note: platform-specific catalog overrides may still supersede these values. On AWS with H100 GPUs, the training entries apply an instance-sized override (limits `cpu: "192"` / `memory: 1800Gi`, requests `cpu: "128"` / `memory: 1200Gi` for p5-class instances) that takes precedence over `resources`.

## Overrides

Catalog entries define a base workload spec. Platform-specific and GPU-specific overrides within the same YAML are applied at render time based on the detected environment. Supported GPU architectures include GB200, GB300, H100, H200, and B200. See [Platform Detection & Overrides](./platform-detection.md) for override semantics.

## Adding a custom entry

See [How-to: Custom Catalog Entries](../how-to-guides/custom-catalog-entries.md).
