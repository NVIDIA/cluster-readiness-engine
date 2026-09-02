---
title: LogProfile
description: CRD reference for the LogProfile resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`LogProfile` is a cluster-scoped resource that defines regex patterns with named capture groups for parsing training framework log output. It is referenced by `GoodputMeasurement` and `BandwidthMeasurement` resources.

## Example

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: LogProfile
metadata:
  name: my-framework
spec:
  timestamp:
    layout: "2006-01-02T15:04:05.999999999Z"
  patterns:
    trainingStep:
      regex: 'reduced_train_loss=(?P<loss>[0-9.]+).*step_time=(?P<stepTiming>[0-9.]+)'
      example: 'reduced_train_loss=0.1234 step_time=2.5'
      units:
        stepTiming: s
    applicationStart:
      regex: 'Training started'
```

## Spec fields

### `spec.timestamp` (required)

Configures how to parse `(?P<timestamp>...)` named captures from log lines.

| Field | Type | Description |
|-------|------|-------------|
| `layout` | string (required) | Go time layout string for parsing captured timestamps. Example: `"2006-01-02T15:04:05.999999999Z"` |

### `spec.patterns`

A `LogPatternSet` — a named struct (not a list) with 8 optional fields, each an `EventPattern`. Each pattern uses Go named capture groups `(?P<name>...)`.

| Field | Captures | Description |
|-------|----------|-------------|
| `trainingStep` | `timestamp`, `globalStep`, `iteration`, `epoch`, `stepTiming`, `loss`, `tflops`, `elapsedTime` | Training iteration log lines |
| `checkpointSave` | `timestamp`, `step`, `path`, `saveDuration` | Checkpoint save start |
| `checkpointDone` | `timestamp`, `step`, `path` | Checkpoint save completion |
| `checkpointRestore` | `timestamp`, `path`, `step` | Checkpoint load start |
| `checkpointLoaded` | `timestamp`, `path`, `step` | Checkpoint load completed |
| `applicationStart` | `timestamp` | Application framework start marker |
| `warmupStep` | — | Warmup/startup iteration marker (presence of match is sufficient) |
| `bandwidthResult` | `size`, `algBW`, `busBW` | NCCL bandwidth test result lines (used by `BandwidthMeasurement`) |

Each `EventPattern` has:

| Field | Type | Description |
|-------|------|-------------|
| `regex` | string (required) | Go regex with named capture groups |
| `example` | string | Sample log line the regex should match (documentation + validation) |
| `units` | map[string]string | Units for duration captures: `"s"`, `"ms"`, `"us"`. Only needed for `stepTiming`, `elapsedTime`, `saveDuration`. Defaults to `"s"`. |

### Additional spec fields

| Field | Type | Description |
|-------|------|-------------|
| `workerStrategy` | WorkerStrategySpec | How to read logs from multi-worker jobs (`Single` or `Multi`). Default: Single (worker-0 only). |
| `containerName` | string | Container to read logs from. Auto-detected if empty. |
| `warmupSteps` | int | Number of initial steps per run to flag as warmup and exclude from goodput calculations. |
| `logInterval` | int | Training log interval (e.g., `--log-interval 10`). Filters intermediate steps. |

## Built-in profiles

The following LogProfiles are installed by the Helm chart:

| Name | Framework |
|------|-----------|
| `megatron-training` | Megatron-LM training logs |
| `megatron-bridge` | Megatron bridge logs |
| `nccl-bandwidth` | NCCL test output (bandwidth results) |
| `nccl-loopback` | NCCL loopback test output |

## Scope

LogProfiles are cluster-scoped — a single profile can be referenced by Jobs across all namespaces.
