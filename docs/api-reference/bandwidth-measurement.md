---
title: BandwidthMeasurement
description: CRD reference for the BandwidthMeasurement resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`BandwidthMeasurement` watches a `Job`'s NCCL log output and computes per-bus bandwidth metrics for collective operations.

## Spec fields

_Generated from CRD schema — coming soon._

## Key spec fields (summary)

| Field | Type | Description |
|-------|------|-------------|
| `jobRef` | ObjectRef | The Job to watch |
| `logProfileRef` | string | Name of the LogProfile for NCCL output parsing |
| `testType` | string | Collective type: `all_reduce`, `all_gather`, `alltoall` |
| `minBusBandwidthGBps` | float | Minimum acceptable bus bandwidth in GB/s |

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `measuredBusBandwidthGBps` | float | Measured bus bandwidth |
| `phase` | string | `InProgress`, `Passed`, or `Failed` |

## How it works

1. Parses NCCL test output from pod logs.
2. Extracts bus bandwidth from the results table for the configured `testType`.
3. Compares against `minBusBandwidthGBps` (set by catalog entry or user override).
4. Marks `Passed` or `Failed` accordingly.
