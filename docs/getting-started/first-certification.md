---
title: Your first certification
description: Install NVCRE on a GPU cluster, run one certification category, read the report, and clean up.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


This guide takes you from an empty GPU cluster to a completed certification report. You install the `nvcrectl` CLI, check that your cluster is a valid target, install NVCRE, run one communication test, and read the result. Plan for 30 to 60 minutes. Most of that time is the workload itself.

## Before you start

You need:

- A Kubernetes cluster with NVIDIA GPU nodes. Every target GPU node must carry the same `nvidia.com/gpu.product` label value. The [NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/) provides these labels. NVCRE does not install the GPU Operator.
- `kubectl` access with permission to create CRDs, cluster roles, and namespaces. The setup step needs this. Later certification runs need less.
- `helm` on your PATH. The setup step calls it.
- The Prometheus Operator CRDs (`monitoring.coreos.com/v1`), **or** the ServiceMonitor turned off. The chart creates a `ServiceMonitor` by default, so the install fails without those CRDs. Either install them — the [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) chart provides them — or set `metrics.serviceMonitor.enabled=false` and skip them. Turning it off only disables the Prometheus scrape config; the controller still serves metrics.
- Only when your cluster pulls from a private mirror or fork: a GitHub token with the `read:packages` scope, passed to `setup init --image-pull-secret`. The public controller image and Helm chart on ghcr.io need no token.
- For GB200 and GB300 clusters only: the NVIDIA DRA driver, because those catalog entries create `ComputeDomain` resources. GB300 RoCE entries also need a Kubernetes version that serves `resource.k8s.io/v1`.
- For training categories only: egress to `github.com` from worker nodes. The training pods clone Megatron-LM at start.

Cordoned nodes are skipped. If a node is cordoned, NVCRE does not select it, and it does not appear in the results.

## Step 1: install the CLI

```bash
curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash
```

`releases/latest` resolves only to the newest **stable** release, never a pre-release. To install a specific version instead, download the installer from that release and pass the tag:

```bash
curl -fsSL https://github.com/NVIDIA/cluster-readiness-engine/releases/download/<tag>/installer | bash -s -- -v <tag>
```

The installer also takes `-p` to accept pre-releases when it resolves the version itself.

The installer detects your OS and architecture, downloads the matching `nvcrectl` binary, installs it to `/usr/local/bin` (with sudo if needed), and adds a `kubectl-nvcre` symlink. After it finishes, both forms work and are the same binary:

```bash
nvcrectl --version
kubectl nvcre --version
```

This guide uses the `kubectl nvcre` form, to match the README. Every command also works as `nvcrectl <command>`.

The CLI uses your current kubeconfig context. Pass `--kubeconfig` or `--context` on any command to point somewhere else.

## Step 2: check your cluster

Run the preflight commands before you install anything:

```bash
kubectl nvcre cluster info
```

The output shows the detected platform (for example `aws`, `gcp`, `azure`, `onprem`), the GPU product with the architecture and GPU count per node, the number of ready nodes, and the network topology if your nodes carry topology labels. Confirm the platform and GPU architecture look right. NVCRE tunes each workload per platform and per GPU architecture from this detection.

```bash
kubectl nvcre setup status
```

Before installation, the expected output is `Status: not ready — run 'nvcrectl setup init' to install missing components`. The status also tells you if the GPU Operator is missing. Install the GPU Operator first if it is; NVCRE cannot do that for you.

If `cluster info` reports `no nodes have nvidia.com/gpu.product label`, the GPU Operator is not labeling your nodes. If it reports `heterogeneous GPUs`, your cluster mixes GPU products. Certify one product at a time by giving a narrower node selector in a Certification YAML.

## Step 3: install NVCRE

```bash
kubectl nvcre setup init
```

The command shows the target cluster and asks for confirmation. Type exactly `yes`. In scripts, pass `--auto-approve`.

Two phases run:

