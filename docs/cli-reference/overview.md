---
title: ncrectl Overview
description: The ncrectl CLI manages the full lifecycle of cluster certification and WorkloadRun.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`ncrectl` is the command-line interface for the Cluster Readiness Engine. It handles installation, certification lifecycle, workload execution, and reporting — without requiring direct `kubectl` access for most operations.

## Install

```bash
curl -sSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/install.sh | bash
ncrectl version
```

## Command groups

| Command group | Purpose |
|--------------|---------|
| `ncrectl setup` | Install and uninstall controller dependencies |
| `ncrectl certification` | Run, render, report, and manage Certification resources |
| `ncrectl workloadrun` | Run and report WorkloadRun resources |

## Global flags

| Flag | Description |
|------|-------------|
| `--kubeconfig` | Path to kubeconfig (defaults to `~/.kube/config`) |
| `--namespace` | Target namespace (defaults to `cluster-readiness-engine`) |
| `--context` | Kubernetes context to use |
| `-v`, `--verbose` | Verbose output |

## Shell completion

```bash
ncrectl completion bash >> ~/.bashrc
ncrectl completion zsh >> ~/.zshrc
```
