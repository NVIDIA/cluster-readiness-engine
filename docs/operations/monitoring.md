---
title: Monitoring
description: Prometheus metrics, structured logging, and alerting for the NVIDIA Cluster Readiness Engine controller.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


Set up Prometheus metrics, structured log queries, and alert rules for the NVIDIA Cluster Readiness Engine (NVCRE) controller.

## Prometheus integration

### ServiceMonitor

The Helm chart installs a `ServiceMonitor` for the Prometheus Operator by default (`metrics.serviceMonitor.enabled: true`; set it to `false` on clusters without the Prometheus Operator CRDs). The shipped monitor scrapes the controller's HTTPS metrics endpoint:

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

Verify it is installed:

```bash
kubectl get servicemonitor -n nvcre
```

### Key metrics

The table below highlights the most important metrics. See the [Metrics Reference](./metrics.md) for the full list, labels, and example PromQL queries.

| Metric | Type | What it tells you |
|--------|------|-------------------|
| `cre_job_status` | Gauge | Current state of each job (in_progress, succeeded, failed) |
| `cre_job_failed_nodes` | Gauge | Number of nodes with hardware failures per job |
| `cre_hardware_failures_detected_total` | Counter | Cumulative hardware failure detections per node |
| `cre_reconcile_duration_seconds` | Histogram | How long each reconcile loop takes |
| `cre_reconcile_total` | Counter | Reconcile attempts by result (success, error, requeue) |
| `cre_goodput_ratio` | Gauge | Training efficiency from 0.0 to 1.0 |
| `cre_nccl_algbw_gbps` | Gauge | NCCL algorithmic bandwidth in GB/s per message size |
| `cre_nccl_busbw_gbps` | Gauge | NCCL bus bandwidth in GB/s per message size |
| `cre_topology_validated_nodes` | Gauge | Nodes that passed validation per topology domain |

## Structured logging

The controller emits structured zap logs. In the shipped manager configuration, zap development mode is enabled, so logs use console encoding unless you set `--zap-encoder=json`.

### Key fields

| Field | Description |
|-------|-------------|
| `controller` | Which controller emitted the log (job, workflow, certification, goodputmeasurement) |
| `namespace` | Kubernetes namespace of the resource |
| `name` | Resource name |
| `reconcileID` | Unique ID for the reconcile pass — use this to trace a single reconciliation |

### Log levels

| Level | Flag | Use case |
|-------|------|----------|
| info (0) | `--zap-log-level=0` | Normal operations, status changes |
| debug (1) | `--zap-log-level=1` | Detailed troubleshooting |

Enable debug logging by adding the flag to the manager container args in the Deployment spec:

```yaml
args:
  - --zap-log-level=1
```

### Example log queries

Using Loki or a similar log aggregation system (adjust the stream selector to how your agent labels the controller pods — the pods carry the `control-plane=manager` label in the `nvcre` namespace):

```
# Hardware failures
{namespace="nvcre"} |= "Hardware failure detected"

# Errors for a specific job
{namespace="nvcre"} |= "my-job" |= "error"

# All hardware failure detections
{namespace="nvcre"} |= "HardwareFailed"
```

If you switch the manager to JSON logs with `--zap-encoder=json`, you can filter on fields like `controller`, `name`, and `reconcileID` directly in your log backend.

## Recommended alerts

Production GPU fleets require alerting across three dimensions: **hardware failures** (GPU faults, NVLink errors, ECC violations detected during workloads), **performance regressions** (bandwidth or goodput falling below expected thresholds), and **operational health** (controller reconciliation latency, error rates). The starter rules below cover all three.

```yaml
groups:
  - name: nvcre
    rules:
      - alert: NVCREHardwareFailure
        expr: cre_job_failed_nodes > 0
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "Hardware failure in job {{ $labels.job }}"
          description: "{{ $value }} node(s) failed in {{ $labels.namespace }}/{{ $labels.job }}."

      - alert: NVCREJobStuck
        expr: cre_job_status{status="in_progress"} == 1
        for: 6h
        labels:
          severity: warning
        annotations:
          summary: "Job {{ $labels.job }} stuck in progress"
          description: "Job has been in_progress for over 6 hours."

      - alert: NVCRELowGoodput
        expr: cre_goodput_ratio > 0 and cre_goodput_ratio < 0.5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Low goodput for {{ $labels.measurement }}"
          description: "Goodput ratio is {{ $value | humanizePercentage }}."

      - alert: NVCREHighReconcileLatency
        expr: |
          histogram_quantile(0.95,
            sum by (le) (rate(cre_reconcile_duration_seconds_bucket[5m]))
          ) > 5
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Reconciliation P95 latency above 5s"

      - alert: NVCREReconcileErrors
        expr: |
          sum(rate(cre_reconcile_total{result="error"}[5m]))
          / sum(rate(cre_reconcile_total[5m])) > 0.1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Reconciliation error rate above 10%"
```

**Tune alert thresholds:** the `for` durations and thresholds above are starting points. Adjust them based on your workload profiles — long-running multi-day training jobs will need a longer `NVCREJobStuck` threshold.

## Next steps

- [Metrics Reference](./metrics.md) — Full list of metrics with labels and PromQL examples.
- [Troubleshooting](./troubleshooting.md) — Diagnose issues using logs and metrics.
- [Deployment](./deployment.md) — Resource sizing and health-check configuration.
