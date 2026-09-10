---
title: Custom Catalog Entries
description: Add a new domain/variant pair to the certification catalog.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


The catalog is extensible: add a YAML file to `pkg/catalog/entries/` in the source tree to register a custom certification category, then build and deploy new binaries.

## File layout

```
pkg/catalog/entries/
  <domain>/
    <variant>.yaml     ← new file
```

Catalog entries are embedded into the binaries at compile time via `//go:embed` (see `pkg/catalog/loader.go`). The loader discovers every entry in the embedded tree automatically, so no registration step or Go code change is required. Because the entries ship inside the binary, however, adding or changing an entry requires building and deploying a new image. Both `nvcrectl` and the controller embed the catalog, so both must be rebuilt to pick up the change. Entries cannot be dropped into a running system at runtime; see [Runtime alternatives](#runtime-alternatives) below for options that do not require a rebuild.

## YAML structure

A catalog entry YAML has up to four top-level sections:

```yaml
# Dependencies: Kubernetes resources applied before the job runs
# (e.g., a TrainingRuntime defining the MPI/PyTorch topology)
dependencies:
  - apiVersion: trainer.kubeflow.org/v1alpha1
    kind: TrainingRuntime
    metadata:
      name: my-variant-runtime
    spec:
      # ...

# Job template: defines the workload
jobTemplate:
  spec:
    workload:
      trainJob:
        runtimeRef:
          kind: TrainingRuntime
          name: my-variant-runtime
        trainer:
          image: nvcr.io/nvidia/pytorch:26.01-py3
          args:
            - my-benchmark-command
          numNodes: {{ .NodesPerJob }}
          numProcPerNode: {{ .GpusPerNode }}

# Orchestration: controls how jobs are grouped and scheduled
orchestration:
  execution:
    timeoutPerJob: 30m
  iterations: 1

# Overrides: platform- or GPU-specific patches (optional)
overrides:
  - when:
      platform:
        equals: aws
    jobTemplate:
      spec:
        workload:
          trainJob:
            trainer:
              image: public.ecr.aws/my-org/my-benchmark:latest
```

Template variables like `{{ .NodesPerJob }}` and `{{ .GpusPerNode }}` are resolved at render time from the detected cluster and any flag overrides.

## Minimal example

```yaml
# pkg/catalog/entries/my-domain/my-variant.yaml
dependencies:
  - apiVersion: trainer.kubeflow.org/v1alpha1
    kind: TrainingRuntime
    metadata:
      name: my-variant-runtime
    spec:
      mlPolicy:
        numNodes: 1
        torch:
          numProcPerNode: 1
      template:
        spec:
          replicatedJobs:
            - name: node
              template:
                spec:
                  template:
                    spec:
                      containers:
                        - name: node
                          image: nvcr.io/nvidia/pytorch:26.01-py3
                          resources:
                            limits:
                              nvidia.com/gpu: "{{ .GpusPerNode }}"

jobTemplate:
  spec:
    workload:
      trainJob:
        runtimeRef:
          kind: TrainingRuntime
          name: my-variant-runtime
        trainer:
          args:
            - my-command

orchestration:
  execution:
    timeoutPerJob: 30m
  iterations: 1
```

## Verify

After adding the file, rebuild `nvcrectl` so the new entry is embedded, then verify it appears in the catalog and renders correctly:

```bash
make build-nvcrectl

bin/nvcrectl certification list-categories

bin/nvcrectl certification render \
  --platform aws \
  /tmp/my-cert.yaml
```

Check the rendered Workflow for correct resource requests, env vars, and override annotations. To run the new category on a cluster, the controller must embed the entry too: `make build` produces the `bin/manager` binary and `make docker-build` builds the controller image to deploy.

## Runtime alternatives

A catalog entry is the most curated option, but it is compile-time only. If you need a custom test without rebuilding and redeploying, NVCRE has two runtime paths in addition to the catalog entry itself. From most curated to most flexible:

1. **Catalog entry** (this guide). Curated and certification-integrated: the category participates in `Certification` runs and appears in `nvcrectl certification list-categories`. Compile-time: changes require building and deploying a new image.
2. **`WorkloadRun`**. A runtime custom test with a supported UX. You choose a framework (`torch`, `mpi`, or `exec`), optionally mount config files via `spec.config`, and get automatic platform and GPU adaptation. It supports CEL pass/fail `thresholds`, goodput and bandwidth measurements, per-node pass/fail results referenced from status (`succeededNodesRef` / `failedNodesRef`), and your own `spec.overrides` appended to the auto-generated platform overrides. No rebuild needed. See [How-to: Run a WorkloadRun](./run-workloadrun.md).
3. **Hand-authored `Workflow` CR**. Full control over `jobTemplate`, `dependencies`, `orchestration`, and `overrides` at runtime. The controller still detects the platform and GPU architecture and applies matching overrides at reconcile time. This path is kubectl-only and self-maintained: there is no curated guide for it, nothing curates the spec for you, and you own keeping it working across NVCRE upgrades. The [Workflow API reference](../api-reference/workflow.md) documents the spec fields.

Delivering curated catalog updates independently of operator upgrades is tracked in [issue #244](https://github.com/NVIDIA/cluster-readiness-engine/issues/244).
