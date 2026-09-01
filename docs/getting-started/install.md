---
title: Install
description: Install the nvcrectl CLI and set up the NVIDIA Cluster Readiness Engine controller on your Kubernetes cluster.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Prerequisites

- Kubernetes 1.29+ cluster with GPU nodes (the Certification CRD's CEL rules use the `quantity()` library, which the API server accepts in new CRD expressions from 1.29)
- `kubectl` configured and pointing at the target cluster
- NVIDIA GPU Operator installed
- Helm 3.x

## Install the CLI

Download and run the installer. It detects your OS and architecture and resolves
the newest stable release itself:

```bash
curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash
```

To pin a version, download the installer from that release and pass the tag:

```bash
curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/download/<tag>/installer | bash -s -- -v <tag>
```

The installer automatically downloads and verifies a SHA-256 checksum before installing. On air-gapped systems, ensure `checksums.txt` from the same release is reachable alongside the binary.

Verify the installation:

```bash
nvcrectl --version
```

## Set up the cluster

`nvcrectl setup init` installs the controller and its dependencies in two phases:

1. **deps** — Kubeflow Trainer (required for `TrainJob` workloads)
2. **helm** — NVCRE Helm chart (CRDs, controller deployment, built-in LogProfiles)

```bash
nvcrectl setup init
```

### GHCR authentication

The controller image and Helm chart are pulled anonymously from GHCR:

```bash
nvcrectl setup init
```

If your cluster pulls from a private mirror or fork instead, pass `--image-pull-secret <github-token>` and the CLI creates the pull secret for you.

## Verify

Check that the controller is running:

```bash
kubectl get pods -n nvcre
```

Check that the CRDs are installed:

```bash
kubectl get crds | grep nvcre.nvidia.com
```

## Uninstall

```bash
nvcrectl setup reset
```

This removes all NVCRE custom resources, the controller, CRDs, and Kubeflow Trainer. To keep Kubeflow Trainer, pass `--skip-phases=deps`:

```bash
nvcrectl setup reset --skip-phases=deps
```
