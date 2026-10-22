---
title: nvcrectl workloadrun
description: Run and report WorkloadRun resources.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl workloadrun run

Applies a WorkloadRun manifest to the cluster and optionally waits for completion.

```bash
nvcrectl workloadrun run [flags] <file>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--wait` | `false` | Block until the workload completes |
| `--timeout` | `30m` | Timeout for `--wait`. Ignored without `--wait`. When `--wait` is set, must be at least `1s`. The watch checks status once immediately and then every 5s, so a short timeout still observes the WorkloadRun at least once. On timeout, the CLI prints a partial report and leaves the WorkloadRun running in the cluster unless `--cleanup` is set |
| `--setup` | `false` | Install CRDs, controller, and LogProfiles before creating the WorkloadRun |
| `--cleanup` | `false` | Delete the WorkloadRun, the namespace (when created by this run), and installed components after completion |
| `--grant-run-access` | `false` | With the chart's `rbac.scope: namespaced`, create the RoleBinding that grants the controller write access in a namespace that already existed, such as `default`. Namespaces this run creates are granted without it |
| `--image` | — | Override controller image |
| `--controller-pull-secret` | — | Token for controller registry auth during `--setup` (e.g. GitHub PAT for `ghcr.io`) — separate from workload image credentials |
| `--workload-registry` | — | Registry server for workload image pull (e.g. `nvcr.io`, `ghcr.io`) — required when `--workload-registry-password` is set |
| `--workload-registry-username` | — | Registry username for workload image pull (e.g. `$oauthtoken` for NGC) — required when `--workload-registry-password` is set |
| `--workload-registry-password` | — | Registry password or API key — creates an `nvcrectl-pull-<name>` imagePullSecret in the namespace, deleted automatically when the WorkloadRun is deleted |
| `--name` | — | Override the WorkloadRun name |
| `--node-list` | — | Comma-separated list of nodes to target |
| `--topology-domain` | — | Topology domain to target |
| `--topology-key` | — | Node label key for topology grouping |
| `--test-scale` | — | Override testScale (`intra-node`, `intra-rack`, `full-scale`) |
| `--results-file` | — | Write results as JSON to this file path (requires `--wait`) |

`--cleanup` runs on every exit path — success, failure, or `--wait` timeout — matching `certification run --cleanup`. It deletes the WorkloadRun only when this invocation created it (Kubernetes garbage collection then removes the owned Workflow, whose finalizer deletes the Jobs, workloads, and dependency resources), deletes the namespace only when this run created it, and, when combined with `--setup`, uninstalls the installed components after the cascade completes. On a `--wait` timeout the CLI prints a partial report from the still-live WorkloadRun before any cleanup runs. Without `--cleanup`, a `--wait` timeout stops only the CLI watch and the WorkloadRun continues running.

With the chart's `rbac.scope: namespaced`, the controller can write only in namespaces that have a RoleBinding granting it `nvcre-run-role` ([ADR-092](../designs/092-per-namespace-controller-rbac.md)). Without `-n` or `metadata.namespace`, `run` uses `default`, which already exists, so it stops before submitting anything and prints the binding rather than turning `default` into a standing grant. When `nvcrectl` cannot determine whether a binding is needed from cluster-scoped RBAC, it submits with a warning; a missing grant surfaces as the Workflow's access wait. Pass a namespace that does not exist yet (`run` creates it and the binding), or `--grant-run-access` to bind the controller in the existing one. A binding `--grant-run-access` created is deleted by `--cleanup` once the WorkloadRun and its Workflow are fully gone and no other run is left in the namespace or recently adopted the binding; a retained binding is left for the user to delete once no run needs it. Without `--cleanup` it stays. It is created only after node discovery, right before the WorkloadRun, so a run that fails earlier leaves no binding behind. Under `rbac.scope: cluster`, the default, none of this applies. The rules are the same as for [`certification run`](./certification.md#controller-access-to-the-run-namespace).

When `--wait` reaches its timeout, the command prints the WorkloadRun's current report, writes it to `--results-file` when requested, and exits with a timeout error. Without `--cleanup`, the WorkloadRun continues running in the cluster — the timeout stops only the CLI watch — and the timeout output includes commands to watch its progress, print an updated report, or stop it. `nvcrectl workloadrun report` exits nonzero while the WorkloadRun is still running.

### Examples

```bash
nvcrectl workloadrun run --wait my-workload.yaml
```

Pull workload images from NGC:

```bash
nvcrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  --wait my-workload.yaml
```

## nvcrectl workloadrun render

Renders the Workflow that would be created from a WorkloadRun, without applying it. Useful for offline inspection.

```bash
nvcrectl workloadrun render [flags] <workloadrun.yaml>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Override platform detection (`aws`, `gcp`, `azure`, `oci`, `onprem`, `togetherai`, `mistral`, `forge`, `nscale`, `akamai`) |
| `--gpu-arch` | none | GPU architecture for offline render, e.g. `gb300` or the product name `NVIDIA-GB300`; an unknown architecture is rejected with the valid list. Use it for a DRA-only GPU stack whose nodes carry no `nvidia.com/gpu.product` label. Wins over the `target.nodeSelector` label when set; cannot be combined with `--dry-run`, which detects the architecture from real nodes. |
| `--dry-run` | `false` | Connect to cluster, discover real nodes, and render with actual platform/GPU detection |
| `--output` | `yaml` | Output format: `yaml` or `json` |

## nvcrectl workloadrun report

Generates a report for a completed WorkloadRun, including bandwidth and goodput metrics if configured.

```bash
nvcrectl workloadrun report <name> [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--results-file` | — | Write report as JSON to this file path |
| `--output` / `-o` | `text` | Output format: `text` or `json` |

## nvcrectl workloadrun status

Prints the current status of a WorkloadRun.

```bash
nvcrectl workloadrun status <name> [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` / `-o` | `text` | Output format: `text` or `json` |

## nvcrectl workloadrun cancel

Cancels one or more running WorkloadRuns.

```bash
nvcrectl workloadrun cancel <name> [<name>...]
```
