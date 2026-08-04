---
title: ncrectl certification
description: Manage the full lifecycle of Certification resources.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## ncrectl certification run

Applies a Certification manifest, waits for completion, prints the report, and optionally cleans up.

```bash
ncrectl certification run --cert-file <file> [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--cert-file` | — | Path to the Certification YAML (**required**) |
| `--image-pull-secret` | — | NGC pull secret name |
| `--wait` | `false` | Block until the certification completes |
| `--timeout` | `2h` | Maximum wait time |
| `--cleanup` | `false` | Delete the Certification after completion |
| `--platform` | auto | Override platform detection (`aws`, `gcp`, `azure`) |

### Example

```bash
ncrectl certification run \
  --cert-file certification.yaml \
  --image-pull-secret ngc-secret \
  --wait
```

## ncrectl certification render

Renders the Workflow manifests that would be created for a given Certification, without applying them. Useful for inspecting override application and resource requests before running.

```bash
ncrectl certification render [--platform <platform>] [--dry-run] <cert-file>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Override platform detection |
| `--dry-run` | `false` | Validate against the live API server without creating resources |

## ncrectl certification report

Prints the pass/fail report for a completed Certification.

```bash
ncrectl certification report <name> [--namespace <ns>] [--output json|table]
```

## ncrectl certification list

Lists all Certification resources in the namespace with their current status.

```bash
ncrectl certification list
```

## ncrectl certification delete

Deletes a Certification and all its child resources (Workflows, Jobs, Remediations).

```bash
ncrectl certification delete <name>
```
