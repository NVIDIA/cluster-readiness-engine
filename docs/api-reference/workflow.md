---
title: Workflow
description: CRD reference for the Workflow resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`Workflow` manages a single certification category run. It is created by the `Certification` controller — one per category — and is not typically created directly by users.

## Spec fields

_Generated from CRD schema — coming soon._

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Mutually exclusive state: `InProgress`, `Succeeded`, `Failed`, `ValidationFailed` |
| `namespace` | string | Resolved namespace where Jobs and dependencies are created |
| `succeededNodesRef` | TypedLocalObjectReference | ConfigMap reference for the succeeded-nodes list |
| `failedNodesRef` | TypedLocalObjectReference | ConfigMap reference for the failed-nodes list |
| `orchestration.completedIterations` | int | Number of fully completed iterations |
| `orchestration.currentIteration` | int | The iteration currently in progress (1-based) |
| `orchestration.totalNodes` | int | Total nodes discovered from the target |
| `orchestration.nodesPerJob` | int | Nodes per job (auto-detected from workload template) |
| `orchestration.detectedGPUArchitecture` | string | Detected GPU architecture (e.g. `gb200`) |
| `orchestration.detectedPlatform` | string | Detected cloud platform (e.g. `aws`) |
| `orchestration.appliedOverrides` | []AppliedOverride | Which spec overrides matched and were applied |
| `orchestration.groups` | []GroupStatus | Per-group job status for the current iteration |

## Naming

Workflows are named `<certificationName>-<domain>-<variant>`.

## Lifecycle

1. Pulls `WorkflowSpec` from the catalog for the assigned `{domain, variant}`.
2. Detects platform and GPU architecture; applies matching overrides.
3. Creates a child `Job`.
4. Manages iteration count; marks Succeeded or Failed when complete.
