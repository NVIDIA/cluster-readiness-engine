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
| `--image-pull-secret` | — | GitHub token for `ghcr.io` image pull |
| `--image` | — | Override controller image |
| `--name` | — | Override the WorkloadRun name |
| `--node-list` | — | Comma-separated list of nodes to target |
| `--topology-domain` | — | Topology domain to target |
| `--topology-key` | — | Node label key for topology grouping |
| `--test-scale` | — | Test scale override (`intra-node`, `intra-rack`) |
| `--results-file` | — | Write results as JSON to this file path |

### Example

```bash
ncrectl workloadrun run --wait my-workload.yaml
```

## ncrectl workloadrun render

Renders the Workflow that would be created from a WorkloadRun, without applying it. Useful for offline inspection.

```bash
ncrectl workloadrun render [flags] <workloadrun.yaml>
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--platform` | auto | Override platform detection (`aws`, `gcp`, `azure`, `oci`, `mistral`, `forge`) |
| `--dry-run` | `false` | Validate against the live API server without creating resources |
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
