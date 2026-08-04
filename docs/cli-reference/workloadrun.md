---
title: xcalctl workloadrun
description: Run and report WorkloadRun resources.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## xcalctl workloadrun run

Applies a WorkloadRun manifest, optionally waits for completion, and streams logs.

```bash
xcalctl workloadrun run [flags] <file>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image-pull-secret` | — | NGC pull secret name |
| `--wait` | `false` | Block until the workload completes |
| `--timeout` | `1h` | Maximum wait time |
| `--logs` | `false` | Stream pod logs during the run |
| `--platform` | auto | Override platform detection |

### Example

```bash
xcalctl workloadrun run \
  --image-pull-secret ngc-secret \
  --wait \
  my-workload.yaml
```

## xcalctl workloadrun report

Prints the results for a completed WorkloadRun, including bandwidth and goodput metrics if configured.

```bash
xcalctl workloadrun report <name> [--output json|table]
```

## xcalctl workloadrun list

Lists all WorkloadRun resources in the namespace.

```bash
xcalctl workloadrun list
```

## xcalctl workloadrun delete

Deletes a WorkloadRun and its child resources.

```bash
xcalctl workloadrun delete <name>
```
