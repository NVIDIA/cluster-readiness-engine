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
| `results` | []BandwidthResult | Per-message-size average bandwidth measurements |
| `startTime` | Time | When measurement started (when the referenced Job began running) |
| `completionTime` | Time | When measurement completed (when the referenced Job reached a terminal state) |
| `conditions` | []Condition | Current state: `Measuring` (in progress) or `Complete` (finished) |

Each `BandwidthResult` entry contains:

| Field | Type | Description |
|-------|------|-------------|
| `sizeBytes` | int64 | Message size in bytes |
| `algBW` | string | Average algorithmic bandwidth in GB/s |
| `busBW` | string | Average bus bandwidth in GB/s |
| `samples` | int | Number of measurements averaged for this size |

## How it works

1. Watches the pod logs of the referenced Job at `sampleInterval`.
2. Applies the `bandwidthResult` regex pattern from the referenced `LogProfile` to extract `size`, `algBW`, and `busBW` capture groups.
3. Computes a running average per message size across all observed measurements.
4. Sets `Complete` when the Job reaches a terminal state.
