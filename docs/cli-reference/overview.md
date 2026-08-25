---
title: ncrectl Overview
description: The ncrectl CLI manages the full lifecycle of cluster certification and WorkloadRun.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`ncrectl` is the command-line interface for the Cluster Readiness Engine. It handles installation, certification lifecycle, workload execution, and reporting — without requiring direct `kubectl` access for most operations.

## Install

While this repository is internal, release assets require authenticated API
downloads, so fetch the installer with the gh CLI (`gh auth login` first):

```bash
export NCRECTL_VERSION=v0.1.0-rc.9
gh release download "${NCRECTL_VERSION}" --repo dsx-ai-factory/cluster-readiness-engine \
  --pattern installer --output - | bash -s -- -v "${NCRECTL_VERSION}"
ncrectl --version
```

The installer also creates a `kubectl-ncre` symlink so the CLI is available as a kubectl plugin (`kubectl ncre ...`).

## Command groups

| Command group | Purpose |
|--------------|---------|
| `ncrectl setup` | Install and uninstall the controller and its dependencies |
| `ncrectl certification` | Run, render, report, and list-categories for Certification resources |
| `ncrectl workloadrun` | Run, render, report, status, and cancel WorkloadRun resources |
| `ncrectl cluster` | Inspect GPU nodes, platform, and network topology |
| `ncrectl workflow` | Render Workflow manifests offline with overrides applied |

## Global flags

These flags are available on subcommands that connect to a cluster:

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig (defaults to `~/.kube/config`) |
| `--context` | Kubernetes context to use |
| `--namespace` / `-n` | Namespace override (not available on all subcommands) |

## Shell completion

```bash
ncrectl completion bash >> ~/.bashrc
ncrectl completion zsh >> ~/.zshrc
```