1. `deps` installs Kubeflow Trainer 2.2.1 into the `kubeflow-system` namespace. NVCRE runs every workload through a Trainer `TrainJob`.
2. `helm` installs the NVCRE chart into the `nvcre` namespace: the controller, seven CRDs, and five `LogProfile` resources that parse workload logs.

The image and chart pull anonymously from ghcr.io; `--image-pull-secret <github-token>` creates a pull secret instead when pulling from a private mirror or fork. Verify the result:

```bash
kubectl nvcre setup status
```

All components show ready. If the install hangs and then fails after five minutes, see the troubleshooting table below. The most common cause is a cluster with only GPU nodes, because the controller needs a node without GPUs.

## Step 4: run one category

List what the catalog offers:

```bash
kubectl nvcre certification list-categories
```

The catalog has eight categories today. `communication/nccl-all-reduce`, `nccl-all-gather`, and `nccl-alltoall` run NCCL performance tests across all target nodes at once. `nccl-loopback` and `nccl-loopback-nvswitch` run one single-node Job per node and isolate per-node problems. `diagnostics/dcgm-level4` runs the deep DCGM diagnostic on each node. `training/nemotron5-8b` and `nemotron5-56b` run real Megatron-LM pretraining and measure goodput. Both have a minimum GPU count: 4 for the 8B model, 32 for the 56B. The total must also divide evenly by the tensor-parallel width, which varies by architecture — it is 8 on A100, so the 8B model needs 8 GPUs there rather than 4.

Start with `nccl-all-reduce`. It works on any node count and finishes in minutes on a healthy cluster:

```bash
kubectl nvcre certification run \
  --category communication/nccl-all-reduce \
  --wait \
  --timeout 60m \
  --results-file ./cert-results.json
```

You do not need an image pull secret here. The workload images are public.

What happens:

1. The CLI discovers the GPU nodes and prints the detected product.
2. It creates a namespace named `nvcrectl-<timestamp>` and a Certification in it. **Note both names from the output.** You need them later.
3. The controller creates one Workflow for the category, the Workflow creates a Job, and the Job runs the NCCL test through Kubeflow Trainer across all target nodes.
4. With `--wait`, the CLI prints a status line on every change and a heartbeat every 15 seconds, then prints the report.

When `--timeout` is not set, the CLI derives it from the selected categories' catalog `timeoutPerJob` budgets (never less than 30 minutes) and prints the derived value when the watch starts — long categories like `diagnostics/dcgm-level4` (about 90 minutes on healthy hardware) get a matching wait budget automatically. The command above pins it to 60 minutes instead. An explicit `--timeout` always wins; training categories with custom settings may need hours. If the timeout expires, the CLI prints and optionally writes a partial `RUNNING` report, then exits with an error. The Certification continues in the cluster unless you passed `--cleanup`.

## Step 5: read the report

`--wait` prints the report when the run completes. The `report` command prints the same table on demand, so use it any time after the run:

```bash
kubectl nvcre certification report <name> -n <namespace>
```

Pass the certification name and the namespace from the run output. The `report` command defaults to the `default` namespace, and the run created its own, so leaving out `-n` finds nothing. This is the most common first-day mistake.

`--results-file` writes the same data as JSON. The report shows the platform and GPU header, one card per category, and a summary. In a category card:

- **Status** and **Runtime** are the outcome and duration.
- **Scale** is the test scale (`full-scale` by default).
- **Nodes/Job** and **Jobs** show how the nodes were partitioned.
- **MNNVL** shows whether multi-node NVLink was on. It defaults to on for GB200 and GB300, off for the rest.
- The bandwidth table lists message sizes with **AlgBW** (algorithm bandwidth) and **BusBW** (bus bandwidth) in GB/s. BusBW is the number to compare against your fabric's expectation.
- **Failed Nodes** lists nodes that failed, with a reason: `WorkloadFailed` (the workload exited badly), `ThresholdViolation` (a performance threshold was missed), or `HardwareFailureDetected` (the node health monitor fired mid-run).

