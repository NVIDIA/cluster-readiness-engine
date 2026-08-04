---
title: Custom Catalog Entries
description: Add a new domain/variant pair to the certification catalog.
---
{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}


The catalog is extensible — add a YAML file to `pkg/catalog/entries/` to register a custom certification category.

## File layout

```
pkg/catalog/entries/
  <domain>/
    <variant>.yaml     ← new file
```

The catalog loader discovers entries by scanning this directory tree at startup. No registration step or code change is required.

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
          numNodes: "{{ .NodesPerJob }}"
          numProcPerNode: "{{ .GpusPerNode }}"

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

After adding the file, verify it appears in the catalog and renders correctly:

```bash
ncrectl certification list-categories

ncrectl certification render \
  --platform aws \
  /tmp/my-cert.yaml
```

Check the rendered Workflow for correct resource requests, env vars, and override annotations.
