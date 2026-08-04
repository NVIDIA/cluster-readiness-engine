---
title: Install
description: Install the ncrectl CLI and set up the Cluster Readiness Engine controller on your Kubernetes cluster.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## Prerequisites

- Kubernetes 1.28+ cluster with GPU nodes
- `kubectl` configured and pointing at the target cluster
- NVIDIA GPU Operator installed
- Helm 3.x

## Install the CLI

```bash
curl -sSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/install.sh | bash
```

Verify the installation:

```bash
ncrectl version
```

## Set up the cluster

`ncrectl setup init` installs the controller and its dependencies in two phases:

1. **deps** — Kubeflow Trainer (required for `TrainJob` workloads)
2. **helm** — CRE Helm chart (CRDs, controller deployment, built-in LogProfiles)

```bash
ncrectl setup init
```

### GHCR authentication

The controller image and Helm chart are pulled from GHCR. Pass a GitHub token to authenticate — the CLI creates the pull secret for you:

```bash
ncrectl setup init --image-pull-secret $GITHUB_TOKEN
```

## Verify

Check that the controller is running:

```bash
kubectl get pods -n cluster-readiness-engine
```

Check that the CRDs are installed:

```bash
kubectl get crds | grep cre.nvidia.com
```

## Uninstall

```bash
ncrectl setup reset
```

This removes all CRE custom resources, the controller, CRDs, and Kubeflow Trainer. To keep Kubeflow Trainer, pass `--skip-phases=deps`:

```bash
ncrectl setup reset --skip-phases=deps
```
