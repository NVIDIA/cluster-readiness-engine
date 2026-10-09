---
title: Platform Detection & Overrides
description: How the controller auto-detects cloud platform and GPU architecture, and how overrides are applied.
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
---


## Auto-detection

The controller detects two dimensions at runtime:

| Dimension | Source |
|-----------|--------|
| Cloud platform | `spec.providerID` on the Node object (e.g. `aws://...`, `gce://...`) |
| GPU architecture | `nvidia.com/gpu.product` node label (e.g. `NVIDIA-GB200`, `NVIDIA-H100-80GB-HBM3`) |

On a mixed-architecture target, the detected GPU architecture is the one reported by the most nodes, with ties resolved to the architecture whose earliest node sorts first by name. Nodes missing the `nvidia.com/gpu.product` label do not participate in that vote, so an unlabeled node never outvotes labeled ones; `unknown` is detected only when no target node carries the label. Every path uses the same rule: the Certification, Workflow, and WorkloadRun controllers as well as the `nvcrectl` render, cluster info, and workloadrun commands.

### ResourceSlice fallback (no device plugin/GFD)

Platforms that claim GPUs via Dynamic Resource Allocation instead of a device plugin, such as Nscale NKS, run no NVIDIA GPU Feature Discovery DaemonSet, so no node ever carries the `nvidia.com/gpu.product` label. Before the majority-architecture vote runs, node discovery lists `gpu.nvidia.com` `ResourceSlice` objects cluster-wide and reads each device's `productName` attribute, writing it as the `nvidia.com/gpu.product` label onto the corresponding node's in-memory copy (never persisted back to the API server). The driver reports the space-separated NVML name (`NVIDIA GB300`), so it is sanitized exactly as GPU Feature Discovery writes the label: characters outside `[A-Za-z0-9-_. ]` are dropped and each run of whitespace becomes one hyphen (`NVIDIA-GB300`); identical hardware therefore reports one product in a fleet that mixes both, provided GPU Feature Discovery is not time-slicing (it then appends `-SHARED` to its label, so the products differ while the architecture still matches). Every existing label-based consumer — the architecture vote, catalog architecture defaults, NIC detection — is unaffected: they only ever read the label, and simply see it populated from a different source. Devices of type `vfio` (GPU passthrough) are skipped, because they report a PCI-IDs name rather than the NVML product name.

The fallback supplies only the product. GPU nodes must still carry `nvidia.com/gpu.present=true`, which the NVIDIA GPU Operator sets: node discovery drops every node without it before the fallback runs. On a cluster whose GPU nodes carry no NVIDIA labels at all, such as one running only the standalone DRA GPU driver, label the GPU nodes `nvidia.com/gpu.present=true` yourself; otherwise node discovery finds no GPU nodes.

This lookup is skipped entirely when every node already carries the label (every non-DRA cluster), and one `ResourceSlice` `List` call, read directly from the API server rather than the controller cache, covers the whole target set. A missing `resourceslices` RBAC grant, or a driver that publishes no `productName` attribute, degrades to "leave those nodes unlabeled" rather than failing the reconcile — `unknown` architecture is the same outcome an unlabeled node with a device plugin produces today. The fallback reads `resource.k8s.io/v1` ResourceSlices (Kubernetes 1.34+, the same floor the DRA GPU driver needs); on older servers the `List` fails and nodes stay unlabeled.

Offline `nvcrectl certification render` and `nvcrectl workloadrun render` have no cluster to read `ResourceSlice`s from, so on a label-less platform like Nscale, offline render requires `--gpu-arch` to resolve architecture at all. `--gpu-arch` cannot be combined with `--dry-run`, which discovers real nodes and runs the same fallback the controllers do.

The live controller writes detection results to `status.orchestration.detectedPlatform` and `status.orchestration.detectedGPUArchitecture` on the Workflow. When using `nvcrectl workflow render` (client-side), these values are also written as annotations (`nvcrectl.nvidia.com/detected-platform`, `nvcrectl.nvidia.com/detected-gpu-architecture`) on the rendered manifest for offline inspection.

## Override matching

Catalog entries define a base `WorkflowSpec`. Overrides are matched by platform + GPU architecture and applied in order. Two patch mechanisms are supported:

### `jobTemplate` (strategic merge patch)

Replaces entire arrays. Use when you want to set all values for a field (e.g. the full `env` list):

```yaml
jobTemplate:
  spec:
    workload:
      trainJob:
        trainer:
          env:
            - name: MY_VAR
              value: "value"
```

