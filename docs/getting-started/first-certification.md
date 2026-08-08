---
title: Your first certification
description: Install CRE on a GPU cluster, run one certification category, read the report, and clean up.
---

{/* SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved. */}
{/* SPDX-License-Identifier: Apache-2.0 */}

# Your first certification

This guide takes you from an empty GPU cluster to a completed certification report. You install the `ncrectl` CLI, check that your cluster is a valid target, install CRE, run one communication test, and read the result. Plan for 30 to 60 minutes. Most of that time is the workload itself.

## Before you start

You need:

- A Kubernetes cluster with NVIDIA GPU nodes. Every target GPU node must carry the same `nvidia.com/gpu.product` label value. The [NVIDIA GPU Operator](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/) provides these labels. CRE does not install the GPU Operator.
- At least one node without GPUs. The CRE controller schedules only on nodes that do not have the `nvidia.com/gpu.present` label.
- `kubectl` access with permission to create CRDs, cluster roles, and namespaces. The setup step needs this. Later certification runs need less.
- `helm` on your PATH. The setup step calls it.
- The Prometheus Operator CRDs (`monitoring.coreos.com/v1`). The CRE chart always creates a `ServiceMonitor`, and the install fails without them. The [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) chart provides them.
- A GitHub token with the `read:packages` scope. The controller image and the Helm chart live on ghcr.io. Run `gh auth login` once, or set `GITHUB_TOKEN`.
- For GB200 and GB300 clusters only: the NVIDIA DRA driver, because those catalog entries create `ComputeDomain` resources. GB300 RoCE entries also need a Kubernetes version that serves `resource.k8s.io/v1`.
- For training categories only: egress to `github.com` from worker nodes. The training pods clone Megatron-LM at start.

Cordoned nodes are skipped. If a node is cordoned, CRE does not select it, and it does not appear in the results.

## Step 1: install the CLI

```bash
curl -sSL https://github.com/NVIDIA/cluster-readiness-engine/releases/latest/download/installer | bash -s -- -p
```

The `-p` flag accepts pre-releases. All releases are pre-releases today, so keep it until the first stable release exists.

The installer detects your OS and architecture, downloads the matching `ncrectl` binary, installs it to `/usr/local/bin` (with sudo if needed), and adds a `kubectl-ncre` symlink. After it finishes, both forms work and are the same binary:

```bash
ncrectl --version
kubectl ncre --version
```

This guide uses the `kubectl ncre` form, to match the README. Every command also works as `ncrectl <command>`.

The CLI uses your current kubeconfig context. Pass `--kubeconfig` or `--context` on any command to point somewhere else.

## Step 2: check your cluster

Run the preflight commands before you install anything:

```bash
kubectl ncre cluster info
```

The output shows the detected platform (for example `aws`, `gcp`, `azure`, `onprem`), the GPU product with the architecture and GPU count per node, the number of ready nodes, and the network topology if your nodes carry topology labels. Confirm the platform and GPU architecture look right. CRE tunes each workload per platform and per GPU architecture from this detection.

```bash
kubectl ncre setup status
```

Before installation, the expected output is `Status: not ready — run 'ncrectl setup init' to install missing components`. The status also tells you if the GPU Operator is missing. Install the GPU Operator first if it is; CRE cannot do that for you.

If `cluster info` reports `no nodes have nvidia.com/gpu.product label`, the GPU Operator is not labeling your nodes. If it reports `heterogeneous GPUs`, your cluster mixes GPU products. Certify one product at a time by giving a narrower node selector in a Certification YAML.

## Step 3: install CRE

```bash
kubectl ncre setup init --image-pull-secret "$(gh auth token)"
```

The command shows the target cluster and asks for confirmation. Type exactly `yes`. In scripts, pass `--auto-approve`.

Two phases run:

1. `deps` installs Kubeflow Trainer 2.2.1 into the `kubeflow-system` namespace. CRE runs every workload through a Trainer `TrainJob`.
2. `helm` installs the CRE chart into the `cluster-readiness-engine` namespace: the controller, seven CRDs, and five `LogProfile` resources that parse workload logs.

The GitHub token creates a ghcr.io pull secret for the controller image and authenticates the chart pull. Verify the result:

```bash
kubectl ncre setup status
```

All components show ready. If the install hangs and then fails after five minutes, see the troubleshooting table below. The most common cause is a cluster with only GPU nodes, because the controller needs a node without GPUs.

## Step 4: run one category

List what the catalog offers:

```bash
kubectl ncre certification list-categories
```

