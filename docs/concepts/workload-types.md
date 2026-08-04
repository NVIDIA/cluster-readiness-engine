---
title: Workload Types
description: The training frameworks and workload adapters supported by the Cluster Readiness Engine.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


The `Job` controller launches workloads via an adapter pattern that normalizes multiple training frameworks to a common `WorkloadPhase` (Running / Succeeded / Failed). The adapter is selected based on which field is set in the `WorkloadSpec`.

## Supported frameworks

| Framework | Spec field | Notes |
|-----------|-----------|-------|
| Kubeflow TrainJob | `trainJob` | Default for most catalog entries; requires Kubeflow Trainer |
| MPI | `mpi` | Used for NCCL benchmark workloads via `mpirun` |
| PyTorch | `pytorch` | Directly launches `torchrun` |
| Custom | `custom` | Arbitrary pod template; user-supplied entrypoint |

## TrainJob (default)

Most catalog entries use `TrainJob` — the Kubeflow Trainer v2 API. The controller creates a `TrainJob` resource and waits for it to reach a terminal phase. Health monitoring and goodput measurement run concurrently.

## WorkloadRun

`WorkloadRun` is a higher-level, user-facing resource for ad-hoc workloads. It wraps a `WorkloadSpec` in a simplified API — you supply an image, framework config, and node count, and the controller handles platform detection, overrides, and cleanup. See [WorkloadRun Quick Start](../getting-started/workloadrun-quick-start.md).

## Phase mapping

The adapter normalizes framework-specific status to one of three phases:

| Phase | Meaning |
|-------|---------|
| `Running` | Workload pods are active |
| `Succeeded` | Workload completed successfully |
| `Failed` | Workload exited with an error or was evicted |

The `Job` controller acts on these phases to drive its own condition updates and decide whether to checkpoint-restart or mark the run as failed.
