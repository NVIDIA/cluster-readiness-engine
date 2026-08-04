---
title: Health Monitoring & Remediation
description: How the Cluster Readiness Engine detects node failures and quarantines bad nodes.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## Health monitoring

The `Job` controller runs a pluggable `NodeFailureDetector` alongside each workload. Detectors evaluate node state continuously during the run and report failures before the workload exits.

### CEL detector

The built-in detector evaluates [Common Expression Language (CEL)](https://cel.dev) expressions against Kubernetes `Node` objects. Conditions, labels, taints, and allocatable resources are all accessible as CEL fields. Multiple expressions can be combined with `&&` / `||`.

Example expression that fires when a node's GPU ECC error count exceeds a threshold:

```
node.metadata.labels["nvidia.com/gpu.ecc.error.count"] > "0"
```

Custom detectors can be registered by implementing the `NodeFailureDetector` interface.

## Remediation

When a `Certification` fails, the controller auto-creates a `Remediation` resource targeting the affected nodes. The `Remediation` controller:

1. **Taints** the nodes with `excalibur.nvidia.com/preflight-failed:NoExecute` — evicts existing workloads and prevents new scheduling
2. **Cordons** the nodes — marks them unschedulable
3. **Sets conditions** on the node objects documenting the failure reason

### Reversal

Deleting the `Remediation` resource reverses all of the above: taints and cordons are removed, and nodes return to schedulable state.

### Manual remediation

You can also create a `Remediation` resource manually to quarantine nodes outside of a certification run.

## Adaptive fault isolation

When a run fails and multiple nodes are suspect, the controller can bisect the node pool to isolate the faulty node(s) without re-running the full suite. See [How-to: Adaptive Fault Isolation](../how-to-guides/adaptive-fault-isolation.md).