Pass and fail are informational unless you set thresholds. No thresholds ship by default. To enforce them, use a Certification YAML with `options.thresholds` (for example `busBandwidthGBps: "value >= 300"`) and run with `--cert-file` instead of `--category`.

NVCRE reports failed nodes. It does not modify them. Quarantining a bad node (cordon, taint, drain) is your platform's job. Because NVCRE skips cordoned nodes, `kubectl uncordon <node>` is what makes a repaired node eligible for the next run.


## Step 6: clean up

The run namespace and everything in it:

```bash
kubectl delete namespace <namespace>
```

To remove NVCRE itself:

```bash
kubectl nvcre setup reset
```

`reset` also removes Kubeflow Trainer. Add `--skip-phases=deps` to keep it.

## Troubleshooting the first run

| Symptom | Cause | Fix |
|---|---|---|
| `setup init` hangs, then fails after 5 minutes in the helm phase | The controller pod is pending — check `kubectl get pod -n <namespace>` and `kubectl describe pod <controller-pod>` for the cause (resource limits, image pull failure, taint mismatch). | Address the scheduling issue shown in the pod events. |
| `GHCR returned 403` | A stale or under-scoped token was passed via `--image-pull-secret`. Anonymous pulls need no token. | Retry without `--image-pull-secret`, or refresh the token (`gh auth refresh -s read:packages`) if a private mirror requires one. |
| `helm not found in PATH` | Setup shells out to helm. | Install helm 3. |
| `no matches for kind "ServiceMonitor"` in the helm phase | The Prometheus Operator CRDs are not installed, and the chart creates a `ServiceMonitor` by default. | Install the Prometheus Operator (or at least its CRDs), or skip the ServiceMonitor with `metrics.serviceMonitor.enabled=false`. |
| `no nodes have nvidia.com/gpu.product label` | GPU Operator (feature discovery) is not running. | Install or fix the GPU Operator. |
| `heterogeneous GPUs: ...` | Target nodes mix GPU products. | Certify one product at a time with a narrower `spec.target.nodeSelector` in a `--cert-file` YAML. |
| `no GPU nodes match target` | All candidate nodes are cordoned, or the `nvidia.com/gpu.product` label is missing (GPU Operator not running). | `kubectl uncordon` the nodes you want tested, or install the GPU Operator. |
| The watch stops at the timeout | The `--timeout` (derived from category timeouts when not set, 30 minutes minimum) expired. | Read the partial report, then raise `--timeout` if needed. Without `--cleanup`, the Certification keeps running; monitor it with `kubectl get certification <name> -n <namespace> --watch`, print a new snapshot with `kubectl nvcre certification report <name> -n <namespace>` (which exits nonzero while the run is active), or stop it with `kubectl delete certification <name> -n <namespace>`. |
| `dev build "..." has no published chart` | You built `nvcrectl` from source. Setup needs a released version to resolve the chart. | Use the installer binary, or pass `--version <chart-version>` to `setup init`. |
| `certification report` finds nothing | The report command defaults to the `default` namespace. | Pass `-n <namespace>` from the run output. |

## Next steps

- Run a single custom workload with the simplified WorkloadRun API: [ADR-059](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/059-workloadrun-simplified-api.md) describes it. Write a `WorkloadRun` YAML and run it with `kubectl nvcre workloadrun run <file> --wait`.
- Understand the architecture: [ADR-001](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/001-adr-abridged.md) is the readable overview, and [ADR-002](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/002-layered-crd-hierarchy.md) explains the Certification, Workflow, and Job composition this guide walked through.
- Understand why `certification run` does apply, wait, report, and cleanup in one command: [ADR-050](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/050-nvcrectl-unified-run-pipeline.md).
- Write a full Certification YAML with several categories, thresholds, and checkpointing, and run it with `--cert-file`.
