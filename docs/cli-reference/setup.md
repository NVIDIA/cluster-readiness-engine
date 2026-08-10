---
title: ncrectl setup
description: Install and uninstall the Cluster Readiness Engine controller and its dependencies.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## ncrectl setup init

Installs CRE via Helm and its dependencies on the target cluster.

```bash
ncrectl setup init [flags]
```

### What it installs

Runs two phases in order:

| Phase | What |
|-------|------|
| `deps` | Kubeflow Trainer v2.2.0 |
| `helm` | CRE Helm chart (CRDs, controller, built-in LogProfiles) pulled from GHCR |

Use `--skip-phases=deps` to skip Kubeflow Trainer if it is already installed.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image-pull-secret` | — | GitHub token — the CLI creates a `ghcr.io` pull secret and uses it to authenticate the Helm chart pull |
| `--image` | — | Override the controller image (default: `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`) |
| `--skip-phases` | — | Comma-separated phases to skip (e.g., `deps`) |
| `--version` | — | Helm chart version to install (required for dev builds) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt (for CI/automation) |

### Example

```bash
# Standard install
ncrectl setup init

# With GHCR authentication
ncrectl setup init --image-pull-secret $GITHUB_TOKEN

# Skip Kubeflow Trainer (already installed)
ncrectl setup init --skip-phases=deps
```

## ncrectl setup status

Shows the current installation status of each component.

```bash
ncrectl setup status
```

## ncrectl setup reset

Removes CRE and its dependencies from the target cluster. Kubeflow Trainer is removed by default.

```bash
ncrectl setup reset [flags]
```

### What it removes

Runs three phases in order:

| Phase | What |
|-------|------|
| `cr` | All CRE custom resource instances (Certifications, Workflows, Jobs) |
| `helm` | CRE Helm release (CRDs, controller, LogProfiles) |
| `deps` | Kubeflow Trainer |

Use `--skip-phases=deps` to keep Kubeflow Trainer.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--skip-phases` | — | Comma-separated phases to skip (e.g., `deps`) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt |

### Example

```bash
# Full uninstall (including Kubeflow Trainer)
ncrectl setup reset

# Keep Kubeflow Trainer
ncrectl setup reset --skip-phases=deps
```

<Warning>
`reset` deletes all Certification, Workflow, and Job resources. This is irreversible.
</Warning>
