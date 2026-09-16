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

Use `--skip-phases=deps` to skip Kubeflow Trainer if it is already installed, or `--skip-phases=helm` to run dependency setup only. Unknown phase names are rejected.

The `helm` phase reconciles the NVCRE CRDs on every run: it extracts the CRD manifests from the same chart version it is about to install (`helm show crds`) and server-side-applies them (field manager `nvcrectl-setup`, force ownership) before running `helm upgrade --install`. Helm itself applies a chart's `crds/` directory only on the *first* install, so without this step an upgrade would leave the installed CRDs at the old schema. Server-side apply is idempotent, so a fresh install and an unchanged re-run behave exactly as before.

### Retry behavior and automatic recovery

Re-running `setup init` is idempotent for known release and ownership states. Unknown or incomplete evidence stops before mutation:

- **Already deployed**: before accepting the shortcut, setup reads `jobsets.jobset.x-k8s.io`. A read error stops the phase, and a missing CRD stops with repair guidance because a skipped or upgraded Helm release cannot restore a chart CRD. When the CRD exists and the `kubeflow-trainer` release is `deployed` at the pinned chart version, the `deps` phase prints "already deployed" and skips the full ownership inspection and upgrade. This shortcut does not certify controller health or schema compatibility. Not re-rendering the chart means not re-rolling its webhook certificates.
- **JobSet ownership**: a verified external controller makes Trainer install with `jobset.install=false`; bundled ownership remains bundled. A retained CRD with no controller, support resources, release evidence, or JobSets installs the bundled controller without replacing or refreshing the CRD. Ambiguous ownership and unreadable Helm state stop before Helm mutation and print the manual Trainer installation plus `nvcrectl setup init --skip-phases=deps` fallback. Detection covers the published chart fingerprint, not arbitrary customized controllers.
- **External verification requires a JobSet controller, not a JobSet consumer**: holding `jobset.x-k8s.io` RBAC and admission webhooks on `jobsets` does not qualify a release as the external controller. Other components reconcile JobSets with exactly those rules — Kueue is one — and disabling the bundled subchart for them would leave the cluster with no JobSet controller at all. Two installation patterns are recognized, and each is verified on its own terms:
  - *Helm-owned external controller.* The release must additionally ship an admission configuration and a webhook-backing Deployment labelled with the JobSet chart's own identity, `helm.sh/chart: jobset-<version>`. Helm derives that label from chart metadata alone, so the release name, `nameOverride`, and `fullnameOverride` do not affect it. `app.kubernetes.io/name` is not accepted in its place, because charts derive it from `nameOverride` and a consumer can therefore set it to `jobset`. A Helm-owned release whose JobSet resources lack the chart label is reported as ambiguous.
  - *Non-Helm controller.* An install that left no Helm release record carries no chart label at all, so it is verified by resource identity instead. The upstream kustomize build — `kubectl apply -f .../jobset/releases/download/<tag>/manifests.yaml`, which renders `jobset-manager-role` and `jobset-controller-manager` — and the chart's `jobset-controller` identities are both accepted, together with the two webhook configuration names both channels hardcode. Every JobSet webhook must resolve to the same verified `jobset-webhook-service`, and that Service must select the controller Deployment.

  Anything outside those two patterns is reported as ambiguous and requires operator-managed installation. This includes `helm template | kubectl apply`, which labels resources `app.kubernetes.io/managed-by: Helm` while leaving no `meta.helm.sh/*` release annotations: that is partial ownership evidence rather than either pattern, and mixed evidence never selects a subchart value.
- **Failed or pending release**: the install is attempted once. If it fails with the webhook Secret field-ownership conflict signature (`Apply failed with ... conflicts` on `.data` fields of Secrets in `kubeflow-system`) *and* the release state agrees (`failed` or `pending-*`), `setup init` can recover automatically after all gates pass. Recovery uninstalls the release, deletes only the three Trainer CRDs, retains `jobsets.jobset.x-k8s.io`, deletes the inspected `kubeflow-system` namespace UID, and reinstalls using the same JobSet mode. Exactly one recovery attempt is made per run.
- **Safety gate**: recovery performs complete discovery and protected-resource inventory before uninstall, repeats ownership and evidence checks after confirmation and before uninstall, rechecks Trainer workloads before each Trainer CRD deletion, and repeats the protected namespace inventory immediately before namespace deletion. External JobSets outside `kubeflow-system` do not block external-mode recovery because their CRD and controller are retained. Required discovery or list permissions are mandatory; a restricted kubeconfig that cannot complete the inventory stops recovery. Any failure stops further cleanup, but completed steps are not rolled back. Operator-managed recovery must apply the same inventory and quiescence checks:

  ```bash
  helm uninstall kubeflow-trainer --namespace kubeflow-system
  kubectl delete crd trainjobs.trainer.kubeflow.org trainingruntimes.trainer.kubeflow.org \
    clustertrainingruntimes.trainer.kubeflow.org  # retain jobsets.jobset.x-k8s.io
  # Inventory kubeflow-system and delete it only after the same safety checks.
  nvcrectl setup init   # reinstalls the pinned Kubeflow Trainer
  ```

