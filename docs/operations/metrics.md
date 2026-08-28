---
title: Prometheus Metrics
description: Complete reference for all Prometheus metrics exposed by the NVIDIA Cluster Readiness Engine controller.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This page is the full reference for Prometheus metrics exposed by the NVIDIA Cluster Readiness Engine (NVCRE) controller. All metrics are registered with the controller-runtime metrics registry and served on the `/metrics` endpoint.

## Scrape configuration

The Helm chart ships a `ServiceMonitor` for the Prometheus Operator (enabled by default via `metrics.serviceMonitor.enabled`):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: nvcre-metrics-monitor
  namespace: nvcre
spec:
  endpoints:
    - path: /metrics
      port: https
      scheme: https
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tlsConfig:
        insecureSkipVerify: true  # Use cert-manager in production
  selector:
    matchLabels:
      control-plane: manager
```

## Job status metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cre_job_status` | Gauge | `namespace`, `job`, `workflow`, `status` | Current status of burn-in Jobs. Value is `1` for the current status, `0` for others. Status values: `in_progress`, `succeeded`, `failed`. |

The gauge is set for all three status values on each update, ensuring that a transition from `in_progress` to `succeeded` also zeroes out the `in_progress` series. Metrics are cleaned up when a Job is deleted.

## Hardware failure metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cre_hardware_failed_jobs_total` | Counter | `namespace`, `job`, `workflow` | Total number of Jobs that detected hardware failures. Incremented once per Job on first detection. |
| `cre_job_failed_nodes` | Gauge | `namespace`, `job`, `workflow` | Number of nodes with detected hardware failures per Job. |
| `cre_hardware_failures_detected_total` | Counter | `namespace`, `job`, `workflow`, `node` | Total number of hardware failures detected across all evaluations. Per-node granularity. |

## Node health check metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cre_node_health_check_duration_seconds` | Histogram | `namespace`, `job`, `workflow` | Duration of node health check operations in seconds. Buckets: 1ms to ~16s (exponential). |
| `cre_nodes_evaluated_total` | Counter | `namespace`, `job`, `workflow` | Total number of node health evaluations performed. |

## Reconciliation metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cre_reconcile_total` | Counter | `namespace`, `job`, `workflow`, `result` | Total number of reconciliation attempts. `result` values: `success`, `error`, `requeue`. |
| `cre_reconcile_duration_seconds` | Histogram | `namespace`, `job`, `workflow` | Duration of reconciliation operations in seconds. Buckets: 10ms to ~20s (exponential). |

## Workload metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cre_workload_created_total` | Counter | `namespace`, `job`, `workflow` | Total number of workloads created by the Job controller. |

## Goodput metrics

All goodput metrics share the same label set: `namespace`, `measurement`, `job`, `workflow`.

| Metric | Type | Description |
|--------|------|-------------|
| `cre_goodput_ratio` | Gauge | Runtime goodput ratio (0.0 to 1.0) |
| `cre_goodput_avg_tflops_per_gpu` | Gauge | Average TFLOPS per GPU from goodput measurement |
| `cre_goodput_training_time_seconds` | Gauge | Total training wall-clock time in seconds |
| `cre_goodput_reschedule_time_seconds` | Gauge | Cumulative reschedule time in seconds |
| `cre_goodput_resume_time_seconds` | Gauge | Cumulative resume time in seconds |
| `cre_goodput_checkpoint_save_time_seconds` | Gauge | Cumulative checkpoint save time in seconds |
| `cre_goodput_warmup_time_seconds` | Gauge | Total warmup step time in seconds |
| `cre_goodput_non_warmup_time_seconds` | Gauge | Total non-warmup step time in seconds |
| `cre_goodput_avg_step_time_seconds` | Gauge | Average time per training step in seconds |
| `cre_goodput_lost_work_time_seconds` | Gauge | Cumulative lost work time in seconds (work done after last checkpoint, lost on restart) |

Goodput metrics are cleaned up when a GoodputMeasurement is deleted.

### Metric lifecycle

Goodput metrics are cleaned up at specific lifecycle events to prevent stale data in Prometheus:

| Event | What is cleaned up | What is preserved |
|-------|--------------------|-------------------|
| Job completes (succeeded or failed) | Operational metrics (every `cre_goodput_*` gauge except the ratio: TFLOPS, step time, training/warmup/non-warmup time, checkpoint, reschedule, resume, lost work) | `cre_goodput_ratio` (the outcome) |
| Job restarts from checkpoint | Instantaneous metrics (TFLOPS, avg step time) | All cumulative metrics |
| GoodputMeasurement deleted | All goodput metrics for that measurement | — |
| BandwidthMeasurement completes or is deleted | All NCCL bandwidth metrics for that measurement | — |

## NCCL bandwidth metrics

All NCCL bandwidth metrics share the same label set: `namespace`, `measurement`, `job`, `workflow`, `nccl_test`, `message_size_bytes`.

| Metric | Type | Description |
|--------|------|-------------|
| `cre_nccl_algbw_gbps` | Gauge | NCCL algorithmic bandwidth in GB/s per message size |
| `cre_nccl_busbw_gbps` | Gauge | NCCL bus bandwidth in GB/s per message size |

The `nccl_test` label identifies the collective operation (e.g., `all_reduce`, `all_gather`, `alltoall`). The `message_size_bytes` label tracks results per message size tested.

NCCL bandwidth metrics are cleaned up when a BandwidthMeasurement is deleted.

