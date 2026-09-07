---
title: nvcrectl workflow
description: Render Workflow manifests offline with overrides applied.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl workflow render

Renders a Workflow manifest with platform and GPU overrides applied, without connecting to a cluster. Useful for inspecting what the controller would create or diffing override changes.

```bash
nvcrectl workflow render [flags] <workflow.yaml>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Target platform (`aws`, `gcp`, `azure`, `oci`, `mistral`, `forge`) |
| `--gpu-arch` | auto | Target GPU architecture with built-in mock-node templates (`h100`, `gb200`, `gb300`); other architectures are supported via `--nodes-file` or `--dry-run` (auto-detected from the cluster) |
| `--nodes-file` | — | Path to a YAML file of `corev1.Node` objects for offline rendering (mutually exclusive with `--platform`/`--gpu-arch`) |
| `--output` | `yaml` | Output format: `yaml` or `json` |
| `--dry-run` | `false` | Validate against the live API server without creating resources |

### Example

```bash
# Render with explicit platform and GPU arch overrides
nvcrectl workflow render \
  --platform aws \
  --gpu-arch h100 \
  my-workflow.yaml
```
