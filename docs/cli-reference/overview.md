---
title: ncrectl Overview
description: The ncrectl CLI manages the full lifecycle of cluster certification and WorkloadRun.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`ncrectl` is the command-line interface for the Cluster Readiness Engine. It handles installation, certification lifecycle, workload execution, and reporting — without requiring direct `kubectl` access for most operations.

## Install

```bash
export NCRECTL_VERSION=v0.1.0-rc.8
curl -sSL "https://github.com/NVIDIA/cluster-readiness-engine/releases/download/${NCRECTL_VERSION}/installer" | bash
ncrectl version
```

The installer also creates a `kubectl-ncre` symlink so the CLI is available as a kubectl plugin (`kubectl ncre ...`).

<Warning>
Release builds enforce a version check on every invocation. If a newer version exists, the command exits with code 1 and prints an upgrade prompt. Use `ncrectl upgrade` to update. CI pipelines should pin to a specific version to avoid unexpected failures.
</Warning>

## Command groups

| Command group | Purpose |
|--------------|---------|
| `ncrectl setup` | Install and uninstall the controller and its dependencies |
| `ncrectl certification` | Run, render, report, and list-categories for Certification resources |
| `ncrectl workloadrun` | Run, render, report, status, and cancel WorkloadRun resources |
| `ncrectl cluster` | Inspect GPU nodes, platform, and network topology |
| `ncrectl workflow` | Render Workflow manifests offline |
| `ncrectl upgrade` | Upgrade ncrectl to the latest release |

## Global flags

These flags are available on all subcommands that connect to a cluster (provided by `k8s.io/cli-runtime`):

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig (defaults to `~/.kube/config`) |
| `--context` | Kubernetes context to use |
| `--cluster` | Kubernetes cluster to use |
| `--namespace` / `-n` | Namespace override (not available on all subcommands) |
| `--as` | Username to impersonate |
| `--token` | Bearer token for authentication |

## Shell completion

```bash
ncrectl completion bash >> ~/.bashrc
ncrectl completion zsh >> ~/.zshrc
```
