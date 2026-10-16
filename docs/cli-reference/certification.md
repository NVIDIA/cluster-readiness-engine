---
title: nvcrectl certification
description: Manage the full lifecycle of Certification resources.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl certification run

Applies a Certification manifest (or runs categories by flag), waits for completion, prints a report, and optionally cleans up.

```bash
nvcrectl certification run --cert-file <file> [flags]
nvcrectl certification run --category communication/nccl-all-reduce [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--cert-file` | — | Path to a Certification YAML (mutually exclusive with `--category`) |
| `--category` | — | Category in `domain/variant` format; repeatable (mutually exclusive with `--cert-file`) |
| `--name` | auto | Certification name (default: `nvcrectl-<timestamp>`) |
| `--setup` | `false` | Install CRDs, controller, and LogProfiles before creating the certification |
| `--image` | — | Controller image for `--setup` (default: `ghcr.io/nvidia/cluster-readiness-engine/manager:<version>`) |
| `--wait` | `false` | Block until the certification completes and print a report |
| `--timeout` | derived | Timeout for `--wait`. Ignored without `--wait`. When `--wait` is set, must be at least `1s`. When not set, derived from the selected categories' `timeoutPerJob` budgets (max across categories × iterations × 1.5), floored at `30m`; the CLI prints the derived value when the watch starts. The catalog's `timeoutPerJob` defaults are 1h for the communication entries and 24h for the two training entries, so a training category derives a 36h wait on its own. Pass `--timeout` (or set `timeoutPerJob` on the category) in pipelines that need a tighter bound. An explicit value always wins. On timeout, the CLI prints a partial report and leaves the Certification running unless `--cleanup` is set. |
| `--cleanup` | `false` | Delete the certification, namespace, and installed components after completion |
| `--grant-run-access` | `false` | With the chart's `rbac.scope: namespaced`, create the RoleBinding that grants the controller write access in a namespace that already existed. Namespaces this run creates are granted without it. See [Controller access to the run namespace](#controller-access-to-the-run-namespace) |
| `--nodes-per-job` | `0` | Nodes per job (0 = auto-select) |
| `--gpus-per-node` | `0` | GPUs per node (0 = auto-detect from GPU architecture) |
| `--enable-checkpoint` | `false` | Enable checkpoint storage for training workloads |
| `--enable-mnnvl` | `false` | Enable Multi-Node NVLink (`NCCL_MNNVL_ENABLE=1`) |
| `--max-steps` | `0` | Max training steps for NeMo 4 workloads (0 = catalog default) |
| `--exit-duration-mins` | `0` | Training duration in minutes for NeMo 6 workloads (0 = catalog default) |
| `--startup-stall-timeout-seconds` | `0` | Startup-stall window in seconds for training workloads (0 = catalog default; negative values are invalid). Incompatible with `--cert-file`, including explicit 0; set a positive `spec.startupStallTimeoutSeconds` or `spec.categories[].options.startupStallTimeoutSeconds` in the Certification YAML instead. |
| `--repeat-count` | `0` | Orchestration iterations to repeat tests (0 = catalog default) |
| `--max-restarts` | `0` | Maximum checkpoint restarts for training workloads (0 = catalog default) |
| `--storage-class` | — | StorageClass for PVC dependencies created by catalog entries |
| `--results-file` | — | Write the certification report as JSON to this path (requires `--wait`) |
| `--controller-pull-secret` | — | Token for controller registry auth during `--setup` (e.g. GitHub PAT for `ghcr.io`) — separate from workload image credentials |
| `--workload-registry` | — | Registry server for workload image pull (e.g. `nvcr.io`, `ghcr.io`) — required when `--workload-registry-password` is set |
| `--workload-registry-username` | — | Registry username for workload image pull (e.g. `$oauthtoken` for NGC) — required when `--workload-registry-password` is set |
| `--workload-registry-password` | — | Registry password or API key — creates an `nvcrectl-pull-<name>` imagePullSecret in the namespace, deleted automatically when the Certification is deleted |

### Controller access to the run namespace

With the chart's `rbac.scope: namespaced`, the controller can write only in namespaces that have a RoleBinding granting it `nvcre-run-role` ([ADR-092](../designs/092-per-namespace-controller-rbac.md)). Before creating the Certification, `run` makes sure the run namespace has one:

