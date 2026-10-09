<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->

# Examples

Runnable manifests for the NVIDIA Cluster Readiness Engine (NVCRE). Install
the CLI and run `kubectl nvcre setup init` first; the [Quickstart](../README.md#quickstart)
covers both.

Each manifest is a plain custom resource, so `kubectl apply -f <file>` also
works once the CRDs and controller are installed. The `kubectl nvcre` commands
below additionally wait for completion and print the report.

## nccl-all-reduce.yaml

A WorkloadRun that runs the NCCL all-reduce benchmark and measures per-bus
bandwidth. By default `numNodes` is the node count per job group, not the total:
NVCRE partitions all eligible nodes into groups of that size and runs one job per
group. To run a single job of `numNodes` nodes instead, see
[workloadrun-unpinned.yaml](#workloadrun-unpinnedyaml).

```bash
kubectl nvcre workloadrun run examples/nccl-all-reduce.yaml --wait
```

## workloadrun-unpinned.yaml

The same benchmark with `orchestration.placement: Unpinned`, which runs **one**
job of exactly `numNodes` nodes however many the target matches, instead of
partitioning the fleet into groups of that size. The remaining nodes are left
alone, and NVCRE sets no `kubernetes.io/hostname` affinity, so the scheduler
picks the machines within the target.

`numNodes` is honored exactly under Unpinned: it is never clamped to fit the
fleet, and a size that cannot run fails saying why.

Unpinned is a WorkloadRun mode only. A Certification always covers every node
its target matches, because that is what its verdict claims.

```bash
kubectl nvcre workloadrun run examples/workloadrun-unpinned.yaml --wait
```

## certification.yaml

A Certification that runs two catalog categories, `communication/nccl-all-reduce`
and `training/nemotron5-8b`, across all GPU nodes in groups of four. List the
available categories with `kubectl nvcre certification list-categories`.

```bash
kubectl nvcre certification run --cert-file examples/certification.yaml --wait
```

