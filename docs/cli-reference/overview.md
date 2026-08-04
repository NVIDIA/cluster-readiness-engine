---
title: xcalctl Overview
description: The xcalctl CLI manages the full lifecycle of cluster certification and WorkloadRun.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`xcalctl` is the command-line interface for the Cluster Readiness Engine. It handles installation, certification lifecycle, workload execution, and reporting — without requiring direct `kubectl` access for most operations.

## Install

```bash
curl -sSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/install.sh | bash
xcalctl version
```

## Command groups

| Command group | Purpose |
|--------------|---------|
| `xcalctl setup` | Install and uninstall controller dependencies |
| `xcalctl certification` | Run, render, report, and manage Certification resources |
| `xcalctl workloadrun` | Run and report WorkloadRun resources |

## Global flags

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig (defaults to `~/.kube/config`) |
| `--namespace` | Target namespace (defaults to `cluster-readiness-engine`) |
| `--context` | Kubernetes context to use |
| `-v`, `--verbose` | Verbose output |

## Shell completion

```bash
xcalctl completion bash >> ~/.bashrc
xcalctl completion zsh >> ~/.zshrc
```
