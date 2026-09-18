---
title: Job
description: CRD reference for the Job resource.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


`Job` creates and monitors the actual workload (a `TrainJob` or other adapter-supported resource). It is created by the `Workflow` controller and is not typically created directly by users.

## Spec fields

| Field | Type | Description |
|-------|------|-------------|
| `workload` | WorkloadSpec | Required, immutable. Discriminated union selecting the workload framework; exactly one field must be set |
| `workloadMetadata` | WorkloadMetadata | Optional, immutable. Labels applied to the generated workload object itself (see below) |
| `workloadMetadata.labels` | map[string]string | Optional. At most 32 entries. Label keys must be valid Kubernetes label keys and values valid Kubernetes label values; `app.kubernetes.io/managed-by` and any key under `nvcre.nvidia.com/` are rejected |

### Workload object labels

`spec.workloadMetadata.labels` is the canonical place NVCRE records labels for the **workload object** it creates from `spec.workload` — today a Kubeflow `TrainJob`. `WorkloadRun` and `Certification` both resolve their own `workloadMetadata` into this field, and a hand-authored `Workflow` writes `spec.jobTemplate.spec.workloadMetadata` directly.

<Warning>
These labels are executable policy, not decoration. They are how a workload reaches a Kueue local queue, a KAI Scheduler queue, or any other integration keyed on the submitted object's labels — all of which are read **before** workload pods exist. A label here can cause an external system to suspend, mutate, reject, prioritize, or bill the workload. Treat the field as part of the submission contract.
</Warning>

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Job
metadata:
  name: my-job
spec:
  workloadMetadata:
    labels:
      kueue.x-k8s.io/queue-name: gpu-team-a   # Kueue local queue
      kai.scheduler/queue: burn-in            # KAI Scheduler queue
      environment: burn-in                    # arbitrary label
  workload:
    trainJob:
      runtimeRef:
        kind: TrainingRuntime
        name: nccl-all-reduce-runtime
```

The resulting `TrainJob` carries those three labels plus the two the controller owns, `app.kubernetes.io/managed-by: nvcre` and `nvcre.nvidia.com/job: my-job`.

**This is not pod-label injection.** `workloadMetadata.labels` label the workload object and nothing below it: they are not copied to `JobSet`s, pod templates, or pods. Conversely, ordinary Job labels — `Workflow` `spec.jobTemplate.metadata.labels` — label the NVCRE `Job` only and are never copied here. Neither direction is inferred, because propagating labels automatically could opt a workload into an admission controller or a queue that nobody asked for. If you need a label at the pod level, set it in the workload spec's own pod templates.

**Reserved keys.** `app.kubernetes.io/managed-by` and any key under the `nvcre.nvidia.com/` prefix identify controller ownership and Job association. Setting either is rejected at admission rather than silently overwritten.

**Limits.** At most 32 labels. Keys follow the Kubernetes qualified-name grammar: an optional DNS-subdomain prefix of at most 253 characters, a `/`, then a name of at most 63 characters. Values follow Kubernetes label-value syntax and may be empty — an empty value is a real label, not a deletion. There is no deletion syntax.

**Immutability.** `workloadMetadata` cannot be added, removed, or changed after the Job is created; all three are rejected by CRD transition rules. A checkpoint restart therefore recreates the workload with the labels it was originally admitted with, so a restart cannot land the workload in a different queue than the one that admitted it. To change the labels, create a new Job (or a new `WorkloadRun`/`Certification`).

## Status fields

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | []Condition | Exclusive set: `InProgress`, `Succeeded`, `Failed`. Independent (additive): `HardwareFailed` (can be True alongside execution state), `ValidationFailed` (can be True alongside `Succeeded`) |
| `workloadRef` | WorkloadReference | Reference to the created workload (`TrainJob`) |
| `workloadStartTime` | Time | When the workload was first observed running rather than pending (e.g. suspended by Kueue). `timeoutPerJob` is measured from this timestamp, and the stall clock never starts before it, so queued time counts against neither. Cleared on checkpoint restart |
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