- A namespace this run created (the generated `nvcrectl-<timestamp>`, or a `-n` namespace that did not exist yet) gets the binding automatically. Deleting the namespace removes it.
- A namespace that already existed gets it only with `--grant-run-access`. Without the flag, `run` stops before submitting anything and prints the binding, so a shared namespace never becomes a standing grant by accident. A binding `--grant-run-access` created is deleted by `--cleanup` once the Certification is fully gone and no other Certification, WorkloadRun or Workflow is left in the namespace or recently adopted the binding, because the controller's finalizers need it until then; a binding kept for another run stays for you to delete once no run needs it. Without `--cleanup` it stays, and `run` says so.
- A binding that already exists, from `rbac.runNamespaces`, another run, or applied by hand, is used and never deleted.
- The binding is created only after the pull secret, right before the Certification, so a run that fails earlier leaves no binding behind.
- If you cannot read the cluster-scoped `nvcre-run-rolebinding` and `nvcre-run-role`, `run` cannot tell which scope is installed. It submits with a warning instead of stopping, and does not create a binding whose subject it cannot read from `nvcre-manager-rolebinding`.
- Adopting a binding another nvcrectl run created marks it as in use, so that run's cleanup does not delete it before this one is submitted. If you are not allowed to mark it, which is the case with the default `edit` role, `run` warns and submits anyway; in the rare case the other run's cleanup deletes the binding first, your run waits for access and then fails, naming the binding. A binding from `rbac.runNamespaces` or applied by hand is never deleted by a run's cleanup.

Under `rbac.scope: cluster`, the default, none of this applies and nothing is printed. Creating the binding uses your own credentials, which need every permission in `nvcre-run-role` within the namespace (a namespace admin has them), or `bind` on `clusterroles/nvcre-run-role`. See [RBAC requirements](../operations/deployment.md#rbac-requirements).

When `--wait` reaches its timeout, the command prints the Certification's current report, writes it to `--results-file` when requested, and exits with a timeout error. Without `--cleanup`, the Certification continues running in the cluster; the timeout stops only the CLI watch. The timeout output includes commands to watch its progress, print an updated report, or stop it. `nvcrectl certification report` exits nonzero while the Certification is still running. With `--cleanup`, the partial report is produced before the Certification and its child resources are deleted.

### Examples

```bash
nvcrectl certification run \
  --cert-file certification.yaml \
  --wait
```

Pull workload images from NGC:

```bash
nvcrectl certification run \
  --category communication/nccl-all-reduce \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  --wait
```

## nvcrectl certification render

Renders the Workflow manifests that would be created for a Certification, without applying them. Useful for inspecting override application and resource requests before running.

```bash
nvcrectl certification render [flags] <cert-file>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Override platform detection (`aws`, `gcp`, `azure`, `oci`, `onprem`, `togetherai`, `mistral`, `forge`, `nscale`) |
| `--gpu-arch` | none | GPU architecture for offline render, e.g. `gb300` or the product name `NVIDIA-GB300`; an unknown architecture is rejected with the valid list. Use it for a DRA-only GPU stack whose nodes carry no `nvidia.com/gpu.product` label. Wins over the target `nodeSelector`'s label when set; cannot be combined with `--dry-run`, which detects the architecture from real nodes. |
| `--dry-run` | `false` | Validate against the live API server without creating resources. Also discovers the target nodes, so NIC resource detection (on-prem GB200/GB300 and HGX B200/B300), GKE TCPXO network detection and TCPXO plugin version detection (GCP H100) run as they do in the controller and report what they found on stderr. Plugin version detection lists `kube-system` pods, so it needs that permission. |
| `--output` | `yaml` | Output format: `yaml` or `json` |

A Certification that sets `spec.gangScheduler` has that applied to the rendered output too: the scheduler name and the queue label (`gangScheduler.queueLabelKey`, `kai.scheduler/queue` when unset; `runai/queue` on a Run:ai cluster) appear in the rendered manifests on the submitted workload object, on the Job template, and on the pod template metadata, so what you inspect matches what the controller creates. There is no flag for it; the field is set in the Certification YAML.

## nvcrectl certification report

Fetches a completed Certification from the cluster and generates a pass/fail report. Multiple names can be provided to combine them into a single report.

```bash
nvcrectl certification report <name> [<name>...] [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--results-file` | — | Write the report as JSON to this file path |

Bandwidth from a measurement that is not final (its `Complete` condition reason is anything other than `JobSucceeded`, for example `LogsUnavailable` after its Job was deleted) is shown as `(provisional: <reason>)` with a `?` mark instead of `✓`, carries `provisional: true` and `provisionalReason` in the JSON, and is never shown as passing; clique and category bandwidth show the lowest value across their groups.

## nvcrectl certification list-categories

Lists all available catalog categories that can be used in a Certification.

```bash
nvcrectl certification list-categories [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` | `table` | Output format: `table`, `yaml`, or `json` |
