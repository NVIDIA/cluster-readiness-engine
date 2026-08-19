---
title: ncrectl workloadrun
description: Run and report WorkloadRun resources.
---
<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->


## ncrectl workloadrun run

Applies a WorkloadRun manifest to the cluster and optionally waits for completion.

```bash
ncrectl workloadrun run [flags] <file>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--wait` | `false` | Block until the workload completes |
| `--timeout` | `30m` | Timeout for `--wait` |
| `--setup` | `false` | Install CRDs, controller, and LogProfiles before creating the WorkloadRun |
| `--cleanup` | `false` | Delete the WorkloadRun and installed components after completion |
| `--image` | — | Override controller image |
| `--controller-pull-secret` | — | Token for controller registry auth during `--setup` (e.g. GitHub PAT for `ghcr.io`) — separate from workload image credentials |
| `--workload-registry` | — | Registry server for workload image pull (e.g. `nvcr.io`, `ghcr.io`) — required when `--workload-registry-password` is set |
| `--workload-registry-username` | — | Registry username for workload image pull (e.g. `$oauthtoken` for NGC) — required when `--workload-registry-password` is set |
| `--workload-registry-password` | — | Registry password or API key — creates an `ncrectl-pull-<name>` imagePullSecret in the namespace, deleted automatically when the WorkloadRun is deleted |
| `--name` | — | Override the WorkloadRun name |
| `--node-list` | — | Comma-separated list of nodes to target |
| `--topology-domain` | — | Topology domain to target |
| `--topology-key` | — | Node label key for topology grouping |
| `--test-scale` | — | Override testScale (`intra-node`, `intra-rack`, `full-scale`) |
| `--results-file` | — | Write results as JSON to this file path |

### Examples

```bash
ncrectl workloadrun run --wait my-workload.yaml
```

Pull workload images from NGC:

```bash
ncrectl workloadrun run \
  --workload-registry nvcr.io \
  --workload-registry-username '$oauthtoken' \
  --workload-registry-password "$NGC_API_KEY" \
  --wait my-workload.yaml
```

## ncrectl workloadrun render

Renders the Workflow that would be created from a WorkloadRun, without applying it. Useful for offline inspection.

```bash
ncrectl workloadrun render [flags] <workloadrun.yaml>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Override platform detection (`aws`, `gcp`, `azure`, `oci`, `onprem`, `togetherai`, `mistral`, `forge`) |
| `--dry-run` | `false` | Connect to cluster, discover real nodes, and render with actual platform/GPU detection |
| `--output` | `yaml` | Output format: `yaml` or `json` |

## ncrectl workloadrun report

Generates a report for a completed WorkloadRun, including bandwidth and goodput metrics if configured.

```bash
ncrectl workloadrun report <name> [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--results-file` | — | Write report as JSON to this file path |
| `--output` / `-o` | `text` | Output format: `text` or `json` |

## ncrectl workloadrun status

Prints the current status of a WorkloadRun.

```bash
ncrectl workloadrun status <name> [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` / `-o` | `text` | Output format: `text` or `json` |

## ncrectl workloadrun cancel

Cancels one or more running WorkloadRuns.

```bash
ncrectl workloadrun cancel <name> [<name>...]
```
