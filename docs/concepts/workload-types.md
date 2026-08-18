---
title: Workload Types
description: The training frameworks and workload adapters supported by the Cluster Readiness Engine.
---
<!-- SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. -->
<!-- SPDX-License-Identifier: Apache-2.0 -->


The `Job` controller launches workloads via an adapter pattern that normalizes workloads to a common `WorkloadPhase` (Running / Succeeded / Failed).

## Job API: TrainJob only

`Job.spec.workload` is a discriminated union with exactly **one** field:

| Field | Type | Notes |
|-------|------|-------|
| `trainJob` | TrainJobSpec | Kubeflow Trainer v2 TrainJob; requires Kubeflow Trainer installed |

All catalog entries produce a `TrainJob` as the underlying workload. The `Job` controller creates the `TrainJob` resource and normalizes its status to `WorkloadPhase` via `pkg/workload/ForSpec()`.

## WorkloadRun: CLI-layer framework types

`WorkloadRun` is a user-facing simplified API that accepts higher-level framework types (`torch`, `mpi`, `exec`) and translates them into a `TrainJob` spec automatically. These framework types live in the `WorkloadRun` API — they are **not** fields in the `Job.spec.workload` discriminated union.

| `WorkloadRun` framework | What it generates |
|------------------------|-------------------|
| `torch` | TrainJob with PyTorch `mlPolicy`, `torchrun` |
| `mpi` | TrainJob with MPI `TrainingRuntime`, launcher+worker pattern |
| `exec` | TrainJob with a single replicatedJob, arbitrary command |

See [WorkloadRun Quick Start](../getting-started/workloadrun-quick-start.md) and the [WorkloadRun API reference](../api-reference/workloadrun.md).

## Phase mapping

The adapter normalizes framework-specific status to one of three phases:

| Phase | Meaning |
|-------|---------|
| `Running` | Workload pods are active |
| `Succeeded` | Workload completed successfully |
| `Failed` | Workload exited with an error or was evicted |

The `Job` controller acts on these phases to drive its own condition updates and decide whether to checkpoint-restart or mark the run as failed.