The catalog has eight categories today. `communication/nccl-all-reduce`, `nccl-all-gather`, and `nccl-alltoall` run NCCL performance tests across all target nodes at once. `nccl-loopback` and `nccl-loopback-nvswitch` run one single-node Job per node and isolate per-node problems. `diagnostics/dcgm-level4` runs the deep DCGM diagnostic on each node. `training/nemotron5-8b` and `nemotron5-56b` run real Megatron-LM pretraining and measure goodput. Both have a minimum GPU count: 4 for the 8B model, 32 for the 56B. The total must also divide evenly by the tensor-parallel width, which varies by architecture — it is 8 on A100, so the 8B model needs 8 GPUs there rather than 4.

Start with `nccl-all-reduce`. It works on any node count and finishes in minutes on a healthy cluster:

```bash
kubectl ncre certification run \
  --category communication/nccl-all-reduce \
  --wait \
  --timeout 60m \
  --results-file ./cert-results.json
```

You do not need an image pull secret here. The workload images are public.

What happens:

1. The CLI discovers the GPU nodes and prints the detected product.
2. It creates a namespace named `ncrectl-<timestamp>` and a Certification in it. **Note both names from the output.** You need them later.
3. The controller creates one Workflow for the category, the Workflow creates a Job, and the Job runs the NCCL test through Kubeflow Trainer across all target nodes.
4. With `--wait`, the CLI prints a status line on every change and a heartbeat every 15 seconds, then prints the report.

The default `--timeout` is 30 minutes. The command above raises it to 60. Training categories need more; give them hours, not minutes.

## Step 5: read the report

`--wait` prints the report when the run completes. The `report` command prints the same table on demand, so use it any time after the run:

```bash
kubectl ncre certification report <name> -n <namespace>
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

CRE reports failed nodes. It does not modify them. Quarantining a bad node (cordon, taint, drain) is your platform's job. Because CRE skips cordoned nodes, `kubectl uncordon <node>` is what makes a repaired node eligible for the next run.


## Step 6: clean up

The run namespace and everything in it:

```bash
kubectl delete namespace <namespace>
```

To remove CRE itself:

```bash
kubectl ncre setup reset
```

`reset` also removes Kubeflow Trainer. Add `--skip-phases=deps` to keep it.

## Troubleshooting the first run

| Symptom | Cause | Fix |
|---|---|---|
| `setup init` hangs, then fails after 5 minutes in the helm phase | The controller cannot schedule. It requires a node without the `nvidia.com/gpu.present` label. | Add a CPU-only node or node pool. |
| `GHCR returned 403` | The token lacks the `read:packages` scope. | `gh auth refresh -s read:packages` |
| `helm not found in PATH` | Setup shells out to helm. | Install helm 3. |
| `no matches for kind "ServiceMonitor"` in the helm phase | The Prometheus Operator CRDs are not installed. The chart requires them. | Install the Prometheus Operator (or at least its CRDs) first. |
| `no nodes have nvidia.com/gpu.product label` | GPU Operator (feature discovery) is not running. | Install or fix the GPU Operator. |
| `heterogeneous GPUs: ...` | Target nodes mix GPU products. | Certify one product at a time with a narrower `spec.target.nodeSelector` in a `--cert-file` YAML. |
| `no GPU nodes match target` | All candidate nodes are cordoned or lack `nvidia.com/gpu.present=true`. | `kubectl uncordon` the nodes you want tested. |
| The watch stops at the timeout | The default `--timeout` is 30 minutes. | Raise `--timeout`. Training runs need hours. The Certification keeps running in the cluster; re-attach with `kubectl get certification <name> -n <namespace> -w`. |
| `dev build "..." has no published chart` | You built `ncrectl` from source. Setup needs a released version to resolve the chart. | Use the installer binary, or pass `--version <chart-version>` to `setup init`. |
| `certification report` finds nothing | The report command defaults to the `default` namespace. | Pass `-n <namespace>` from the run output. |

## Next steps

- Run a single custom workload with the simplified WorkloadRun API: [ADR-059](../designs/059-workloadrun-simplified-api.md) describes it. Write a `WorkloadRun` YAML and run it with `kubectl ncre workloadrun run <file> --wait`.
- Understand the architecture: [ADR-001](../designs/001-adr-abridged.md) is the readable overview, and [ADR-002](../designs/002-layered-crd-hierarchy.md) explains the Certification, Workflow, and Job composition this guide walked through.
- Understand why `certification run` does apply, wait, report, and cleanup in one command: [ADR-050](../designs/050-xcalctl-unified-run-pipeline.md).
- Write a full Certification YAML with several categories, thresholds, and checkpointing, and run it with `--cert-file`.
