---
title: Interpret Results
description: Read and act on certification and WorkloadRun reports.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Get a report

```bash
nvcrectl certification report <name>
nvcrectl workloadrun report <name>
```

## Report structure

| Section | Contents |
|---------|---------|
| Summary | Overall pass/fail, node count, run duration |
| Category results | Per-domain/variant measured vs. expected values with pass/fail |
| Failed groups | Group nodes, failure reason, and the failed workload pod's captured log excerpt when available |
| Node results | Per-node breakdown — which passed and which failed |

## Status values

| Status | Meaning |
|--------|---------|
| `Passed` | All categories met thresholds on all node groups |
| `Failed` | One or more categories failed; affected nodes listed in ConfigMaps referenced by `status.categoryStatuses[].failedNodesRef` |
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

## Failed nodes

Failed nodes are stored in a ConfigMap referenced by `status.categoryStatuses[].failedNodesRef` for each category. The Job tier records them inline on `status.failedNodes`; the Workflow and Certification tiers persist them to ConfigMaps.

```bash
# Get the ConfigMap name for the first category
kubectl get certification <name> -o jsonpath='{.status.categoryStatuses[0].failedNodesRef.name}'
# Read the ConfigMap contents
kubectl get configmap <ref-name> -o yaml
```

Or use the CLI for a formatted report:

```bash
nvcrectl certification report <name>
```

For execution failures, the failed-group section also shows the captured pod,
node, exit code, termination reason, and workload log tail. Machine-readable
reports include the same data under
`categories[].failedGroups[].failureLog`. The field is absent if the failed Job
was deleted before the report was generated.

The log represents one selected pod, not aggregated logs from every failed pod.
For execution failures, capture prefers a pod with an `OOMKilled` container.
When no nonzero exit code was captured, both the human report and JSON omit it
and retain the explanatory reason instead.

The human report shows the end of the excerpt: at most 4 KiB of input and 20
rendered log lines per failed group, with the line limit applied after wrapping.
Each wrapped log line is also bounded to 256 bytes of sanitized text, so
zero-width characters cannot produce arbitrarily long physical lines.
An explicit truncation notice points to the full captured excerpt in the JSON
report or `Job.status.failureLog` (up to 32 KiB). These limits do not shorten the
JSON tail.

Human output displays terminal controls and Unicode bidirectional controls as
visible escape sequences. CRLF and bare carriage returns become newlines in
human log excerpts. The JSON tail preserves the original captured text.

NVCRE records which nodes failed and why — it does not taint or cordon them. To quarantine a failed node:

```bash
kubectl cordon <node>    # prevent new workloads
kubectl drain <node>     # evict existing workloads
```

After repair, uncordon the node and re-run the relevant category to confirm it passes.
