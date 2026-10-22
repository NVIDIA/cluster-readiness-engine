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

After `helm upgrade --install --wait` returns, the `helm` phase checks that the controller stays up. Helm's wait passes as soon as it sees the controller pod Ready once, so a controller that is Ready for a few seconds and then OOMKilled during its initial cache sync passes it ([issue #504](https://github.com/NVIDIA/cluster-readiness-engine/issues/504)). `setup init` therefore reports success only when every controller pod of the `nvcre` release (labels `control-plane: manager` and `app.kubernetes.io/instance: nvcre`) has been Ready, with its `manager` container running and not restarted, for one minute, timed from the pod's last turn to Ready. A controller that has already been Ready that long, as on a re-run that changes nothing, passes at once. The phase fails as soon as the container exits or restarts during the check or sits in `CrashLoopBackOff`, and after three minutes if the pods have not settled. When Helm's own wait times out, the phase reads the controller pods once and prints the cause if one is crash-looping. Each read of the pods gives up after 10 seconds, so an unresponsive API server cannot stall either step. In both cases the error names the pod, its restart count and its most recent exit reason. For an OOMKill it also names the memory limit and the flag that raises it:

```text
Error: [helm] the NVCRE controller is crash-looping: pod nvcre/nvcre-manager-795cb6fd4c-hpwh4 restarted 1 time, last exit OOMKilled (exit code 137).
It ran out of memory at its 96Mi limit. The controller's memory grows with the cluster's size; raise the limit, for example:
  nvcrectl setup init --set manager.resources.limits.memory=192Mi
```

The suggested limit is twice the current one, a starting point rather than a sizing rule; see [Resource requirements](../operations/deployment.md#resource-requirements). A later `setup init` keeps the raised limit ([Values across runs](#values-across-runs)).

### Retry behavior and automatic recovery

Re-running `setup init` is idempotent for known release and ownership states. Unknown or incomplete evidence stops before mutation:

- **Already deployed**: before accepting the shortcut, setup reads `jobsets.jobset.x-k8s.io`. A read error stops the phase, and a missing CRD stops with repair guidance because a skipped or upgraded Helm release cannot restore a chart CRD. When the CRD exists and the `kubeflow-trainer` release is `deployed` at the pinned chart version, the `deps` phase prints "already deployed" and skips the full ownership inspection and upgrade. This shortcut does not certify controller health or schema compatibility. Not re-rendering the chart means not re-rolling its webhook certificates.
- **JobSet ownership**: a verified external controller makes Trainer install with `jobset.install=false`; bundled ownership remains bundled. A retained CRD with no controller, support resources, release evidence, or JobSets installs the bundled controller without replacing or refreshing the CRD. Ambiguous ownership and unreadable Helm state stop before Helm mutation and print the manual Trainer installation plus `nvcrectl setup init --skip-phases=deps` fallback. Detection covers the published chart fingerprint, not arbitrary customized controllers. A ClusterRole whose `jobset.x-k8s.io` rules each grant only a non-empty subset of `get`, `list`, and `watch` is a read-only consumer, not a fingerprint, so platform roles such as GKE's addon-managed `system:clustermetrics` do not block installation and are left untouched. Roles with write verbs on JobSets still count as evidence.
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
| `--image-pull-secret` | — | GitHub token for clusters that pull from a private GHCR mirror or fork: the CLI creates the `ghcr.io`-scoped Kubernetes pull secret and authenticates Helm chart pulls only when the chart ref is on GHCR. For a chart on a non-GHCR mirror, run `helm registry login <mirror>` before `setup init` instead. The secret is added at the end of `manager.imagePullSecrets`, after any list already set. The public image and chart need no token. |
| `--image` | — | Override the controller image, as `repo:tag` or `repo@sha256:<digest>` (default: `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`). A digest sets `manager.image.digest`, so the chart renders `repo@sha256:<digest>`. Cannot be combined with values that set `manager.image.repository`, `.tag` or `.digest` on the same run |
| `--values`, `-f` | — | Values file for the NVCRE release, as with `helm --values`. Repeatable, or comma-separated; files apply in the order given. `-f -` reads stdin and needs `--auto-approve`. URLs are not accepted: download the file and pass its path |
| `--set-json` | — | JSON value for the NVCRE release, as with `helm --set-json`. Repeatable; overrides `--values`. Needs Helm 3.10 or later |
| `--set` | — | Value for the NVCRE release, as with `helm --set`. Repeatable; overrides `--values` and `--set-json` |
| `--reset-values` | `false` | Do not keep the installed release's values, except `rbac.scope` and `rbac.runNamespaces`. See [Values across runs](#values-across-runs) |
| `--chart-ref` | `oci://ghcr.io/nvidia/cluster-readiness-engine` | NVCRE Helm chart location. Override to install from a mirror registry; used for both the release install and the CRD extraction (`helm show crds`). |
| `--trainer-chart-ref` | `oci://ghcr.io/kubeflow/charts/kubeflow-trainer` | Kubeflow Trainer Helm chart location. Override to install from a mirror registry. |
| `--skip-phases` | — | Comma-separated valid phases to skip (`deps`, `helm`) |
| `--version` | — | Helm chart version to install (required for dev builds) |
| `--auto-approve` | `false` | Skip the interactive confirmation prompt (for CI/automation) |
| `--rbac-scope` | — | Where the controller may write: `cluster` or `namespaced` (chart value `rbac.scope`, [ADR-092](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/092-per-namespace-controller-rbac.md)). Unset keeps the installed release's `rbac.scope`, or the chart default on a first install. Cannot be combined with values that set `rbac.scope` on the same run. See [RBAC requirements](../operations/deployment.md#rbac-requirements) |

The value flags (`--values`, `--set`, `--set-json`, `--reset-values`) apply to the NVCRE release only, never to Kubeflow Trainer. Giving any of them with `--skip-phases` including `helm` is an error, since the phase that would use them does not run.

`setup init` needs Helm 3.8 or later, the release where OCI chart support became generally available. The only flag it adds to the upgrade itself is one `-f`. An operator's own `--set-json` needs Helm 3.10 or later, as it would with Helm directly.

The chart versions are unaffected by the `-ref` flags: the NVCRE chart is still pulled at the CLI version (or `--version`), and Kubeflow Trainer at the pinned version, so a mirror must host those chart versions.

For a controller image on a private non-GHCR mirror, `--image-pull-secret` does not help: its token and the secret it creates are scoped to `ghcr.io`. Create the mirror's pull secret in the `nvcre` namespace yourself and bind it with `--set 'manager.imagePullSecrets[0].name=<secret-name>'`, or configure node-level registry credentials for the mirror (containerd/kubelet); see [Restricted egress and air-gapped installs](../operations/deployment.md#restricted-egress-and-air-gapped-installs). The binding is kept on later runs.

### Values across runs

`setup init` keeps the values set on the installed release ([ADR-098](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/098-setup-init-chart-values.md)). A value you set once, with `setup init` or with `helm upgrade`, survives every later `setup init`, including one with no flags. Each value is resolved in this order, lowest first:

> chart defaults → values set on the installed release → this run's `--values` files → this run's `--set-json` → this run's `--set` → values `setup init` sets itself

"Values set on the installed release" are what `helm get values nvcre --namespace nvcre` prints, not the chart defaults. The chain holds for each value separately: a value given as a parent object replaces only the keys it names. `--set-json 'rbac={"runNamespaces":["team-b"]}'` changes `rbac.runNamespaces` and keeps an installed `rbac.scope: namespaced`, where Helm on its own would drop it. A `null` removes a value, kept or default. A list replaces the kept list; an index write such as `--set 'manager.imagePullSecrets[0].name=x'` edits it.

Exceptions and rules:

- **The controller image is worked out on every run and never kept.** `manager.image.repository`, `.tag` and `.digest` are dropped from the kept values, so a digest pinned on an earlier run cannot hold the controller on an old image after an upgrade. Each run uses this run's values when they set any of those three keys (`setup init` then sets none, and the chart fills the rest: an unset tag is the chart's `appVersion`, so `--set manager.image.repository=<mirror>/manager` is enough for a mirror), else `--image`, else the CLI-version image. Other keys under `manager.image`, such as `pullPolicy`, are kept and do not choose the image.
- **A value with its own flag is set by the flag or by a value on one run, not both.** `--image` with values that set `manager.image.repository`, `.tag` or `.digest`, and `--rbac-scope` with a value for `rbac.scope`, are rejected before anything changes. A kept value never conflicts with a flag.
- **`--image-pull-secret` adds to the pull-secret list.** It appends `nvcrectl-pull-secret` to `manager.imagePullSecrets` as resolved from the kept values and this run's values, unless it is already listed. It never overwrites an existing entry.
- **`--reset-values` starts again from the chart defaults,** except `rbac.scope` and `rbac.runNamespaces`, which are still kept so a reset never widens a namespaced grant. Use it to restore a chart default that a kept value pins, or to drop a kept key a later chart no longer reads.

