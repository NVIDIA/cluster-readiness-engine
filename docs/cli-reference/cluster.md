---
title: nvcrectl cluster
description: Inspect GPU nodes, platform detection, and network topology.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## nvcrectl cluster info

Discovers GPU nodes in the cluster and reports the detected platform, GPU architecture, per-node GPU count, and network topology (rack/T1 leaf switch grouping).

A node's GPU count is its allocatable `nvidia.com/gpu`. On a DRA-only GPU stack, where no node advertises that resource, the count is the number of full GPUs the node publishes in its `gpu.nvidia.com` ResourceSlices.

```bash
nvcrectl cluster info [flags]
```

The topology key is auto-detected per cloud platform:

| Platform | Topology label |
|----------|---------------|
| AWS | `topology.k8s.aws/network-node-layer-1` |
| GCP | `cloud.google.com/gce-topology-block` |
| Azure | `kubernetes.azure.com/ppg` |

Use `--topology-key` to override for other environments.

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--output` / `-o` | `table` | Output format: `table`, `json`, or `yaml` |
| `--topology-key` | auto | Node label for topology grouping |

### Example

```bash
# Table output (default)
nvcrectl cluster info

# JSON output for scripting
nvcrectl cluster info -o json
```

### Sample output

```
Platform:     aws
GPU:          NVIDIA H100 80GB HBM3 (h100, 8 GPUs/node)
Nodes:        16 ready

TOPOLOGY (topology.k8s.aws/network-node-layer-1):
  rack-a    8 nodes
  rack-b    8 nodes
```
