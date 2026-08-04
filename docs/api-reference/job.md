---
title: Job
description: CRD reference for the Job resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`Job` creates and monitors the actual workload (a `TrainJob` or other adapter-supported resource). It is created by the `Workflow` controller and is not typically created directly by users.

## Spec fields

_Generated from CRD schema — coming soon._

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `phase` | string | `InProgress`, `Passed`, or `Failed` |
| `conditions` | []Condition | InProgress, Succeeded, Failed (mutually exclusive) |
| `workloadRef` | ObjectRef | Reference to the created workload (TrainJob, etc.) |
| `goodputMeasurementRef` | ObjectRef | Reference to the GoodputMeasurement (if configured) |
| `bandwidthMeasurementRef` | ObjectRef | Reference to the BandwidthMeasurement (if configured) |
| `failedNodes` | []string | Nodes identified as failed by health monitors |

## Naming

Jobs are named `<workflowName>-job`.

## Lifecycle

1. Creates the workload via the adapter pattern (selects adapter from `WorkloadSpec`).
2. Runs `NodeFailureDetector` concurrently.
3. Optionally creates `GoodputMeasurement` or `BandwidthMeasurement`.
4. On workload completion, evaluates health results and performance thresholds.
5. Marks Succeeded or Failed.
