---
title: Catalog
description: How the certification catalog maps domain/variant pairs to workload specs.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


The catalog is the registry of all supported certification categories. It maps `{domain, variant}` pairs to `WorkflowSpec` builders — the concrete workload definitions that run during certification.

## Structure

Each catalog entry lives in `internal/catalog/entries/<domain>/<variant>/` and registers itself via a Go `init()` function. There is no config file or central manifest; adding a new category means adding a new Go file.

## Domains and variants

| Domain | Variant | What it tests |
|--------|---------|---------------|
| `communication` | `nccl-all-reduce` | All-reduce collective bandwidth |
| `communication` | `nccl-all-gather` | All-gather collective bandwidth |
| `communication` | `nccl-alltoall` | All-to-all collective bandwidth |
| `training` | `nemotron4-15b` | End-to-end training throughput (NeMo, 15B param model) |
| `training` | `nemotron6` | End-to-end training throughput (NeMo, Nemotron 6) |

_This table is a stub — full catalog reference TBD._

## Selecting categories

In a `Certification` spec, list any combination of domain/variant pairs under `categories`:

```yaml
spec:
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron4-15b
```

## Overrides

Catalog entries define a base `WorkflowSpec`. Platform-specific and GPU-specific overrides are applied at render time based on the detected environment. See [Platform Detection & Overrides](./platform-detection.md) for override semantics.

## Adding a custom entry

See [How-to: Custom Catalog Entries](../how-to-guides/custom-catalog-entries.md).
