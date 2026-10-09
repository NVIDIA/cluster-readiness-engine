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
| `transport` | []string | Distinct NCCL network names from `NCCL INFO Using network ...` (for example `IB`, `Socket`). Empty when that line never appears. Record-only; not used for pass/fail |
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

1. While the Job runs, samples the launcher's log at `sampleInterval` and keeps a running average per message size. These results are provisional: they show progress and feed the Prometheus gauges, but no threshold is evaluated against them. `nvcrectl` reports display whatever results the measurement holds.
2. Applies the `bandwidthResult` regex pattern from the referenced `LogProfile` to extract `size`, `algBW`, and `busBW` capture groups.
3. When the LogProfile defines `networkTransport`, records the distinct set of captured `transport` names on `status.transport`. Record-only; never used for pass/fail.
4. When the Job succeeds, reads the launcher's log once more from its first line to its last, counts every result row exactly once, and replaces the provisional results with the per-size averages. The result depends only on the log, not on when samples were taken.
5. Sets `Complete`. The reason says whether the results are final:

| `Complete` reason | Results | Used for thresholds |
|-------------------|---------|---------------------|
| `JobSucceeded` | Final, from the Job's full log | Yes |
| `NoDataCollected` | None: the complete log was read and no line matched the `bandwidthResult` pattern | No |
| `LogsUnavailable` | Provisional, or none: the complete log could not be read (for example the launcher pod or its log was gone, no pod matched the `LogProfile`'s worker selection, or the `LogProfile` did not resolve), or the Job was deleted or replaced. The condition message gives the cause | No |
| `JobFailed` | Final if the log could still be read, otherwise provisional | No; a failed Job is not evaluated |

A threshold on `busBandwidthGBps` or `algBandwidthGBps` is evaluated only against final results. Any other outcome leaves the value unmeasured, and the Job fails validation once its `measurementTimeout` expires. In diagnose mode, the group is treated as failed.

If the final read fails, for example because the launcher pod's log is briefly unreachable, `Measuring` is set to `False` with reason `FinalReadPending` and the read is retried for as long as the Job waits for measurement data: its `measurementTimeout` (5m by default), and never less than two minutes. After that the measurement completes as `LogsUnavailable`. A log that can never be read in full, for example one with a line longer than 1 MiB, completes as `LogsUnavailable` at once.

Set `measurementTimeout` as a Go duration such as `10m`: `options.measurementTimeout` on a Certification category (or at the top of the Certification spec for every category), `spec.validation.performance.measurementTimeout` on a Workflow, or `spec.measurementTimeout` on a Job.

The final read covers the container's current log file. If the kubelet rotated the file while the Job ran, which happens only when the container logs more than the node's `containerLogMaxSize` (10Mi by default), rows in the rotated file are not part of the final results.
