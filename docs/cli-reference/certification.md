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
| `--timeout` | derived | Timeout for `--wait`. When not set, derived from the selected categories' `timeoutPerJob` budgets (max across categories × iterations × 1.5), floored at `30m`; the CLI prints the derived value when the watch starts. An explicit value always wins. On timeout, the CLI prints a partial report and leaves the Certification running unless `--cleanup` is set. |
| `--cleanup` | `false` | Delete the certification, namespace, and installed components after completion |
| `--nodes-per-job` | `0` | Nodes per job (0 = auto-select) |
| `--gpus-per-node` | `0` | GPUs per node (0 = auto-detect from GPU architecture) |
| `--enable-checkpoint` | `false` | Enable checkpoint storage for training workloads |
| `--enable-mnnvl` | `false` | Enable Multi-Node NVLink (`NCCL_MNNVL_ENABLE=1`) |
| `--max-steps` | `0` | Max training steps for NeMo 4 workloads (0 = catalog default) |
| `--exit-duration-mins` | `0` | Training duration in minutes for NeMo 6 workloads (0 = catalog default) |
| `--repeat-count` | `0` | Orchestration iterations to repeat tests (0 = catalog default) |
| `--max-restarts` | `0` | Maximum checkpoint restarts for training workloads (0 = catalog default) |
| `--storage-class` | — | StorageClass for PVC dependencies created by catalog entries |
| `--results-file` | — | Write the certification report as JSON to this path (requires `--wait`) |
| `--controller-pull-secret` | — | Token for controller registry auth during `--setup` (e.g. GitHub PAT for `ghcr.io`) — separate from workload image credentials |
| `--workload-registry` | — | Registry server for workload image pull (e.g. `nvcr.io`, `ghcr.io`) — required when `--workload-registry-password` is set |
| `--workload-registry-username` | — | Registry username for workload image pull (e.g. `$oauthtoken` for NGC) — required when `--workload-registry-password` is set |
| `--workload-registry-password` | — | Registry password or API key — creates an `nvcrectl-pull-<name>` imagePullSecret in the namespace, deleted automatically when the Certification is deleted |

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
| `--dry-run` | `false` | Validate against the live API server without creating resources |
| `--output` | `yaml` | Output format: `yaml` or `json` |

A Certification that sets `spec.gangScheduler` has that applied to the rendered output too: the scheduler name and the `kai.scheduler/queue` label appear in the rendered manifests, so what you inspect matches what the controller creates. There is no flag for it; the field is set in the Certification YAML.

## nvcrectl certification report

Fetches a completed Certification from the cluster and generates a pass/fail report. Multiple names can be provided to combine them into a single report.

```bash
nvcrectl certification report <name> [<name>...] [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--results-file` | — | Write the report as JSON to this file path |

## nvcrectl certification list-categories

Lists all available catalog categories that can be used in a Certification.

```bash
nvcrectl certification list-categories [flags]
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` | `table` | Output format: `table`, `yaml`, or `json` |