Before the confirmation prompt, and with `--auto-approve` too, preflight reads the installed release's values, checks this run's against them and prints what the release will be given. Values are printed by path and source only, never with their contents, because the output reaches CI logs and a value can hold a credential. The controller image, the pull-secret names and `rbac.scope` are printed with their values:

```text
[preflight] Values for release "nvcre" (paths only; values are not printed):
  manager.resources.limits.memory    (installed release)
  manager.resources.requests.memory  (this run)
  rbac.runNamespaces                 (installed release)
[preflight] rbac.scope: namespaced (installed release)
[preflight] Controller image: ghcr.io/nvidia/cluster-readiness-engine/manager:v0.8.0 (CLI version)
[preflight] Image pull secrets: [mirror-pull-secret, nvcrectl-pull-secret] (installed release, --image-pull-secret)
```

To see the values themselves, read your own inputs or run `helm get values nvcre --namespace nvcre`. When `--skip-phases` includes `helm`, the release is not read and nothing is printed.

`setup init` runs Helm up to three times for the values: two local `helm template` runs that read the values, then the upgrade. It reads each `--values` source once before any of them, so stdin, a named pipe or process substitution reach every run with the same content.

### Example

```bash
# Standard install
nvcrectl setup init

# For a private GHCR mirror or fork (the public image and chart need no token;
# a chart on a non-GHCR mirror needs `helm registry login <mirror>` instead)
nvcrectl setup init --image-pull-secret $GITHUB_TOKEN

# Skip Kubeflow Trainer (already installed)
nvcrectl setup init --skip-phases=deps

# Size the controller for a large cluster; later runs keep it
nvcrectl setup init \
  --set manager.resources.requests.memory=4Gi \
  --set manager.resources.limits.memory=8Gi

# Values from a file, or from stdin in automation
nvcrectl setup init -f nvcre-values.yaml
generate-values | nvcrectl setup init --auto-approve -f -

# Drop the kept values (rbac.scope and rbac.runNamespaces stay)
nvcrectl setup init --reset-values

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