- **Confirmation**: in interactive mode the recovery plan is printed and re-confirmed before anything is deleted; `--auto-approve` covers CI.

<Warning>
Automatic recovery deletes `kubeflow-system` only after complete protected-resource inventory and repeated evidence checks. Keep protected-resource and Trainer custom-resource writes quiescent throughout recovery; `--auto-approve` does not establish quiescence.

Events, core Endpoints, and EndpointSlices are deliberately excluded from the protected inventory and can be lost with the namespace. A concurrent write after the last check and before deletion can also be lost; setup does not install a write barrier.
</Warning>

NVCRE does not refresh an existing JobSet CRD schema. When Trainer or JobSet is operator-managed, the operator is responsible for keeping the Trainer and JobSet CRD schemas, controllers, and external consumers mutually compatible, including any required migrations. Automatic reconciliation of the shared JobSet CRD is deferred work that needs its own design, covering schema compatibility, stored-version migration, and coordination with external controllers and workloads; reset and re-init are not a schema-upgrade procedure.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image-pull-secret` | — | GitHub token for clusters that pull from a private GHCR mirror or fork: the CLI creates the `ghcr.io`-scoped Kubernetes pull secret and authenticates Helm chart pulls only when the chart ref is on GHCR. For a chart on a non-GHCR mirror, run `helm registry login <mirror>` before `setup init` instead. The public image and chart need no token. |
| `--image` | — | Override the controller image (default: `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`) |
| `--chart-ref` | `oci://ghcr.io/nvidia/cluster-readiness-engine` | NVCRE Helm chart location. Override to install from a mirror registry; used for both the release install and the CRD extraction (`helm show crds`). |
| `--trainer-chart-ref` | `oci://ghcr.io/kubeflow/charts/kubeflow-trainer` | Kubeflow Trainer Helm chart location. Override to install from a mirror registry. |
| `--skip-phases` | — | Comma-separated valid phases to skip (`deps`, `helm`) |
| `--version` | — | Helm chart version to install (required for dev builds) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt (for CI/automation) |

The chart versions are unaffected by the `-ref` flags: the NVCRE chart is still pulled at the CLI version (or `--version`), and Kubeflow Trainer at the pinned version, so a mirror must host those chart versions.

For a controller image on a private non-GHCR mirror, `--image-pull-secret` does not help: its token and the secret it creates are scoped to `ghcr.io`, and `setup init` has no flag to bind a custom-named pull secret to the controller pod. Either configure node-level registry credentials for the mirror (containerd/kubelet), or install the chart directly with Helm and bind the secret via `--set 'manager.imagePullSecrets[0].name=<secret-name>'`; see [Restricted egress and air-gapped installs](../operations/deployment.md#restricted-egress-and-air-gapped-installs).

### Example

```bash
# Standard install
nvcrectl setup init

# For a private GHCR mirror or fork (the public image and chart need no token;
# a chart on a non-GHCR mirror needs `helm registry login <mirror>` instead)
nvcrectl setup init --image-pull-secret $GITHUB_TOKEN

# Skip Kubeflow Trainer (already installed)
nvcrectl setup init --skip-phases=deps

# Restricted egress: pull both charts and the controller image from a mirror
nvcrectl setup init \
  --chart-ref oci://registry.example.com/mirror/cluster-readiness-engine \
  --trainer-chart-ref oci://registry.example.com/mirror/kubeflow-trainer \
  --image registry.example.com/mirror/manager:<version>
```

## nvcrectl setup status

Reports the installation status of NVCRE and its dependencies by querying the cluster.

```bash
nvcrectl setup status [flags]
```

Components checked: `nvcreCRDs`, `nvcreController`, `kubeflowTrainer`, `logProfiles`, `gpuOperator`, `dcgm` (optional).

The `kubeflowTrainer` check verifies the installed Kubeflow Trainer version, not just that the TrainJob CRD exists. The version is detected from the managed Helm release chart version, the Trainer controller Deployment image tag, or the `app.kubernetes.io/version` label on the TrainJob CRD — whichever answers first — and reported under `kubeflowTrainerVersion` with its detection source. A version other than the one this NVCRE build supports fails the check with a message naming the detected and supported versions; a Trainer install whose version cannot be determined passes with a warning.

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

After all phases complete, `setup reset` prints a **Retained resources** block. Namespaces and the controller pull Secret include manual cleanup commands. The shared JobSet CRD is retained in every ownership mode and is warning-only: deleting it destroys JobSets across all namespaces, so the CLI does not print a deletion command. A Trainer Helm uninstall failure returns nonzero and stops before Trainer CRD cleanup.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--skip-phases` | — | Comma-separated valid phases to skip (`cr`, `helm`, `deps`) |
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
