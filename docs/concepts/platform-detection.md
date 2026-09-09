---
title: Platform Detection & Overrides
description: How the controller auto-detects cloud platform and GPU architecture, and how overrides are applied.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Auto-detection

The controller detects two dimensions at runtime:

| Dimension | Source |
|-----------|--------|
| Cloud platform | `spec.providerID` on the Node object (e.g. `aws://...`, `gce://...`) |
| GPU architecture | `nvidia.com/gpu.product` node label (e.g. `NVIDIA-GB200`, `NVIDIA-H100-80GB-HBM3`) |

On a mixed-architecture target, the detected GPU architecture is the one reported by the most nodes, with ties resolved to the architecture whose earliest node sorts first by name. Nodes missing the `nvidia.com/gpu.product` label do not participate in that vote, so an unlabeled node never outvotes labeled ones; `unknown` is detected only when no target node carries the label. Every path uses the same rule: the Certification, Workflow, and WorkloadRun controllers as well as the `nvcrectl` render, cluster info, and workloadrun commands.

The live controller writes detection results to `status.orchestration.detectedPlatform` and `status.orchestration.detectedGPUArchitecture` on the Workflow. When using `nvcrectl workflow render` (client-side), these values are also written as annotations (`nvcrectl.nvidia.com/detected-platform`, `nvcrectl.nvidia.com/detected-gpu-architecture`) on the rendered manifest for offline inspection.

## Override matching

Catalog entries define a base `WorkflowSpec`. Overrides are matched by platform + GPU architecture and applied in order. Two patch mechanisms are supported:

### `jobTemplate` (strategic merge patch)

Replaces entire arrays. Use when you want to set all values for a field (e.g. the full `env` list):

```yaml
jobTemplate:
  spec:
    workload:
      trainJob:
        trainer:
          env:
            - name: MY_VAR
              value: "value"
```

<Warning>
Arrays in `jobTemplate` overrides are **replaced entirely**, not merged by name. If the base spec has env vars, they will be wiped unless you include them in the override.
</Warning>

### `jobTemplatePatch` (RFC 6902 JSON Patch)

Appends or modifies individual fields without replacing arrays. Use `op: add` with path ending in `/-` to append to an array:

```yaml
jobTemplatePatch:
  - op: add
    path: /spec/workload/trainJob/trainer/env/-
    value:
      name: EXTRA_VAR
      value: "extra"
```

<Warning>
A `jobTemplatePatch` that appends to an array must come **after** any `jobTemplate` override that sets that array, or the appended values will be wiped.
</Warning>

## Architecture-specific resources

Different GPU architectures and cloud platforms require different Kubernetes resources:

| Architecture | Platform | Interconnect | Key resources |
|-------------|---------|-------------|--------------|
| GB200 | AWS | EFA | `hugepages-2Mi`, `vpc.amazonaws.com/efa`, EFA hostPath volume, ComputeDomain |
| GB200 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| GB300 | AWS | RoCE | `roce-channel` resource claim (DRA), no hugepages, no EFA |
| GB300 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| H100 | AWS | EFA | `vpc.amazonaws.com/efa: 32`, no hugepages |
| H100 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap |
| GB200/GB300 | On-prem | InfiniBand | arm64/GPU taint tolerations, portable IB NCCL env (no HCA pinning), NIC resource auto-detected or set via `nicResourceName`, ComputeDomain |

The live controller tracks which overrides matched in `status.orchestration.appliedOverrides`. When using `nvcrectl workflow render`, the same information is also written to the `nvcrectl.nvidia.com/applied-overrides` annotation on the rendered manifest.

## On-prem clusters

`onprem` is the detection fallback: a node with an empty `spec.providerID` (and no Forge hostname or NKE site-name label) resolves to the `onprem` platform. Nodes provisioned by bare-metal stacks such as BCM typically carry no providerID, so they land here without any configuration.

For GB200/GB300 targets, the on-prem override contributes:

- **Tolerations** for the `kubernetes.io/arch=arm64:NoSchedule` and `nvidia.com/gpu=present:NoSchedule` taints common on NVL72 deployments. Without them, workload pods never schedule on tainted arm64 nodes.
- **A portable InfiniBand NCCL environment** without HCA pinning: NCCL auto-detects HCAs when `NCCL_IB_HCA` is unset, so the same override works across sites with different HCA layouts.
- **A NIC resource request**, detected automatically or configured via `nicResourceName`. Resource names vary by RDMA device plugin (`rdma/ib`, `nvidia.com/mlnxnics`, and others are all in the wild), so when the field is unset the controller inspects the target nodes: if exactly one candidate resource (any `rdma/*` name, or the exact name `nvidia.com/mlnxnics`) is allocatable on every target node, that name is requested. Detection never guesses: with zero candidates, or with several, no NIC resource is requested (pods still schedule, without an explicit NIC allocation) and a Normal `NICResourceDetection` event on the Certification or WorkloadRun lists what was found. Set `nicResourceName` to override detection or to resolve an ambiguous fleet. The per-container count always comes from `mlnxPerNode`, detected or not. GB200/GB300 default `mlnxPerNode` to 8; sites running a shared-device plugin (one pooled resource per pod) should set `mlnxPerNode: 1`.

Offline `nvcrectl certification render` and `nvcrectl workloadrun render` have no cluster to inspect, so without `--dry-run` they render field-only: the NIC resource appears only when `nicResourceName` is set. With `--dry-run`, real nodes are discovered and detection runs exactly as in the controllers.

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: onprem-gb300-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB300
  nodesPerJob: 18
  enableMNNVL: true
  # Extended resource name advertised by the site's RDMA device plugin.
  # Optional: when omitted, the controller auto-detects a single qualifying
  # rdma/* or nvidia.com/mlnxnics resource allocatable on every target node.
  # Set it to override detection or when several candidates are advertised.
  nicResourceName: rdma/ib
  mlnxPerNode: 1   # shared-device plugin: one pooled resource per pod
  categories:
    - domain: communication
      variant: nccl-all-reduce
```

<Warning>
**Metal3 detection trap.** Nodes with a `metal3://` providerID resolve to the `mistral` platform, not `onprem`. An on-prem site provisioned by Cluster API + Metal3 therefore silently picks up the Mistral site-specific override, which pins HCA names (`NCCL_IB_HCA`, `UCX_NET_DEVICES`) and the `rdma/ib` resource name. To preview what a generic on-prem render looks like regardless of providerID, pass the platform explicitly: `nvcrectl certification render --platform onprem <cert.yaml>`.
</Warning>

Diagnostics (`dcgm-level4`) needs no on-prem override: it already tolerates all taints and runs intra-node, so it schedules on tainted arm64 nodes unchanged.
