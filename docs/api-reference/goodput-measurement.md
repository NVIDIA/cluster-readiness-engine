---
title: GoodputMeasurement
description: CRD reference for the GoodputMeasurement resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`GoodputMeasurement` watches a `Job`'s pod logs in real time, applies regex patterns from a `LogProfile`, and computes a goodput ratio — the fraction of elapsed training time the job was making useful forward progress. It is automatically created by the `Job` controller when `spec.goodputMeasurement` is configured, or can be created manually.

## Spec fields

| Field | Type | Description |
|-------|------|-------------|
| `jobRef` | TypedLocalObjectReference | The Job whose pod logs to watch |
| `logProfileRef` | string (required) | Name of the cluster-scoped `LogProfile` defining parsing patterns |
| `sampleInterval` | Duration | How often to sample pod logs while the Job is running. Default: 60s |

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `result` | string | Current runtime goodput ratio (0.0–1.0) as a string |
| `startTime` | Time | When measurement started |
| `completionTime` | Time | When measurement completed |
| `conditions` | []Condition | Current state: `Measuring` (in progress) or `Complete` (finished) |

Additional diagnostic fields (all optional):

| Field | Type | Description |
|-------|------|-------------|
| `currentStep` | int | Latest training step observed from logs |
| `highestStep` | int | Highest training step ever reached (may exceed `currentStep` after restart) |
| `trainingTimeSec` | string | Total training wall-clock time in seconds |
| `lostWorkTimeSec` | string | Cumulative lost work time across all interruptions |
| `interruptionCount` | int | Number of interruptions detected |
| `avgStepTimeSec` | string | Average time per training step (excluding warmup) |
| `avgTFLOPSPerGPU` | string | Average TFLOPS per GPU from training step logs |

## How it works

1. Watches pod logs via the `PodLogFetcher` interface at `sampleInterval`.
2. Applies named capture group regexes from the referenced `LogProfile`.
3. Computes the goodput ratio: `(t_w - t_ch - t_rm - t_re - t_save) / (t_w - t_re)` where `t_w` = training time, `t_ch` = lost work time, `t_rm` = resume time, `t_re` = reschedule time, `t_save` = checkpoint save time.
4. Sets `Complete` when the referenced Job reaches a terminal state.

Threshold evaluation (pass/fail) is done at the Job tier, not here — see `Job.spec.thresholds`.
