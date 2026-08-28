---
title: Install
description: Install the nvcrectl CLI and set up the Cluster Readiness Engine controller on your Kubernetes cluster.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Prerequisites

- Kubernetes 1.28+ cluster with GPU nodes
- `kubectl` configured and pointing at the target cluster
- NVIDIA GPU Operator installed
- Helm 3.x

## Install the CLI

While this repository is internal, GitHub serves release assets only through
authenticated API downloads — plain `curl` against `releases/download/...` returns a
404 "Not Found" page instead of the script, even with a token. Fetch the installer
with the gh CLI (authenticate with `gh auth login` first). Set the version you want
to install, then run the installer:

```bash
export NVCRECTL_VERSION=v0.1.0-rc.9
gh release download "${NVCRECTL_VERSION}" --repo NVIDIA/cluster-readiness-engine \
  --pattern installer --output - | bash -s -- -v "${NVCRECTL_VERSION}"
```

The installer automatically downloads and verifies a SHA-256 checksum before installing. On air-gapped systems, ensure `checksums.txt` from the same release is reachable alongside the binary.

Verify the installation:

```bash
nvcrectl --version
```

## Set up the cluster

`nvcrectl setup init` installs the controller and its dependencies in two phases:

1. **deps** — Kubeflow Trainer (required for `TrainJob` workloads)
2. **helm** — CRE Helm chart (CRDs, controller deployment, built-in LogProfiles)

```bash
nvcrectl setup init
```

### GHCR authentication

The controller image and Helm chart are pulled from GHCR. Pass a GitHub token to authenticate — the CLI creates the pull secret for you:

```bash
nvcrectl setup init --image-pull-secret $GITHUB_TOKEN
```

## Verify

Check that the controller is running:

```bash
kubectl get pods -n cluster-readiness-engine
```

Check that the CRDs are installed:

```bash
kubectl get crds | grep nvcre.nvidia.com
```

## Uninstall

```bash
nvcrectl setup reset
```

This removes all CRE custom resources, the controller, CRDs, and Kubeflow Trainer. To keep Kubeflow Trainer, pass `--skip-phases=deps`:

```bash
nvcrectl setup reset --skip-phases=deps
```
