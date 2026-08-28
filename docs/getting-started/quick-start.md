---
title: Quick Start
description: Certify a GPU cluster end-to-end in minutes.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This guide walks through a full cluster certification: install nvcrectl, run the certification suite, and review the results.

**Certification vs WorkloadRun:** Use `nvcrectl certification run` to run a full burn-in suite — it tests multiple categories (NCCL benchmarks, training workloads) against all node groups and records pass/fail results per node. Use `nvcrectl workloadrun run` when you want to run a single workload ad hoc, such as a quick bandwidth check or a one-off training smoke test, without setting up a full certification. See [WorkloadRun Quick Start](./workloadrun-quick-start.md).

## Before you begin

[Install nvcrectl and set up the cluster](./install.md) before continuing.

You will also need an NGC API key to pull the certification workload images.

## Define a Certification

Create a `certification.yaml` targeting your GPU nodes:

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  imagePullSecrets:
    - name: ngc-secret
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
```

## Run the certification

Use `nvcrectl certification run` to handle the full lifecycle — apply the manifest, wait for completion, print the report, and clean up:

```bash
nvcrectl certification run \
  --cert-file certification.yaml \
  --wait
```

## Review the results

When the certification completes, nvcrectl prints a pass/fail summary per node group and category. A full report is available with:

```bash
nvcrectl certification report gpu-cluster-cert
```

Failed categories indicate nodes that did not meet performance thresholds. NVCRE records which nodes failed and why — it does not taint or cordon them. Use `kubectl cordon <node>` to quarantine nodes as needed.

## Next steps

- [Concepts: Architecture](../concepts/architecture.md) — understand the Certification → Workflow → Job hierarchy
- [How-to: Certify a Cluster](../how-to-guides/certify-a-cluster.md) — per-cloud-platform guides (AWS, GCP, Azure)
- [How-to: Interpret Results](../how-to-guides/interpret-results.md) — reading the report in detail
