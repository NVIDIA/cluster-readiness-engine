---
title: Quick Start
description: Certify a GPU cluster end-to-end in minutes.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


This guide walks through a full cluster certification: install ncrectl, run the certification suite, and review the results.

## Before you begin

[Install ncrectl and set up the cluster](./install.md) before continuing.

You will also need an NGC API key to pull the certification workload images.

## Define a Certification

Create a `certification.yaml` targeting your GPU nodes:

```yaml
apiVersion: cre.nvidia.com/v1alpha1
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
      variant: nemotron4-15b
```

## Run the certification

Use `ncrectl certification run` to handle the full lifecycle — apply the manifest, wait for completion, print the report, and clean up:

```bash
ncrectl certification run \
  --cert-file certification.yaml \
  --image-pull-secret ngc-secret \
  --wait
```

## Review the results

When the certification completes, ncrectl prints a pass/fail summary per node group and category. A full report is available with:

```bash
ncrectl certification report gpu-cluster-cert
```

Failed categories indicate nodes that did not meet performance thresholds. The controller automatically taints and cordons those nodes so production workloads avoid them.

## Next steps

- [Concepts: Architecture](../concepts/architecture.md) — understand the Certification → Workflow → Job hierarchy
- [How-to: Certify a Cluster](../how-to-guides/certify-a-cluster.md) — per-cloud-platform guides (AWS, GCP, Azure)
- [How-to: Interpret Results](../how-to-guides/interpret-results.md) — reading the report in detail
