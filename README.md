# CRE

A Kubernetes controller for GPU cluster certification, orchestrated benchmarking, and hardware failure detection. Run real distributed workloads across topology-aware node groups, measure training performance, detect hardware failures, and automatically quarantine bad nodes — before production workloads touch the cluster.

## Install

```bash
# Install the CLI
curl -sSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash
# Include pre-release versions:
curl -sSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash -s -- -p

# Set up the cluster (installs Kubeflow Trainer, CRDs, controller, and LogProfiles)
ncrectl setup init
```

## Certify a GPU cluster

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gpu-cluster-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  categories:
    - domain: communication
      variant: nccl-all-reduce
    - domain: training
      variant: nemotron5-8b
```

```bash
kubectl apply -f certification.yaml
kubectl get certifications.cre.nvidia.com -w
```

Or run the full lifecycle with ncrectl (setup, run, report, cleanup):

```bash
ncrectl certification run --cert-file certification.yaml --image-pull-secret $NGC_API_KEY --wait
```

## Run a training workload

WorkloadRun is a simplified API for running training, NCCL, or custom workloads. Provide an image, framework, and node count — CRE auto-detects the platform and GPU architecture.

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nccl-all-reduce
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  framework:
    mpi:
      binary: /usr/local/bin/all_reduce_perf_mpi
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
  numNodes: 4
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
```

```bash
ncrectl workloadrun run --image-pull-secret $NGC_API_KEY --wait my-workload.yaml
```

## Documentation

Full documentation: **[github.com/NVIDIA/cluster-readiness-engine](https://github.com/NVIDIA/cluster-readiness-engine)**

| | |
|---|---|
| Getting Started | Install ncrectl and set up the cluster |
| Tutorials | Quick Start, WorkloadRun walkthroughs, NCCL test |
| Concepts | API hierarchy, workload types, health monitoring, remediation, goodput |
| How-to Guides | Certify clusters, WorkloadRun, benchmarks, adaptive fault isolation, gray failures |
| Reference | CRD specs (Job, Workflow, Certification, WorkloadRun), ncrectl CLI, metrics |
| Operations | Deployment, monitoring, troubleshooting, FAQ |
| Design Decisions | Architecture Decision Records (`docs/designs/`) |

## Development

```bash
make manifests generate    # Regenerate CRDs and DeepCopy
make lint                  # Lint
make test                  # Unit + integration tests
make build                 # Build binary
```

## License

Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.

