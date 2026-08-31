---
title: Health Monitoring & Failed Node Attribution
description: How the NVIDIA Cluster Readiness Engine detects node failures and records which nodes failed and why.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Health monitoring

The `Job` controller runs a pluggable `NodeFailureDetector` alongside each workload. Detectors evaluate node state continuously during the run and report failures before the workload exits.

### CEL detector

The built-in detector evaluates [Common Expression Language (CEL)](https://cel.dev) expressions against Kubernetes `Node` objects. Conditions, labels, taints, and allocatable resources are all accessible as CEL fields. Multiple expressions can be combined with `&&` / `||`.

Example expression that fires when a node's GPU ECC error count exceeds a threshold:

```
node.metadata.labels["nvidia.com/gpu.ecc.error.count"] > "0"
```

Custom detectors can be registered by implementing the `NodeFailureDetector` interface.

## Failed node attribution

NVCRE does not taint, cordon, or patch nodes. Its job is to identify which nodes failed which tests and why. That signal is recorded on the Certification CR and is available for external systems (node lifecycle operators, alerting pipelines) to act on.

### How failures propagate

Failed nodes bubble up through the three-tier hierarchy:

```
Job            status.failedNodes[]            ← inline list; set when the Job fails, one entry per node with a reason
      ↑ persisted to
Workflow       status.failedNodesRef           ← ConfigMap reference; union across all failed Jobs for this category
      ↑ copied to
Certification  status.categoryStatuses[].failedNodesRef  ← per-category ConfigMap reference
```

Failed nodes are stored in ConfigMaps (not inline on the status) at the Workflow and Certification tiers. To read them:

```bash
# Certification tier
kubectl get certification <name> -o jsonpath='{.status.categoryStatuses[0].failedNodesRef.name}'
kubectl get configmap <ref-name> -o yaml

# Workflow tier
kubectl get workflow <name> -o jsonpath='{.status.failedNodesRef.name}'
kubectl get configmap <ref-name> -o yaml
```

### Failure reasons

Each failed node entry carries a `reason`:

| Reason | Meaning |
|--------|---------|
| `HardwareFailureDetected` | CEL health check detected an unhealthy node **mid-run** |
| `ThresholdViolation` | A performance threshold (bandwidth, goodput, step time) was missed |
| `WorkloadFailed` | The workload exited non-zero or stalled |

<Note>
Cordoned nodes and nodes reporting fewer allocatable GPUs than the workload requests per node are filtered **before** a Job runs, not attributed as `HardwareFailureDetected`. They appear in `status.orchestration.excludedNodes` with the reason in `exclusionReason` and cause the run to be marked `INCOMPLETE` rather than `Failed`. If **no** matching node can supply the requested GPU count, the run fails immediately with a message naming the requirement and the best available count instead of scheduling pods that would stay `Pending` forever.
</Note>

Different nodes in the same category can fail with different reasons. A node that fails in multiple categories appears in each category's failed-nodes ConfigMap, potentially with a different reason each time.

### Reading failed nodes

Use the CLI for a structured report:

```bash
nvcrectl certification report <name>
```

Or read the raw ConfigMap directly:

```bash
kubectl get certification <name> -o jsonpath='{.status.categoryStatuses[0].failedNodesRef.name}'
kubectl get configmap <ref-name> -o yaml
```

### Acting on failed nodes

NVCRE reports failures — quarantine is your platform's responsibility. Common patterns:

- Cordon the node (`kubectl cordon <node>`) to prevent new workloads from scheduling
- Drain it (`kubectl drain <node>`) to evict existing workloads
- Trigger a node lifecycle operator or repair pipeline using the `failedNodesRef` ConfigMap data as input
- After repair, uncordon the node and re-run the relevant category to confirm it passes

## Adaptive fault isolation

When a run fails and multiple nodes are suspect, the controller can bisect the node pool to isolate the faulty node(s) without re-running the full suite. See [How-to: Adaptive Fault Isolation](../how-to-guides/adaptive-fault-isolation.md).
