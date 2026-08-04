---
title: GoodputMeasurement
description: CRD reference for the GoodputMeasurement resource.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


`GoodputMeasurement` watches a `Job`'s pod logs in real time, applies regex patterns from a `LogProfile`, and computes a goodput ratio — the fraction of elapsed time the job was making useful forward progress.

## Spec fields

_Generated from CRD schema — coming soon._

## Key spec fields (summary)

| Field | Type | Description |
|-------|------|-------------|
| `jobRef` | ObjectRef | The Job to watch |
| `logProfileRef` | string | Name of the LogProfile defining parsing patterns |
| `minGoodputRatio` | float | Minimum acceptable goodput (0.0–1.0); job fails if below this |

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `goodputRatio` | float | Measured goodput ratio |
| `phase` | string | `InProgress`, `Passed`, or `Failed` |

## How it works

1. Watches pod logs via the `PodLogFetcher` interface.
2. Applies named capture group regexes from the referenced `LogProfile`.
3. Computes the ratio of time-in-progress to total elapsed time.
4. Marks `Passed` if the ratio meets `minGoodputRatio`, `Failed` otherwise.
