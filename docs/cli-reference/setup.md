---
title: nvcrectl setup
description: Install and uninstall the NVIDIA Cluster Readiness Engine controller and its dependencies.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl setup init

Installs NVCRE via Helm and its dependencies on the target cluster.

```bash
nvcrectl setup init [flags]
```

### What it installs

Runs two phases in order:

| Phase | What |
|-------|------|
| `deps` | Kubeflow Trainer v2.2.1 |
| `helm` | NVCRE Helm chart (CRDs, controller, built-in LogProfiles) pulled from GHCR |

Use `--skip-phases=deps` to skip Kubeflow Trainer if it is already installed.

The `helm` phase reconciles the NVCRE CRDs on every run: it extracts the CRD manifests from the same chart version it is about to install (`helm show crds`) and server-side-applies them (field manager `nvcrectl-setup`, force ownership) before running `helm upgrade --install`. Helm itself applies a chart's `crds/` directory only on the *first* install, so without this step an upgrade would leave the installed CRDs at the old schema. Server-side apply is idempotent, so a fresh install and an unchanged re-run behave exactly as before.

### Retry behavior and automatic recovery

`setup init` converges from any partial state, so re-running it after a failure is always safe to try:

- **Already deployed**: when the `kubeflow-trainer` release is `deployed` at the pinned chart version, the `deps` phase prints "already deployed" and skips the upgrade entirely. Not re-rendering the chart means not re-rolling its webhook certificates.
- **Failed or pending release**: the install is attempted once. If it fails with the webhook Secret field-ownership conflict signature (`Apply failed with ... conflicts` on `.data` fields of Secrets in `kubeflow-system`) *and* the release state agrees (`failed` or `pending-*`), `setup init` performs an automatic recovery: uninstall the release, delete its four CRDs (`trainjobs`, `trainingruntimes`, `clustertrainingruntimes` in `trainer.kubeflow.org`; `jobsets` in `jobset.x-k8s.io`), delete the `kubeflow-system` namespace, and reinstall the pinned chart. Exactly one recovery attempt is made per run.
- **Safety gate**: automatic recovery is refused when any `TrainJob` or `JobSet` instance exists, or when a `TrainingRuntime`/`ClusterTrainingRuntime` exists that is not Helm-owned (missing the `app.kubernetes.io/managed-by: Helm` label) — deleting the CRDs would destroy them. In that case, and for any ambiguous failure, `setup init` fails fast and prints the manual procedure:

  ```bash
  helm uninstall kubeflow-trainer --namespace kubeflow-system
  kubectl delete crd trainjobs.trainer.kubeflow.org trainingruntimes.trainer.kubeflow.org \
    clustertrainingruntimes.trainer.kubeflow.org jobsets.jobset.x-k8s.io
  kubectl delete namespace kubeflow-system
  nvcrectl setup init   # reinstalls the pinned Kubeflow Trainer
  ```

- **Confirmation**: in interactive mode the recovery plan is printed and re-confirmed before anything is deleted; `--auto-approve` covers CI.

<Warning>
Automatic recovery deletes the `kubeflow-system` namespace, including anything you placed there manually. The namespace is created and managed by `setup init`.
</Warning>

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image-pull-secret` | — | GitHub token — the CLI creates a `ghcr.io` pull secret and uses it to authenticate the Helm chart pull |
| `--image` | — | Override the controller image (default: `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`) |
| `--skip-phases` | — | Comma-separated phases to skip (e.g., `deps`) |
| `--version` | — | Helm chart version to install (required for dev builds) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt (for CI/automation) |

### Example

```bash
# Standard install
nvcrectl setup init

# With GHCR authentication
nvcrectl setup init --image-pull-secret $GITHUB_TOKEN

# Skip Kubeflow Trainer (already installed)
nvcrectl setup init --skip-phases=deps
```

## nvcrectl setup status

Reports the installation status of NVCRE and its dependencies by querying the cluster.

```bash
nvcrectl setup status [flags]
```

Components checked: `nvcreCRDs`, `nvcreController`, `kubeflowTrainer`, `logProfiles`, `gpuOperator`, `dcgm` (optional).

The Helm releases managed by `setup init` (`nvcre` and `kubeflow-trainer`) are also checked via the helm CLI and reported under `helmReleases`. A release in a failed or pending state (e.g. `failed`, `pending-upgrade`) makes the status not ready. A release helm has no record of, or that cannot be queried (helm not in PATH), is reported but does not affect readiness.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` / `-o` | `table` | Output format: `table`, `json` |

### Example

```bash
nvcrectl setup status
nvcrectl setup status -o json
```

## nvcrectl setup reset

Removes NVCRE and its dependencies from the target cluster. Kubeflow Trainer is removed by default.

```bash
nvcrectl setup reset [flags]
```

### What it removes

Runs three phases in order:

| Phase | What |
|-------|------|
| `cr` | All NVCRE custom resource instances (Certifications, Workflows, Jobs) |
| `helm` | NVCRE Helm release (CRDs, controller, LogProfiles) |
| `deps` | Kubeflow Trainer |

Use `--skip-phases=deps` to keep Kubeflow Trainer.

After all phases complete, `setup reset` prints a **Retained resources** block listing any namespaces or secrets that were not deleted (Helm never removes the release namespace, and the `nvcrectl-pull-secret` created by `setup init` is outside the Helm release). Each entry includes the `kubectl delete` command to remove it manually if you want a pristine cluster.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--skip-phases` | — | Comma-separated phases to skip (e.g., `deps`) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt |

### Example

```bash
# Full uninstall (including Kubeflow Trainer)
nvcrectl setup reset

# Keep Kubeflow Trainer
nvcrectl setup reset --skip-phases=deps
```

<Warning>
`reset` deletes all Certification, Workflow, and Job resources. This is irreversible.
</Warning>
