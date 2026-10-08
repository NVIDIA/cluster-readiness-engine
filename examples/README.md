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
bandwidth. `numNodes` is the node count per job group, not the total: NVCRE
partitions all eligible nodes into groups of that size and runs one job per
group. To run a single job of `numNodes` nodes instead, see
[certification-unpinned.yaml](#certification-unpinnedyaml).

```bash
kubectl nvcre workloadrun run examples/nccl-all-reduce.yaml --wait
```

## certification.yaml

A Certification that runs two catalog categories, `communication/nccl-all-reduce`
and `training/nemotron5-8b`, across all GPU nodes in groups of four. List the
available categories with `kubectl nvcre certification list-categories`.

```bash
kubectl nvcre certification run --cert-file examples/certification.yaml --wait
```

## certification-unpinned.yaml

The same shape with `placement: Unpinned`, which runs **one** job
of exactly `nodesPerJob` nodes however many the target matches, instead of
partitioning the fleet into groups of that size. The remaining target nodes go
untested, and NVCRE sets no `kubernetes.io/hostname` affinity, so the scheduler
picks the machines within the target.

`nodesPerJob` is required here. Unpinned honors the size you ask for or fails
saying why; it is never clamped to fit the fleet or auto-selected.

```bash
kubectl nvcre certification run --cert-file examples/certification-unpinned.yaml --wait
```
