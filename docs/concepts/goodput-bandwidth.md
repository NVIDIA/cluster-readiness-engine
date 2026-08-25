---
title: Goodput & Bandwidth Measurement
description: How the Cluster Readiness Engine measures training throughput and network bandwidth.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Goodput measurement

Goodput is the fraction of elapsed time during which a training job is making useful progress (i.e. not stalled, restarting, or doing overhead work). A `GoodputMeasurement` resource watches a `Job`'s pod logs in real time, applies regex patterns from a `LogProfile`, and computes the ratio.

### LogProfile

A `LogProfile` (cluster-scoped) defines named regex capture groups that match log lines from a training framework. The goodput calculator uses the captured timestamps and step counts to determine when the job was making progress.

Built-in LogProfiles installed by `ncrectl setup init`: `megatron-training`, `megatron-bridge`, `nccl-bandwidth`, `nccl-loopback`.

### Measurement lifecycle

While the measured `Job` is running, the measurement's `Measuring` condition is `True` and every value in its status — including `result` — is **provisional**: it is recomputed on each sampling pass and changes as the run progresses.

When the `Job` reaches a terminal state, the controller performs one final log read and folds it into a single terminal status write, anchored to the timestamp of the Job's terminal condition rather than the controller's wall clock: `completionTime` is the Job's terminal transition time, and `trainingTimeSec` (and therefore `result`) is measured up to that instant. That write sets `Complete=True` and `Measuring=False`.

Once `Complete` is `True` the measurement is **frozen**: its status never changes again, so `ncrectl certification report` returns the same goodput on every read, and controller restarts or repeated reconciles cannot move the value (see [ADR-072](../designs/072-goodput-terminal-freeze.md)). Goodput-based threshold evaluation (`goodputRatio`, `avgTFLOPSPerGPU`, `avgStepTimeSec`) waits for `Complete=True` and only ever consumes the frozen values.

### Interpreting goodput

A goodput ratio of 1.0 means the job was making continuous progress. Values below ~0.95 indicate meaningful overhead — stalls, restarts, or slow nodes.

## Bandwidth measurement

A `BandwidthMeasurement` resource watches a `Job`'s NCCL log output and computes per-bus bandwidth metrics for collective operations (all-reduce, all-gather, alltoall).

### Thresholds

Each catalog entry defines expected bandwidth thresholds per GPU architecture. A `Job` that fails to meet its threshold is marked failed, which propagates up through `Workflow` to `Certification`.

### Report

`ncrectl certification report` and `ncrectl workloadrun report` display measured vs. expected bandwidth with a pass/fail indicator per collective operation.
