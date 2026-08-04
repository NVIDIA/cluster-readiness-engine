---
title: ncrectl workloadrun
description: Run and report WorkloadRun resources.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## ncrectl workloadrun run

Applies a WorkloadRun manifest, optionally waits for completion, and streams logs.

```bash
ncrectl workloadrun run [flags] <file>
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
ncrectl workloadrun run \
  --image-pull-secret ngc-secret \
  --wait \
  my-workload.yaml
```

## ncrectl workloadrun report

Prints the results for a completed WorkloadRun, including bandwidth and goodput metrics if configured.

```bash
ncrectl workloadrun report <name> [--output json|table]
```

## ncrectl workloadrun list

Lists all WorkloadRun resources in the namespace.

```bash
ncrectl workloadrun list
```

## ncrectl workloadrun delete

Deletes a WorkloadRun and its child resources.

```bash
ncrectl workloadrun delete <name>
```
