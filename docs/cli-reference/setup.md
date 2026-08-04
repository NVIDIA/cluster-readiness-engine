---
title: xcalctl setup
description: Install and uninstall the Cluster Readiness Engine controller and its dependencies.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## xcalctl setup init

Installs all controller dependencies and deploys the controller.

```bash
xcalctl setup init [flags]
```

### What it installs

1. Kubeflow Trainer (required for `TrainJob` workloads)
2. Cluster Readiness Engine CRDs
3. Controller deployment
4. Built-in LogProfiles for supported frameworks

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image-pull-secret` | — | Name of an existing pull secret for NGC images |
| `--namespace` | `cluster-readiness-engine` | Namespace to install into |
| `--dry-run` | `false` | Print resources without applying |

### Example

```bash
xcalctl setup init --image-pull-secret ngc-secret
```

## xcalctl setup status

Shows the current installation status of each component.

```bash
xcalctl setup status
```

## xcalctl setup reset

Uninstalls the controller and CRDs. Does not remove Kubeflow Trainer.

```bash
xcalctl setup reset [--namespace cluster-readiness-engine]
```

<Warning>
`reset` deletes all Certification, Workflow, Job, and Remediation resources in the namespace. This is irreversible.
</Warning>
