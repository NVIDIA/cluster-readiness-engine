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

`ncrectl setup init` installs all controller dependencies (Kubeflow Trainer, CRDs, the controller deployment, and built-in LogProfiles) in a single step.

```bash
ncrectl setup init
```

The command will:

1. Install Kubeflow Trainer (required for `TrainJob` workloads)
2. Install the Cluster Readiness Engine CRDs
3. Deploy the controller
4. Install built-in LogProfiles for supported frameworks

### Image pull secret

If your cluster requires an NGC image pull secret to pull the certification workload images, pass it during setup:

```bash
ncrectl setup init --image-pull-secret ngc-secret
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

This removes the controller, CRDs, and all associated resources. It does not remove Kubeflow Trainer.
