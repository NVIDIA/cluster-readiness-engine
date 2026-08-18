---
title: Architecture
description: How the Cluster Readiness Engine controller is structured and how its resources compose.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


The Cluster Readiness Engine is a Kubebuilder-based Kubernetes controller. A single binary runs six reconcilers that compose in a three-tier hierarchy modeled after Kubernetes' own Deployment → ReplicaSet → Pod pattern.

## Resource hierarchy

```mermaid
graph TD
WR[WorkloadRun] -->|creates| W0[Workflow]
C[Certification] -->|creates| W1[Workflow: training-nemotron5-8b]
C -->|creates| W2[Workflow: communication-nccl]
W0 -->|creates| J0[Job]
W1 -->|creates| J1[Job]
W2 -->|creates| J2[Job]
J0 -->|creates| T0[TrainJob]
J1 -->|creates| T1[TrainJob]
J2 -->|creates| T2[TrainJob]
J0 -.->|monitors| N[Node Health]
J1 -.->|monitors| N
J2 -.->|monitors| N
GM[GoodputMeasurement] -.->|watches| J0
BM[BandwidthMeasurement] -.->|watches| J2
```

### Certification

The top-level resource. It references a set of `categories` (domain + variant pairs) from the catalog. The controller creates one `Workflow` per category and tracks overall pass/fail status. Failed nodes are recorded per category in ConfigMaps referenced by `status.categoryStatuses[].failedNodesRef`.

### Workflow

Manages a single test run for one category. It pulls the `WorkflowSpec` from the catalog, applies platform and GPU overrides, manages iteration count, and creates the child `Job`. Orchestration targets (which node group runs this workflow) are set at this tier.

### Job

Creates the actual workload (a `TrainJob`) via the adapter pattern. Manages health monitoring via `NodeFailureDetector`, optionally creates a `GoodputMeasurement` or `BandwidthMeasurement` to parse output, and handles checkpoint restart.

### Supporting resources

| Resource | Purpose |
|----------|---------|
| `GoodputMeasurement` | Watches a Job's pod logs via a LogProfile, computes the goodput ratio |
| `BandwidthMeasurement` | Parses NCCL log output, computes per-bus bandwidth metrics |
| `LogProfile` | Cluster-scoped; defines regex patterns with named capture groups for log parsing |

## Controller patterns

- **`setExclusiveCondition()`** — enforces mutually exclusive InProgress / Succeeded / Failed conditions at every tier
- **`Owns()` watches** — each tier watches its children for event-driven reconciliation; polling (15s in production, 1s in tests) is a safety net
- **Adapter pattern** — the `Job` controller normalizes `TrainJob` (Kubeflow Trainer v2) to a `WorkloadPhase` via a `WorkloadAdapter` interface (`pkg/workload/ForSpec()`). MPI and PyTorch are framework types at the `WorkloadRun` CLI layer — they generate a `TrainJob` under the hood, not a separate CRD field.

### Condition lifecycle

Each resource tier uses three mutually exclusive conditions:

```mermaid
stateDiagram-v2
[*] --> InProgress: Resource created
InProgress --> Succeeded: Workload completed
InProgress --> Failed: Workload failed
Succeeded --> [*]
Failed --> [*]
```

## Catalog

The catalog maps `{domain, variant}` pairs to `WorkflowSpec` builders. Each entry is a YAML file under `pkg/catalog/entries/<domain>/<variant>/` that registers itself at startup. See [Concepts: Catalog](./catalog.md).

## Platform and GPU detection

The controller auto-detects cloud platform from `spec.providerID` and GPU architecture from the `nvidia.com/gpu.product` node label. This drives which overrides apply. See [Concepts: Platform Detection & Overrides](./platform-detection.md).
