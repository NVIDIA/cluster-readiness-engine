# Cluster Readiness Engine (CRE)

[![CI](https://github.com/dsx-ai-factory/cluster-readiness-engine/actions/workflows/ci.yml/badge.svg)](https://github.com/dsx-ai-factory/cluster-readiness-engine/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](go.mod)

New GPU clusters often contain faulty nodes, and those faults surface only under real distributed load. CRE is a Kubernetes controller that certifies GPU clusters before production workloads run on them. It runs real training and communication workloads across topology-aware node groups, measures performance, detects hardware failures, and reports every bad node with a reason. Quarantine is left to your platform: CRE never cordons, taints, or otherwise modifies a node.

CRE is for platform and infrastructure teams that bring up, validate, or resell GPU clusters.

## Features

- A certification catalog with NCCL communication tests and multi-node training workloads
- Platform detection (AWS, GCP, Azure, OCI, nscale, TogetherAI, Mistral, Forge, on-prem) and GPU architecture detection (GB200, GB300, H100, A100, L40S, L40)
- Goodput measurement parsed from training logs with configurable LogProfile patterns
- Per-bus bandwidth measurement parsed from NCCL logs
- Node health monitoring with CEL expressions while workloads run
- Per-node failure reporting with a reason for every failed node
- Topology-aware node grouping and adaptive fault isolation
- Checkpoint restart for training jobs
- WorkloadRun, a single resource to run a training, NCCL, or custom workload
- The `ncrectl` CLI for setup, render, run, report, and cleanup

## How it works

The APIs compose like Deployment, ReplicaSet, and Pod:

```mermaid
flowchart LR
    C[Certification] -->|one per catalog category| W[Workflow]
    W -->|one| J[Job]
    J -->|adapter| T[TrainJob and other workloads]
    J -.-> G[GoodputMeasurement]
    J -.-> B[BandwidthMeasurement]
```

A Certification creates one Workflow per catalog category. Each Workflow creates a Job from its template. The Job creates the workload through an adapter, for example a Kubeflow Trainer TrainJob. Measurement resources parse pod logs with LogProfile regex patterns and compute goodput and bandwidth. When a node fails, CRE records it in the certification result with a reason. CRE does not modify nodes; quarantine is left to your platform.

## Quickstart

For complete prerequisites, installation, a first run, and cleanup, see
[Your first certification](docs/getting-started/first-certification.md).

**0. Check the cluster**

CRE requires the NVIDIA GPU Operator and, with the default metrics settings,
the Prometheus Operator CRDs that serve `monitoring.coreos.com/v1`; GB200 and
GB300 catalog entries also require the NVIDIA DRA driver for `ComputeDomain`
resources. The `diagnostics/dcgm-level4` category additionally requires the
standalone DCGM service, which the GPU Operator creates only when
`spec.dcgm.enabled` is true. Because the operator normally uses embedded DCGM
for metrics, standalone DCGM is off by default; enable it with:

```bash
kubectl patch clusterpolicy cluster-policy --type=merge \
  -p '{"spec":{"dcgm":{"enabled":true}}}'
```

Run `kubectl ncre setup status` at any time to see what is present.

**1. Install the CLI**

Every release so far is a pre-release, and `releases/latest` resolves only to the
newest **stable** release. Until `v0.1.0` is tagged, name the version explicitly:

```bash
CRE_VERSION=v0.1.0-rc.8
curl -sSL https://github.com/dsx-ai-factory/cluster-readiness-engine/releases/download/${CRE_VERSION}/installer \
  | bash -s -- -v "${CRE_VERSION}"
```

Once a stable release exists, this shorter form works and picks up the newest one:

```bash
curl -sSL https://github.com/dsx-ai-factory/cluster-readiness-engine/releases/latest/download/installer | bash
```

The installer places `ncrectl` on your `$PATH` and creates a `kubectl-ncre` symlink so the CLI is also available as `kubectl ncre`.

The installer needs a GitHub token while this repository is internal: authenticate with `gh auth login`, or set `GITHUB_TOKEN`.

**2. Set up the cluster**

```bash
kubectl ncre setup init --image-pull-secret "$(gh auth token)"
```

This installs Kubeflow Trainer, the CRE CRDs, the controller, and the built-in LogProfiles.

**3. Certify**

```bash
kubectl ncre certification run \
  --category communication/nccl-all-reduce \
  --wait
```

**4. Report**

The report prints when the run completes. To print it again later, pass the name and the namespace from the run output:

```bash
kubectl ncre certification report <name> -n <namespace>
```

```
╔════════════════════════════════════════════════════════════════╗
║                      Certification Report                      ║
╚════════════════════════════════════════════════════════════════╝

  Name:      ncrectl-20260806-162730
  Platform:  aws
  GPU:       gb300
  Nodes:     16

┌────────────────────────────────────────────────────────────────┐
│  communication/nccl-all-reduce                                 │
├────────────────────────────────────────────────────────────────┤
│  Status:    Succeeded                                          │
│  Runtime:   3m 56s                                             │
│  Scale:     full-scale                                         │
│  Nodes/Job: 16                                                 │
│  Jobs:      1                                                  │
│  MNNVL:     Enabled                                            │
│                                                                │
│  Bandwidth:                                                    │
│    Size       AlgBW        BusBW        Samples                │
│    16 GB      473.44 GB/s  932.09 GB/s  9                      │
└────────────────────────────────────────────────────────────────┘

┌────────────────────────────────────────────────────────────────┐
│  Summary                                                       │
├────────────────────────────────────────────────────────────────┤
│  Categories:   1/1 passed                                      │
│  Failed Nodes: none                                            │
│  Result:       PASSED                                          │
└────────────────────────────────────────────────────────────────┘
```

### Install from the registry instead

`setup init` above is the quickest path. If you would rather manage CRE with
Helm, or need to pin the controller image in your own manifests, both are
published to the GitHub Container Registry on every release.

```bash
CRE_VERSION=v0.1.0-rc.8

# Inspect the chart before installing it
helm show chart oci://ghcr.io/dsx-ai-factory/cluster-readiness-engine --version "$CRE_VERSION"

helm install cluster-readiness-engine \
  oci://ghcr.io/dsx-ai-factory/cluster-readiness-engine \
  --version "$CRE_VERSION" \
  --namespace cluster-readiness-engine \
  --create-namespace
```

The controller image is `ghcr.io/dsx-ai-factory/cluster-readiness-engine/manager`, tagged
with the same release version. Builds from `main` are also published, tagged
`main-<commit-sha>`; use a release tag rather than one of those.

Pin an explicit version rather than installing whatever is newest. Chart and
image versions move together, so the two commands above and your own manifests
should all name the same tag.

## Run a training workload

WorkloadRun is a simplified API for running training, NCCL, or custom workloads. Write a YAML file with an image, a framework, and a node count. CRE detects the platform and GPU architecture.

```yaml
# nccl-all-reduce.yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: WorkloadRun
metadata:
  name: nccl-all-reduce
spec:
  image: nvcr.io/nvidia/pytorch:26.01-py3
  numNodes: 4
  framework:
    mpi:
      binary: /usr/local/bin/all_reduce_perf_mpi
      mpirunPath: /usr/local/bin/mpirun
      args: ["-b", "8", "-e", "32G", "-f", "2", "-n", "100"]
  bandwidthMeasurement:
    logProfileRef: nccl-bandwidth
    testType: all_reduce
```

```bash
kubectl ncre workloadrun run nccl-all-reduce.yaml --wait
```

## Scope and non-goals

CRE certifies clusters with burn-in workloads and reports the nodes that fail. CRE is not:

- A continuous monitoring system. CRE watches nodes only while its workloads run.
- A general workload scheduler or a training platform for production pipelines.
- A benchmark leaderboard. The measurements exist to find faults, not to rank hardware.

## Documentation

- [Architecture Decision Records](docs/designs/) explain the design (ADR-000 to ADR-069).
- [CONTRIBUTING.md](CONTRIBUTING.md) describes the contribution workflow.
- [GOVERNANCE.md](GOVERNANCE.md) and [MAINTAINERS.md](MAINTAINERS.md) describe who decides what.
- [RELEASE.md](RELEASE.md) describes how a release is cut and how to verify one.
- [SECURITY.md](SECURITY.md) describes how to report a vulnerability.

A hosted documentation site is in progress.

## Roadmap

- First tagged release (v0.1.0) with `ncrectl` binaries and the Helm chart
- Hosted documentation site
- Signed artifacts, SBOMs, and build provenance in the release pipeline
- Branch protection and DCO checks for public contributions

## Community

- Ask questions in [GitHub Discussions](https://github.com/dsx-ai-factory/cluster-readiness-engine/discussions).
- Report bugs and request features with the [issue templates](https://github.com/dsx-ai-factory/cluster-readiness-engine/issues/new/choose).
- Report security issues per [SECURITY.md](SECURITY.md), never through GitHub issues.
- We follow the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).

## Development

```bash
make manifests generate    # Regenerate CRDs and DeepCopy
make lint                  # Lint
make test                  # Unit + integration tests
make build                 # Build binary
```

Read [CONTRIBUTING.md](CONTRIBUTING.md) before you open a pull request. Open an issue first, and sign your commits with `git commit -s`.

## License

Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details. Third-party attributions are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
