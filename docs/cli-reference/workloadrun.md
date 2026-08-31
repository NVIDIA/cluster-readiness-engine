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
| `--timeout` | `30m` | Timeout for `--wait`. On timeout, the CLI prints a partial report and leaves the WorkloadRun running in the cluster unless `--cleanup` is set |
| `--setup` | `false` | Install CRDs, controller, and LogProfiles before creating the WorkloadRun |
| `--cleanup` | `false` | Delete the WorkloadRun, the namespace (when created by this run), and installed components after completion |
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
| `--platform` | auto | Override platform detection (`aws`, `gcp`, `azure`, `oci`, `onprem`, `togetherai`, `mistral`, `forge`, `nscale`) |
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
