---
title: nvcrectl Overview
description: The nvcrectl CLI manages the full lifecycle of cluster certification and WorkloadRun.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`nvcrectl` is the command-line interface for the NVIDIA Cluster Readiness Engine. It handles installation, certification lifecycle, workload execution, and reporting — without requiring direct `kubectl` access for most operations.

## Install

While this repository is internal, release assets require authenticated API
downloads, so fetch the installer with the gh CLI (`gh auth login` first):

```bash
export NVCRECTL_VERSION=$(gh release list --repo NVIDIA/cluster-readiness-engine --limit 1 --json tagName -q '.[0].tagName')
gh release download "${NVCRECTL_VERSION}" --repo NVIDIA/cluster-readiness-engine \
  --pattern installer --output - | bash -s -- -v "${NVCRECTL_VERSION}"
nvcrectl --version
```

The installer also creates a `kubectl-nvcre` symlink so the CLI is available as a kubectl plugin (`kubectl nvcre ...`).

## Command groups

| Command group | Purpose |
|--------------|---------|
| `nvcrectl setup` | Install and uninstall the controller and its dependencies |
| `nvcrectl certification` | Run, render, report, and list-categories for Certification resources |
| `nvcrectl workloadrun` | Run, render, report, status, and cancel WorkloadRun resources |
| `nvcrectl cluster` | Inspect GPU nodes, platform, and network topology |
| `nvcrectl workflow` | Render Workflow manifests offline with overrides applied |

## Global flags

These flags are available on subcommands that connect to a cluster:

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig (defaults to `~/.kube/config`) |
| `--context` | Kubernetes context to use |
| `--namespace` / `-n` | Namespace override (not available on all subcommands) |

## Shell completion

```bash
nvcrectl completion bash >> ~/.bashrc
nvcrectl completion zsh >> ~/.zshrc
```
