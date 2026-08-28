---
title: Troubleshooting
description: Diagnose and resolve common issues — stuck jobs, hardware detection failures, and stalls.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This page is for operators who need to diagnose problems with the NVIDIA Cluster Readiness Engine (NVCRE). Each section follows a problem-solution format: symptoms, diagnostic commands, and fixes.

## Job not progressing

**Symptoms:** Job stays in `InProgress` for longer than expected. No `Succeeded` or `Failed` condition appears.

**Diagnosis:**

```bash
# Check the Job conditions
kubectl describe jobs.nvcre.nvidia.com <name> -n <namespace>

# Check the underlying workload (use the kind from status.workloadRef)
kubectl get trainjob -n <namespace>
kubectl get pods -l nvcre.nvidia.com/job=<name> -n <namespace> -o wide

# Check controller logs for this job
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep <name>
```

**Solutions:**

- The workload resource exists but is not completing — check Kubeflow Trainer logs and pod events.
- Pods are `Pending` — verify GPU resources are available on target nodes (`kubectl describe node <node>`).
- Kubeflow Trainer is not running — confirm its pods are healthy (`kubectl get pods -n kubeflow-system`).

## Workflow stuck without a Job

**Symptoms:** Workflow condition is `InProgress`, but no child Job appears.

**Diagnosis:**

```bash
# Check Workflow status and conditions
kubectl get workflows.nvcre.nvidia.com <name> -o yaml

# Look for the child Job
kubectl get jobs.nvcre.nvidia.com -l nvcre.nvidia.com/workflow=<name>

# Check if dependencies were created
kubectl get workflows.nvcre.nvidia.com <name> \
  -o jsonpath='{.status.dependencyRefs}' | jq .
```

**Solutions:**

- Dependency creation failed — check controller logs for RBAC errors when creating ConfigMaps, PVCs, or other dependency resources.
- Target nodes do not match — verify the Workflow's `nodeSelector` or `nodeNames` matches existing nodes (`kubectl get nodes -l <selector>`).
- The Workflow spec is invalid — look for validation errors in the controller logs.

## Hardware failures not detected

**Symptoms:** A node has a known hardware problem, but the Job does not report the `HardwareFailed` condition.

**Diagnosis:**

```bash
# Verify pods are running on the expected nodes
kubectl get pods -l nvcre.nvidia.com/job=<name> -o wide

# Check that the pod label was injected
kubectl get pods -l nvcre.nvidia.com/job=<name> --show-labels

# Inspect the node for the expected conditions/taints
kubectl get node <node> -o yaml

# Check controller logs for CEL evaluation errors
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep "CEL"
```

**Solutions:**

- The CEL expression has a syntax error — look for parse errors in controller logs. Test your expression against a node object.
- Pods are not scheduled on target nodes — the controller only evaluates nodes where Job pods are running. Verify pod placement.
- The pod label is missing — the controller injects `nvcre.nvidia.com/job` automatically. If pods were created before the controller started, they may lack the label.
- Node conditions or taints do not exist yet — NVCRE only reads node state; it relies on your cluster's health monitoring stack to set the conditions or taints your CEL expression checks. Verify with `kubectl describe node`.

## High reconciliation latency

**Symptoms:** Status updates are slow. The `nvcre_reconcile_duration_seconds` P95 is above 5 seconds.

**Diagnosis:**

```bash
# Check controller resource usage
kubectl top pod -n nvcre

# Check for API server throttling (429 responses)
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep "throttling"
```

**Solutions:**

- Controller is resource-constrained — increase CPU and memory limits. See the [Deployment](./deployment.md) page for sizing guidance.
- Too many nodes per health check — large clusters increase CEL evaluation time. Consider splitting workloads across fewer nodes.
- API server is under load — check API server metrics and reduce concurrent reconciles if needed.

## Certification not progressing

**Symptoms:** Certification stays `InProgress` after Workflows have finished.

**Diagnosis:**

```bash
# Check category statuses
kubectl get certifications.nvcre.nvidia.com <name> \
  -o jsonpath='{.status.categoryStatuses}' | jq .

# List child Workflows
kubectl get workflows.nvcre.nvidia.com -l nvcre.nvidia.com/certification=<name>

# Check for CategoryNotFound errors
kubectl logs -n nvcre deploy/nvcre-manager \
  | grep "CategoryNotFound"
```

**Solutions:**

- A category is not registered in the catalog — the controller sets the Certification to `Failed` with reason `CategoryNotFound`. Verify your domain and variant values against `nvcrectl certification list-categories`.
- A child Workflow is still running — the Certification waits for all Workflows to complete before transitioning.

## Stall detection not triggering

**Symptoms:** A Job appears stuck but is not marked `Failed` with reason `WorkloadStalled`.

**Diagnosis:**

```bash
# Verify stallMultiplier is set on the Job
kubectl get jobs.nvcre.nvidia.com <name> -o jsonpath='{.spec.stallMultiplier}'

# Check if a GoodputMeasurement exists and has step data
kubectl get goodputmeasurement -l nvcre.nvidia.com/job=<name> -o yaml

# Look for avgStepTimeSec and lastStepTimestamp
kubectl get goodputmeasurement -l nvcre.nvidia.com/job=<name> \
  -o jsonpath='{.items[0].status.avgStepTimeSec}'
```

**Solutions:**

- `stallMultiplier` is not set — stall detection is opt-in. Add `stallMultiplier` to the Job spec (e.g., `10` means stalled if no step for 10x average step time).
- No GoodputMeasurement exists — stall detection requires a GoodputMeasurement to provide `avgStepTimeSec` and `lastStepTimestamp`. Configure `goodputMeasurement` on the Job.
- Not enough training steps — the controller needs at least two non-warmup steps to compute `avgStepTimeSec`. Wait for the workload to produce more log output.

## BandwidthMeasurement not reporting results

**Symptoms:** A BandwidthMeasurement exists but `status.results` is empty.

**Diagnosis:**

```bash
# Check the BandwidthMeasurement status
kubectl get bandwidthmeasurement <name> -o yaml

# Verify the LogProfile has a bandwidthResult pattern
kubectl get logprofile <profile-name> -o jsonpath='{.spec.patterns.bandwidthResult}'

# Check pod logs for NCCL output
kubectl logs <pod-name> | head -50
```

**Solutions:**

- The LogProfile is missing the `bandwidthResult` pattern — BandwidthMeasurement requires a LogProfile with a `bandwidthResult` regex. Add it to the LogProfile spec.
- The regex does not match the NCCL output format — verify the regex against actual log output. NCCL test output format varies between versions.
- The workload has not produced output yet — bandwidth results appear only after the NCCL test completes its message-size sweep.
- The `replicatedJobName` is wrong — for MPI workloads, set `workerStrategy.replicatedJobName: launcher` in the LogProfile since NCCL output goes to the launcher pod.

## Enable debug logging

For deeper investigation, enable debug-level logging by adding the flag to the manager container args:

```yaml
# In the Deployment spec
args:
  - --zap-log-level=1
```

This surfaces detailed reconciliation traces and CEL evaluation results.

## Next steps

- [Monitoring](./monitoring.md) — Set up alerts so you catch issues before they need manual diagnosis.
- [Deployment](./deployment.md) — Review resource sizing and RBAC.
- [Health Monitoring & Failed Node Attribution](../concepts/health-monitoring-remediation.md) — Understand how CEL-based hardware detection works.
- [Certify a Cluster](../how-to-guides/certify-a-cluster.md) — End-to-end certification walkthrough.
