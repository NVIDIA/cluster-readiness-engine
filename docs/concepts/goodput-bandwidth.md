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

Built-in LogProfiles are installed by `ncrectl setup init` for supported frameworks (NeMo, PyTorch Lightning, etc.).

### Interpreting goodput

A goodput ratio of 1.0 means the job was making continuous progress. Values below ~0.95 indicate meaningful overhead — stalls, restarts, or slow nodes.

## Bandwidth measurement

A `BandwidthMeasurement` resource watches a `Job`'s NCCL log output and computes per-bus bandwidth metrics for collective operations (all-reduce, all-gather, alltoall).

### Thresholds

Each catalog entry defines expected bandwidth thresholds per GPU architecture. A `Job` that fails to meet its threshold is marked failed, which propagates up through `Workflow` to `Certification`.

### Report

`ncrectl certification report` and `ncrectl workloadrun report` display measured vs. expected bandwidth with a pass/fail indicator per collective operation.