**Cardinality at scale:** NCCL metrics include a `message_size_bytes` label (typically 20-30 values per test). With 3 test types and 10 concurrent measurements, expect ~600-900 NCCL time series. Goodput metrics produce 10 series per measurement. At 100+ concurrent Jobs, monitor your Prometheus memory and consider increasing `sampleInterval` or limiting concurrent Certifications.

## Topology metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cre_topology_validated_nodes` | Gauge | `namespace`, `workflow`, `topology_key`, `domain`, `node` | Set to `1` for each node that passed burn-in validation. Per-node granularity for identifying exactly which nodes are certified in each topology domain. |
| `cre_topology_failed_nodes` | Gauge | `namespace`, `workflow`, `topology_key`, `domain`, `node` | Set to `1` for each node that failed burn-in validation. Useful for identifying bad switches, racks, or NVLink cliques from Prometheus. |

## Example PromQL queries

### Job status

```promql
# Count of failed jobs
count(cre_job_status{status="failed"} == 1) by (namespace)

# All currently running jobs
cre_job_status{status="in_progress"} == 1

# Job status breakdown per namespace
sum by (namespace, status) (cre_job_status == 1)
```

### Hardware failures

```promql
# Nodes with hardware failures across all jobs
count(cre_hardware_failures_detected_total) by (node)

# Hardware failure rate (per hour)
sum(rate(cre_hardware_failures_detected_total[1h])) by (namespace)

# Jobs with the most failed nodes
topk(10, cre_job_failed_nodes)
```

### Goodput

```promql
# Average goodput across all measurements in a namespace
avg(cre_goodput_ratio) by (namespace)

# Jobs with low goodput (below 80%)
cre_goodput_ratio < 0.8

# Training time breakdown for a specific job
{__name__=~"cre_goodput_(training|reschedule|resume|checkpoint_save|warmup|non_warmup)_time_seconds", job="my-training-job"}

# Average TFLOPS per GPU across measurements
avg(cre_goodput_avg_tflops_per_gpu) by (namespace)
```

### NCCL bandwidth

```promql
# Bus bandwidth for all_reduce across all message sizes
cre_nccl_busbw_gbps{nccl_test="all_reduce"}

# Average algorithmic bandwidth per test type
avg(cre_nccl_algbw_gbps) by (nccl_test)

# Compare bandwidth across message sizes for a specific measurement
cre_nccl_algbw_gbps{measurement="nccl-allreduce-bw"}
```

### Reconciliation performance

```promql
# Reconciliation error rate
sum(rate(cre_reconcile_total{result="error"}[5m])) by (namespace)

# 99th percentile reconciliation duration
histogram_quantile(0.99, sum(rate(cre_reconcile_duration_seconds_bucket[5m])) by (le))

# Node health check duration (p95)
histogram_quantile(0.95, sum(rate(cre_node_health_check_duration_seconds_bucket[5m])) by (le))
```

### Topology validation

```promql
# Nodes validated per topology domain
cre_topology_validated_nodes{topology_key="nvidia.com/gpu.clique"}

# Total validated nodes per workflow
sum(cre_topology_validated_nodes) by (namespace, workflow)
```

## Example alerting rules

The following `PrometheusRule` examples provide starting points for monitoring a burn-in cluster.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: nvcre-alerts
  labels:
    app.kubernetes.io/name: nvcre
spec:
  groups:
    - name: burnin.rules
      rules:
        # Alert when hardware failure rate exceeds threshold
        - alert: NVCREHighHardwareFailureRate
          expr: |
            sum(rate(cre_hardware_failures_detected_total[1h])) by (namespace) > 0.5
          for: 10m
          labels:
            severity: critical
          annotations:
            summary: "High hardware failure rate in namespace {{ $labels.namespace }}"
            description: >
              More than 0.5 hardware failures per second detected in
              namespace {{ $labels.namespace }} over the last hour.

        # Alert when goodput drops below acceptable threshold
        - alert: NVCRELowGoodput
          expr: |
            cre_goodput_ratio < 0.75
            and cre_goodput_ratio > 0
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: "Low goodput for measurement {{ $labels.measurement }}"
            description: >
              Goodput ratio for {{ $labels.measurement }} in
              {{ $labels.namespace }} is {{ $value | humanize }},
              below the 75% threshold. Check for frequent checkpointing
              or rescheduling overhead.

        # Alert when a job appears stuck (in_progress for too long)
        - alert: NVCREJobStuck
          expr: |
            cre_job_status{status="in_progress"} == 1
            unless on(namespace, job) (
              increase(cre_reconcile_total[30m]) > 0
            )
          for: 30m
          labels:
            severity: warning
          annotations:
            summary: "NVCRE job {{ $labels.job }} appears stuck"
            description: >
              Job {{ $labels.job }} in namespace {{ $labels.namespace }}
              has been in_progress for at least 30 minutes with no
              reconciliation activity.
```

## See also

- [Job API](../api-reference/job.md) — the resource that emits job status and hardware failure metrics
- [GoodputMeasurement API](../api-reference/goodput-measurement.md) and [BandwidthMeasurement API](../api-reference/bandwidth-measurement.md) — the resources that populate goodput and bandwidth metrics
- [Workflow API](../api-reference/workflow.md) — the resource that populates topology validation metrics
- [Goodput & Bandwidth Measurement](../concepts/goodput-bandwidth.md) — conceptual overview of the goodput formula and its components
