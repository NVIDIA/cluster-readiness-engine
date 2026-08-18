---
title: ncrectl workflow
description: Render Workflow manifests offline with overrides applied.
---
<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->


## ncrectl workflow render

Renders a Workflow manifest with platform and GPU overrides applied, without connecting to a cluster. Useful for inspecting what the controller would create or diffing override changes.

```bash
ncrectl workflow render [flags] <workflow.yaml>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Target platform (`aws`, `gcp`, `azure`, `oci`, `mistral`, `forge`) |
| `--gpu-arch` | auto | Target GPU architecture (`h100`, `gb200`, `gb300`) |
| `--nodes-file` | — | Path to a file listing node names (one per line) for topology-aware rendering |
| `--output` | `yaml` | Output format: `yaml` or `json` |
| `--dry-run` | `false` | Validate against the live API server without creating resources |

### Example

```bash
# Render with explicit platform and GPU arch overrides
ncrectl workflow render \
  --platform aws \
  --gpu-arch h100 \
  my-workflow.yaml
```
