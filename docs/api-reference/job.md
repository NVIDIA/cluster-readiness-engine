---
title: Job
description: CRD reference for the Job resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`Job` creates and monitors the actual workload (a `TrainJob` or other adapter-supported resource). It is created by the `Workflow` controller and is not typically created directly by users.

## Spec fields

_Generated from CRD schema — coming soon._

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Exclusive set: `InProgress`, `Succeeded`, `Failed`. Independent (additive): `HardwareFailed` (can be True alongside execution state), `ValidationFailed` (can be True alongside `Succeeded`) |
| `workloadRef` | WorkloadReference | Reference to the created workload (`TrainJob`) |
| `failedNodes` | []FailedNode | Nodes identified as failed; each entry has `name`, `reason`, and optional `message` |
| `restartCount` | int32 | Number of checkpoint-based restarts |
| `failureLog` | FailureLog | Tail of pod logs from the most recent failure (pod name, node, exit code, log tail) |

Each `FailedNode` entry has:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Kubernetes node name |
| `reason` | string | `HardwareFailureDetected`, `ThresholdViolation`, or `WorkloadFailed` |
| `message` | string | Detailed failure message |

`GoodputMeasurement` and `BandwidthMeasurement` resources reference the Job via their own `spec.jobRef` — the Job does not hold references to them.

## Naming

Jobs are named `<workflowName>-job`.

## Lifecycle

1. Creates the workload via the adapter pattern (selects adapter from `WorkloadSpec`).
2. Runs `NodeFailureDetector` concurrently.
3. Optionally creates `GoodputMeasurement` or `BandwidthMeasurement`.
4. On workload completion, evaluates health results and performance thresholds.
5. Marks Succeeded or Failed.