<Warning>
Arrays in `jobTemplate` overrides are **replaced entirely**, not merged by name. If the base spec has env vars, they will be wiped unless you include them in the override.
</Warning>

### `jobTemplatePatch` (RFC 6902 JSON Patch)

Appends or modifies individual fields without replacing arrays. Use `op: add` with path ending in `/-` to append to an array:

```yaml
jobTemplatePatch:
  - op: add
    path: /spec/workload/trainJob/trainer/env/-
    value:
      name: EXTRA_VAR
      value: "extra"
```

<Warning>
A `jobTemplatePatch` that appends to an array must come **after** any `jobTemplate` override that sets that array, or the appended values will be wiped.
</Warning>

## Architecture-specific resources

Different GPU architectures and cloud platforms require different Kubernetes resources:

| Architecture | Platform | Interconnect | Key resources |
|-------------|---------|-------------|--------------|
| GB200 | AWS | EFA | `hugepages-2Mi`, `vpc.amazonaws.com/efa`, EFA hostPath volume, ComputeDomain |
| GB200 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| GB300 | AWS | RoCE | `roce-channel` resource claim (DRA), no hugepages, no EFA |
| GB300 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| H100 | AWS | EFA | `vpc.amazonaws.com/efa: 32`, no hugepages |
| H100 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap |
| H100 | GCP | TCPXO (FastRak) | `tcpxo-daemon` sidecar, `NCCL_FASTRAK_*` env, the 8 GPU NIC networks auto-detected from node allocatable, workload and `tcpxo-daemon` images chosen from the TCPXO plugin version detected on the nodes (A3 Mega only, see [GCP H100 clusters](#gcp-h100-clusters)) |
| GB200/GB300 | GCP | RoCE | `networking.gke.io.networks/rdma-0`..`rdma-3` (fixed network names), ComputeDomain |
| RTX PRO 6000 Blackwell | GCP G4 | TCP over `eth0`, PCIe GPU peer-to-peer | Variable GPU count by G4 machine size; `nvidia.com/gpu=present:NoSchedule` toleration; no RDMA resource request |
| GB200/GB300 | On-prem | InfiniBand | arm64/GPU taint tolerations, portable IB NCCL env (no HCA pinning), NIC resource auto-detected or set via `nicResourceName`, ComputeDomain |
| GB200 | OCI | RoCE | No NIC request by default (`mlnxPerNode: 0`); ComputeDomain. Set `mlnxPerNode` to request `nvidia.com/mlnxnics` and the matching `network-operator/sriov-net` attachments |
| GB300 | OCI | RoCE | `nvidia.com/mlnxnics: 4`, four `network-operator/sriov-net` attachments, ComputeDomain |
| L40S | OCI | RoCE | `nvidia.com/mlnxnics: 2`, two `network-operator/sriov-net` attachments |
| B200/GB300 | Nscale | InfiniBand | `gpu.nvidia.com`/`rdma.nscale.com` DRA `ResourceClaimTemplate`s (no `nvidia.com/gpu` extended-resource request; `dcgm-level4` and `nccl-loopback-nvswitch` claim the GPU only, since the first runs `hostNetwork` and the second uses NVLink only), portable IB NCCL env (no HCA pinning) plus QP/channel tuning on the B200 NCCL collectives, arm64/GPU taint tolerations; GB300 additionally gets the ComputeDomain block and uses `topology.nks.nscale.com/accelerator-domain` as the topology key wherever other platforms use `nvidia.com/gpu.clique` (Nscale runs no GFD, so that label never exists). `dcgm-level4` connects to NKS's `nvidia-platform/nvidia-dcgm-dra` DCGM Service instead of `gpu-operator/nvidia-dcgm` |

OCI GB200 is the one OCI architecture that requests no NIC by default. The
shapes do not consistently expose `nvidia.com/mlnxnics`, and the `sriov-net`
NetworkAttachmentDefinition that the annotation references is not present on a
stock cluster, so a default request leaves every worker pod Pending on
`Insufficient nvidia.com/mlnxnics`. Sites running the SR-IOV device plugin set
`mlnxPerNode` to their own per-node count.

Setting `mlnxPerNode: 0` is a supported opt-out on **any** platform whose
templates request `nvidia.com/mlnxnics` (Azure, OCI, TogetherAI, Forge): at
zero, the resource and the network attachment annotation are both omitted
rather than requested as `"0"`. On Nscale, which claims its NICs through DRA,
zero likewise omits the `rdma.nscale.com` claim while keeping the GPU claim.

RTX PRO 6000 defaults to **eight GPUs per node**, including on GCP. The default
is architecture-based, not machine-shape detection. On smaller G4 nodes, set
`gpusPerNode` explicitly in the Certification or WorkloadRun to the node's
allocatable GPU count (for example, `gpusPerNode: 4` for `g4-standard-192`).
Without that setting, nodes advertising fewer than eight allocatable GPUs are
excluded by the capacity filter; an all-four-GPU target fails rather than
silently reducing the request. An explicit smaller request on an eight-GPU
node tests only the requested GPUs and does not establish full-device coverage.

The NCCL overlay selects `NCCL_P2P_LEVEL=SYS` for eight GPUs and `PHB` for two
or four GPUs, following [Google's G4 guidance](https://docs.cloud.google.com/compute/docs/accelerator-optimized-machines#g4_series).
Offline rendering also uses the eight-GPU default and does not validate node
capacity. Set `gpusPerNode: 4` explicitly when previewing the four-GPU
configuration. The live controller applies the capacity filter.

The GCP RTX PRO 6000 override sets `NCCL_NET_PLUGIN=none` in addition to
`NCCL_IB_DISABLE=1`. The PyTorch image includes an external RDMA plugin that
otherwise attempts to initialize even on this TCP-only network.

The live controller tracks which overrides matched in `status.orchestration.appliedOverrides`. When using `nvcrectl workflow render`, the same information is also written to the `nvcrectl.nvidia.com/applied-overrides` annotation on the rendered manifest.

## On-prem clusters

`onprem` is the detection fallback: a node with an empty `spec.providerID` (and no Forge hostname or NKE site-name label) resolves to the `onprem` platform. Nodes provisioned by bare-metal stacks such as BCM typically carry no providerID, so they land here without any configuration.

For GB200/GB300 targets, the on-prem override contributes:

- **Tolerations** for the `kubernetes.io/arch=arm64:NoSchedule` and `nvidia.com/gpu=present:NoSchedule` taints common on NVL72 deployments. Without them, workload pods never schedule on tainted arm64 nodes.
- **A portable InfiniBand NCCL environment** without HCA pinning: NCCL auto-detects HCAs when `NCCL_IB_HCA` is unset, so the same override works across sites with different HCA layouts.
- **A NIC resource request**, detected automatically or configured via `nicResourceName`. Resource names vary by RDMA device plugin (`rdma/ib`, `nvidia.com/mlnxnics`, and others are all in the wild), so when the field is unset the controller inspects the target nodes: if exactly one candidate resource (any `rdma/*` name, or the exact name `nvidia.com/mlnxnics`) is allocatable at the resolved `mlnxPerNode` count on every target node, that name is requested. Detection never guesses or over-commits: with zero candidates, with several, or when candidates exist but no node set can cover the requested count, no NIC resource is requested (pods still schedule, without an explicit NIC allocation) and a Normal `NICResourceDetection` event on the Certification or WorkloadRun explains what was found, naming the requested count when candidates fall below it. Set `nicResourceName` to override detection or to resolve an ambiguous fleet. The per-container count always comes from `mlnxPerNode`, detected or not. GB200/GB300 default `mlnxPerNode` to 8; sites running a shared-device plugin (one pooled resource per pod) should set `mlnxPerNode: 1`; with the default of 8, a pooled resource advertised as `rdma/ib: 1` is not detected, because the resulting request could never schedule.

Offline `nvcrectl certification render` and `nvcrectl workloadrun render` have no cluster to inspect, so without `--dry-run` they render field-only: the NIC resource appears only when `nicResourceName` is set. With `--dry-run`, real nodes are discovered and detection runs exactly as in the controllers.

```yaml
apiVersion: nvcre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: onprem-gb300-cert
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.product: NVIDIA-GB300
  nodesPerJob: 18
  enableMNNVL: true
  # Extended resource name advertised by the site's RDMA device plugin.
  # Optional: when omitted, the controller auto-detects a single qualifying
  # rdma/* or nvidia.com/mlnxnics resource allocatable at the mlnxPerNode
  # count on every target node.
  # Set it to override detection or when several candidates are advertised.
  nicResourceName: rdma/ib
  mlnxPerNode: 1   # shared-device plugin: one pooled resource per pod
  categories:
    - domain: communication
      variant: nccl-all-reduce
```

<Warning>
**Metal3 detection trap.** Nodes with a `metal3://` providerID resolve to the `mistral` platform, not `onprem`. An on-prem site provisioned by Cluster API + Metal3 therefore silently picks up the Mistral site-specific override, which pins HCA names (`NCCL_IB_HCA`, `UCX_NET_DEVICES`) and the `rdma/ib` resource name. To preview what a generic on-prem render looks like regardless of providerID, pass the platform explicitly: `nvcrectl certification render --platform onprem <cert.yaml>`.
</Warning>

Diagnostics (`dcgm-level4`) needs no on-prem override: it already tolerates all taints and runs intra-node, so it schedules on tainted arm64 nodes unchanged.

## GCP H100 clusters

On GCP H100, the override runs NCCL over GPUDirect-TCPXO. It adds a `tcpxo-daemon` sidecar, the `NCCL_FASTRAK_*` environment, and a `networking.gke.io/interfaces` pod annotation that attaches the pod to the node's eight GPU NIC networks as `eth1` through `eth8`. GKE looks up each network in that annotation as a `Network` object when the pod is admitted, and rejects the pod if one is missing. The names are chosen by whoever provisions the cluster: AICR names them `<deployment-id>-gpu-nic-0` through `-7`, and Google's samples use `vpc1` through `vpc8`.

So the controller reads the names from the target nodes rather than assuming them. GKE advertises an extended resource `networking.gke.io.networks/<network>` on every node attached to a network:

- **Which networks.** The controller uses the networks advertised on every target node, and only when there are exactly eight of them. It ignores the `<network>.IP` resources and the `default` network.
- **Which order.** When the nodes' `networking.gke.io/north-interfaces` and `networking.gke.io/nic-info` annotations place each network on a host NIC, and all nodes agree, the pod's `eth1` carries the same network as the host's `eth1`, and so on. Otherwise the names are sorted.
- **When detection fails.** Detection never guesses. With any count other than eight, the controller renders the default names `gpu-nic0` through `gpu-nic7` and emits a Warning `GKENetworkDetection` event on the Certification listing what it found. Causes include no multi-networking, a network missing from one node, a ninth network on every node, or an A3 High pool. GKE rejects the resulting pods unless the cluster has `Network` objects with those default names.

To see what the nodes advertise, list the networks and a GPU node's allocatable resources:

```bash
kubectl get networks.networking.gke.io
kubectl get node <gpu-node> -o jsonpath='{.status.allocatable}'
```

Offline `nvcrectl certification render` has no nodes to inspect, so it renders the default names. With `--dry-run`, detection runs against the real nodes and prints the networks it found, or why it fell back, to stderr.

The TCPXO NCCL plugin comes from the node, not the image. GKE's TCPXO installer puts it in `/home/kubernetes/bin/nvidia`, which the override mounts at `/usr/local/nvidia`. The plugin links a CUDA runtime that it does not bundle, so it loads the one in the workload image, and the two CUDA major versions must match. Google also pairs each plugin release with one `tcpxo-daemon` release. Both are properties of the installed plugin, not of GCP H100: Google qualifies plugin v1.0.16 and earlier against CUDA 12, and v1.0.17 against CUDA 13.2 (with GPU driver R595 and GKE 1.33.5-gke.1125000 or later).

**Plugin version detection.** The controller reads the plugin release from GKE's `nccl-tcpxo-installer` pods in `kube-system`. The release is the tag of the init container image `nccl-plugin-gpudirecttcpx-dev`, read through an `@sha256` digest if one is appended. The image is matched by that last path segment, so a mirrored registry is still recognized. Google allows upgrading the plugin one node pool at a time, so only the installers on the job's target nodes count, and only Running ones. Google sometimes republishes a release as a rebuild with a build suffix (`v1.0.13-1` a week after `v1.0.13`). A suffix does not change the release, so `v1.0.17-1` gets the v1.0.17 row, and nodes caught mid-rollout between `v1.0.17` and `v1.0.17-1` agree on v1.0.17. The release then picks the images:

| Plugin version | NCCL test image | Training image | `tcpxo-daemon` |
|---|---|---|---|
| v1.0.15 (AICR) | `pytorch:25.06-py3` (CUDA 12.9) | `pytorch:25.06-py3` | `v1.0.21` |
| v1.0.16 | `pytorch:25.06-py3` | `pytorch:25.06-py3` | `v1.0.22` |
| v1.0.17 | `pytorch:26.01-py3` (CUDA 13.1) | `pytorch:25.08-py3` (CUDA 13.0) | `v1.0.23` |
| newer than v1.0.17 | same as v1.0.17 | same as v1.0.17 | same as v1.0.17 |
| older than v1.0.15, or none found | same as v1.0.15 | same as v1.0.15 | same as v1.0.15 |

The images cover the NCCL test workers, launcher and launcher init container, and the training workers and their `megatron-clone` init container. The CUDA 13 images are an earlier CUDA 13 minor version than Google's 13.2 qualification; they have the same `libcudart.so.13` soname but have not yet been validated on a v1.0.17 cluster. The v1.0.15 and v1.0.16 rows are validated on an AICR A3 Mega cluster (GKE 1.35, driver 580.159.04): `nccl-all-reduce` reaches 337.72 GB/s (v1.0.15) and 337.48 GB/s (v1.0.16) bus bandwidth at 16 GB, and `nemotron5-8b` 425.6 and 422.9 TFLOPs per GPU.

**Supported releases and fallbacks.** v1.0.15 is the minimum supported release. Detection never guesses silently: unless every target node runs one mapped release, the controller renders the nearest safe row and emits a Warning `TCPXOPluginDetection` event on the Certification saying what it found and what it rendered.

- **A release newer than the latest mapped one** renders the latest row, since a newer plugin keeps at least the newest CUDA major. Those images are not validated against the newer release.
- **A release between mapped ones that has no row of its own** renders the newest row older than it, for the same reason, and is not validated either.
- **A release older than v1.0.15** renders the v1.0.15 row, since every earlier release is also CUDA 12. Upgrade the installer if you can.
- **No release at all** (no running installer pod on a target node, nodes on different releases, an untagged or non-release tag, or a failed pod List) renders the v1.0.15 row: the release AICR-provisioned clusters pin, and the one validated on hardware. A cluster built from Google's published manifest gets the latest plugin, which is CUDA 13, so if detection finds nothing there, set the image yourself.

To check the installed release:

```bash
kubectl -n kube-system get daemonset nccl-tcpxo-installer \
  -o jsonpath='{.spec.template.spec.initContainers[*].image}'
```

Offline `nvcrectl certification render` has no cluster to read, so it renders the v1.0.15 row. With `--dry-run`, detection runs against the real installer pods and prints the release and images it picked, or why it fell back, to stderr.

**Overriding the image.** Set `image` per category, under `categories[].options.image`, not as the certification-wide `spec.image`. The spec-level field is a default for every category, so it also replaces the image of categories that have nothing to do with TCPXO. `diagnostics/dcgm-level4` runs `dcgmi` out of a DCGM image, and a PyTorch image has no `dcgmi`, so the spec-level form fixes NCCL and breaks diagnostics in the same certification:

```yaml
categories:
  - domain: communication
    variant: nccl-all-reduce
    options:
      image: nvcr.io/nvidia/pytorch:26.01-py3   # CUDA 13 plugin
  - domain: diagnostics
    variant: dcgm-level4                        # keeps its own DCGM image
```

Whatever image you set, it must also ship what the entry runs: the `*_perf_mpi` NCCL test binaries, or the training stack. The `tcpxo-daemon` sidecar keeps its own image and is unaffected by `image`.

**Symptom to fix.** Both directions of the mismatch fail at plugin load, and the missing soname names the CUDA major the plugin wants, not the one the image has:

| NCCL log line | Meaning | Fix |
|---|---|---|
| `Error loading libnccl-net_internal.so: libcudart.so.12` | plugin wants CUDA 12, image is CUDA 13 | set `categories[].options.image` to a CUDA 12 image |
| `Error loading libnccl-net_internal.so: libcudart.so.13` | plugin wants CUDA 13, image is CUDA 12 | set `categories[].options.image` to a CUDA 13 image |

The TCPXO NCCL plugin also checks the NCCL environment against the `a3plus_guest_config.textproto` that GKE's TCPXO installer puts on each node, and aborts the job on any value the file enforces. The `NCCL_FASTRAK_*` environment NVCRE sets matches the enforced values of current installers, for example `NCCL_PROTO=Simple,LL128`. It also leaves `NCCL_ALGO` unset, as the file recommends. If a job fails with `NCCL WARN NCCL/NET (shim) mismatch enforced`, compare the named variable with that file on the node: `/home/kubernetes/bin/nvidia/lib64/a3plus_guest_config.textproto`, which is mounted in the pod at `/usr/local/nvidia/lib64/`.

<Note>
Only A3 Mega (`a3-megagpu-8g`) is supported. A3 High (`a3-highgpu-8g`) is also H100, but it uses GPUDirect-TCPX with four GPU NICs, which this override does not configure. Both report `nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3`, so the override cannot tell them apart. The eight-network rule is what flags an A3 High target.
</Note>
