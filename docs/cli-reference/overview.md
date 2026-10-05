---
title: nvcrectl Overview
description: The nvcrectl CLI manages the full lifecycle of cluster certification and WorkloadRun.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`nvcrectl` is the command-line interface for the NVIDIA Cluster Readiness Engine. It handles installation, certification lifecycle, workload execution, and reporting — without requiring direct `kubectl` access for most operations.

## Install

```bash
curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash
nvcrectl --version
```

To pin a version: `curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/download/<tag>/installer | bash -s -- -v <tag>`.

The installer also creates a `kubectl-nvcre` symlink so the CLI is available as a kubectl plugin (`kubectl nvcre ...`).

## Command groups

| Command group | Purpose |
|--------------|---------|
| `nvcrectl setup` | Install and uninstall the controller and its dependencies |
| `nvcrectl certification` | Run, render, report, and list-categories for Certification resources |
| `nvcrectl workloadrun` | Run, render, report, status, and cancel WorkloadRun resources |
| `nvcrectl cluster` | Inspect GPU nodes, platform, and network topology |
| `nvcrectl workflow` | Render Workflow manifests offline with overrides applied |
| `nvcrectl mcp` | Serve read-only certification state to MCP agents over stdio |

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
