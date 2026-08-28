---
title: Introduction
description: NVIDIA Cluster Readiness Engine — GPU cluster certification, benchmarking, and hardware failure detection for Kubernetes.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


The NVIDIA Cluster Readiness Engine is a Kubernetes controller for GPU cluster burn-in certification, orchestrated benchmarking, and hardware failure detection. Run real distributed workloads across topology-aware node groups, measure training throughput and interconnect bandwidth, detect hardware failures, and record which nodes failed and why — before production workloads touch the cluster.

## What it does

- **Certifies GPU clusters** by running domain-specific workloads (NCCL benchmarks, distributed training) against all node groups and validating results against per-architecture thresholds
- **Detects hardware failures** via CEL-based node health monitors that evaluate expressions against Node objects (e.g. checking for unhealthy GPU taints or conditions), running concurrently with workloads
- **Isolates faulty nodes** automatically using bisection-based adaptive fault isolation
- **Identifies failed nodes** per category with a machine-readable reason (`HardwareFailureDetected`, `ThresholdViolation`, `WorkloadFailed`) for external remediation pipelines to act on
- **Measures goodput and bandwidth** by parsing pod logs in real time against LogProfile patterns

## Key components

| Component | Purpose |
|-----------|---------|
| `Certification` | Top-level resource; runs a suite of certification categories against a node pool |
| `Workflow` | Manages one category run; applies catalog, overrides, and orchestration |
| `Job` | Creates and monitors the actual workload; drives health monitoring and measurement |
| `WorkloadRun` | Simplified API for ad-hoc workloads without full certification overhead |
| `nvcrectl` | CLI for setup, certification lifecycle, rendering, and reporting |

## Next steps

- [Install](./getting-started/install.md) — set up the controller and CLI
- [Quick Start](./getting-started/quick-start.md) — certify a cluster in minutes
- [Architecture](./concepts/architecture.md) — understand how the components fit together
