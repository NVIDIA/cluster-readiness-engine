---
title: BandwidthMeasurement
description: CRD reference for the BandwidthMeasurement resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`BandwidthMeasurement` watches a `Job`'s NCCL log output and computes per-message-size bus bandwidth metrics for collective operations. It is automatically created by the `Job` controller when `spec.bandwidthMeasurement` is configured, or can be created manually.

## Spec fields

| Field | Type | Description |
|-------|------|-------------|
| `jobRef` | TypedLocalObjectReference | The Job whose pod logs to watch |
| `logProfileRef` | string (required) | Name of the cluster-scoped `LogProfile` that defines the `bandwidthResult` regex pattern |
| `sampleInterval` | Duration | How often to sample pod logs while the Job is running. Default: 60s |
| `testType` | string | NCCL collective operation identifier (e.g., `all_reduce`, `alltoall`). Used as the `nccl_test` Prometheus label |

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `results` | []BandwidthResult | Per-message-size average bandwidth. Provisional while the Job runs; final once `Complete` is `True` with reason `JobSucceeded` |
| `startTime` | Time | When measurement started (when the referenced Job began running) |
| `completionTime` | Time | When the referenced Job reached a terminal state |
| `conditions` | []Condition | Current state: `Measuring` (in progress) or `Complete` (finished) |

Each `BandwidthResult` entry contains:

| Field | Type | Description |
|-------|------|-------------|
| `sizeBytes` | int64 | Message size in bytes |
| `algBW` | string | Average algorithmic bandwidth in GB/s |
| `busBW` | string | Average bus bandwidth in GB/s |
| `samples` | int | Number of result rows averaged for this size. In final results, the number of times the log reports that size, usually one per test cycle |

## How it works

1. While the Job runs, samples the launcher's log at `sampleInterval` and keeps a running average per message size. These results are provisional: they show progress and feed the Prometheus gauges, but no verdict uses them.
2. Applies the `bandwidthResult` regex pattern from the referenced `LogProfile` to extract `size`, `algBW`, and `busBW` capture groups.
3. When the Job succeeds, reads the launcher's log once more from its first line to its last, counts every result row exactly once, and replaces the provisional results with the per-size averages. The result depends only on the log, not on when samples were taken.
4. Sets `Complete`. The reason says whether the results are final:

| `Complete` reason | Results | Used for thresholds |
|-------------------|---------|---------------------|
| `JobSucceeded` | Final, from the Job's full log | Yes |
| `NoDataCollected` | None: nothing in the log matched the `bandwidthResult` pattern | No |
| `LogsUnavailable` | Provisional: the full log could not be read, or the Job no longer exists | No |
| `JobFailed` | Final if the log could still be read, otherwise provisional | No; a failed Job is not evaluated |

A threshold on `busBandwidthGBps` or `algBandwidthGBps` is evaluated only against final results. Any other outcome leaves the value unmeasured, and the Job fails validation once its `measurementTimeout` expires. In diagnose mode, the group is treated as failed.

If the final read fails, for example because the launcher pod's log is briefly unreachable, `Measuring` is set to `False` with reason `FinalReadPending` and the read is retried for as long as the Job waits for measurement data: its `measurementTimeout` (5m by default), and never less than two minutes. After that the measurement completes as `LogsUnavailable`.

The final read covers the container's current log file. If the kubelet rotated the file while the Job ran, which happens only when the container logs more than the node's `containerLogMaxSize` (10Mi by default), rows in the rotated file are not part of the final results.
