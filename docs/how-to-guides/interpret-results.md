---
title: Interpret Results
description: Read and act on certification and WorkloadRun reports.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


## Get a report

```bash
xcalctl certification report <name>
xcalctl workloadrun report <name>
```

## Report structure

| Section | Contents |
|---------|---------|
| Summary | Overall pass/fail, node count, run duration |
| Category results | Per-domain/variant measured vs. expected values with pass/fail |
| Node results | Per-node breakdown — which passed, which failed, which were remediated |

## Status values

| Status | Meaning |
|--------|---------|
| `Passed` | All categories met thresholds on all node groups |
| `Failed` | One or more categories failed; affected nodes tainted and cordoned |
| `InProgress` | Still running |

## Bandwidth results

- **Measured bus bandwidth** (GB/s) per collective
- **Expected threshold** for the detected GPU architecture
- **Pass/fail** per operation

Below-threshold results indicate a network issue — degraded link, misconfigured EFA/RoCE, or faulty NIC.

## Goodput results

- **Goodput ratio** (0.0–1.0) — fraction of time the job was making useful forward progress
- **Expected minimum** from the catalog entry
- A ratio below ~0.95 suggests stalls, slow nodes, or framework overhead

## Remediated nodes

```bash
kubectl get remediations.excalibur.nvidia.com
kubectl describe remediation <name>
```

To release nodes after fixing the underlying issue:

```bash
kubectl delete remediation <name>
```
