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
| `phase` | string | `InProgress`, `Passed`, or `Failed` |
| `conditions` | []Condition | InProgress, Succeeded, Failed (mutually exclusive) |
| `iterationsCompleted` | int | Number of completed iterations |
| `appliedOverrides` | string | Annotation listing which platform/GPU overrides matched |
| `detectedGPUArchitecture` | string | Detected GPU architecture (e.g. `GB200`) |
| `detectedPlatform` | string | Detected cloud platform (e.g. `aws`) |

## Naming

Workflows are named `<certificationName>-<domain>-<variant>`.

## Lifecycle

1. Pulls `WorkflowSpec` from the catalog for the assigned `{domain, variant}`.
2. Detects platform and GPU architecture; applies matching overrides.
3. Creates a child `Job`.
4. Manages iteration count; marks Succeeded or Failed when complete.
