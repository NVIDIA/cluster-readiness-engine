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

Nodes with a `spec.providerID` starting with `linode://` are detected as `akamai` (Akamai LKE). The CLI also accepts `--platform akamai` for offline rendering.

### ResourceSlice fallback (no device plugin/GFD)

Platforms that claim GPUs via Dynamic Resource Allocation instead of a device plugin, such as Nscale NKS, run no NVIDIA GPU Feature Discovery DaemonSet, so no node ever carries the `nvidia.com/gpu.product` label. Before the majority-architecture vote runs, node discovery lists `gpu.nvidia.com` `ResourceSlice` objects cluster-wide and reads each device's `productName` attribute, writing it as the `nvidia.com/gpu.product` label onto the corresponding node's in-memory copy (never persisted back to the API server). The driver reports the space-separated NVML name (`NVIDIA GB300`), so it is sanitized exactly as GPU Feature Discovery writes the label: characters outside `[A-Za-z0-9-_. ]` are dropped and each run of whitespace becomes one hyphen (`NVIDIA-GB300`); identical hardware therefore reports one product in a fleet that mixes both, provided GPU Feature Discovery is not time-slicing (it then appends `-SHARED` to its label, so the products differ while the architecture still matches). Every existing label-based consumer — the architecture vote, catalog architecture defaults, NIC detection — is unaffected: they only ever read the label, and simply see it populated from a different source. Devices of type `vfio` (GPU passthrough) are skipped, because they report a PCI-IDs name rather than the NVML product name.

The fallback supplies only the product. GPU nodes must still carry `nvidia.com/gpu.present=true`, which the NVIDIA GPU Operator sets: node discovery drops every node without it before the fallback runs. On a cluster whose GPU nodes carry no NVIDIA labels at all, such as one running only the standalone DRA GPU driver, label the GPU nodes `nvidia.com/gpu.present=true` yourself; otherwise node discovery finds no GPU nodes.

This lookup is skipped entirely when every node already carries the label (every non-DRA cluster), and one `ResourceSlice` `List` call, read directly from the API server rather than the controller cache, covers the whole target set. A missing `resourceslices` RBAC grant, or a driver that publishes no `productName` attribute, degrades to "leave those nodes unlabeled" rather than failing the reconcile — `unknown` architecture is the same outcome an unlabeled node with a device plugin produces today. The fallback reads `resource.k8s.io/v1` ResourceSlices (Kubernetes 1.34+, the same floor the DRA GPU driver needs); on older servers the `List` fails and nodes stay unlabeled.

Offline `nvcrectl certification render` and `nvcrectl workloadrun render` have no cluster to read `ResourceSlice`s from, so on a label-less platform like Nscale, offline render requires `--gpu-arch` to resolve architecture at all. `--gpu-arch` cannot be combined with `--dry-run`, which discovers real nodes and runs the same fallback the controllers do.

The live controller writes detection results to `status.orchestration.detectedPlatform` and `status.orchestration.detectedGPUArchitecture` on the Workflow. `nvcrectl workflow render` and `nvcrectl certification render`, offline or with `--dry-run`, also write these values as annotations (`nvcrectl.nvidia.com/detected-platform`, `nvcrectl.nvidia.com/detected-gpu-architecture`) on each rendered Workflow for inspection. An offline `certification render` detects from a synthetic node built from `--platform`, `--gpu-arch` and the target `nodeSelector`. Without `--platform` that node has no `providerID`, so it detects as `onprem` and the on-prem overrides apply, unless the `nodeSelector` carries a Forge marker (`nke.nvidia.com/site-name`, or a `kubernetes.io/hostname` containing `forge`): the node then detects as `forge`, with or without `--platform onprem`.

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
| GB200 | AWS | EFA | `hugepages-2Mi`, `vpc.amazonaws.com/efa`, ComputeDomain |
| GB200 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| GB300 | AWS | RoCE | `roce-channel` resource claim (DRA), no hugepages, no EFA |
| GB300 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap, ComputeDomain |
| B300 | AWS | EFA | `vpc.amazonaws.com/efa: 16` (p6-b300.48xlarge: card 0 is ENA-only), no hugepages, same EFA env as H100 |
| H100 | AWS | EFA | `vpc.amazonaws.com/efa: 32`, no hugepages |
| H100 | Azure | InfiniBand | mlnxnics dep, topo ConfigMap |
| H100 | GCP | TCPXO (FastRak) | `tcpxo-daemon` sidecar, `NCCL_FASTRAK_*` env, the 8 GPU NIC networks auto-detected from node allocatable, workload and `tcpxo-daemon` images chosen from the TCPXO plugin version detected on the nodes (A3 Mega only, see [GCP H100 clusters](#gcp-h100-clusters)) |
| GB200/GB300 | GCP | RoCE | `networking.gke.io.networks/rdma-0`..`rdma-3` (fixed network names), ComputeDomain |
| RTX PRO 6000 Blackwell | GCP G4 | TCP over `eth0`, PCIe GPU peer-to-peer | Variable GPU count by G4 machine size; `nvidia.com/gpu=present:NoSchedule` toleration; no RDMA resource request |
| RTX PRO 6000 Blackwell | Akamai LKE | TCP over `eth0` | Standard `nvidia.com/gpu` resources; inspected GPU nodes are untainted; no RDMA request; set `gpusPerNode` to the VM GPU count |
| RTX PRO 6000 Blackwell | AWS g7e.48xlarge | EFA, PCIe GPU peer-to-peer | Eight GPUs and `vpc.amazonaws.com/efa: 4` per worker; no ComputeDomain or multi-node NVLink |
| GB200/GB300 | On-prem | InfiniBand | arm64/GPU taint tolerations, portable IB NCCL env (no HCA pinning), NIC resource auto-detected or set via `nicResourceName`, ComputeDomain |
| B200/B300 (x86 HGX) | On-prem | InfiniBand or RoCE via the site's RDMA device plugin | `nvidia.com/gpu:NoSchedule` toleration (any taint value, on the MPI launcher too), NIC resource auto-detected or set via `nicResourceName`, or one resource and Multus network per rail via `networkAttachments`; no NCCL environment, no ComputeDomain (NVLink does not span nodes) |
| GB200 | OCI | RoCE | No NIC request by default (`mlnxPerNode: 0`); ComputeDomain. Set `mlnxPerNode` to request `nvidia.com/mlnxnics` and as many attachments to the [detected RDMA network](#oci-rdma-network) |
| GB300 | OCI | RoCE | `nvidia.com/mlnxnics: 4`, four attachments to the [detected RDMA network](#oci-rdma-network), ComputeDomain |
| L40S | OCI | RoCE | `nvidia.com/mlnxnics: 2`, two attachments to the [detected RDMA network](#oci-rdma-network) |
| B200/GB300 | Nscale | InfiniBand | `gpu.nvidia.com`/`rdma.nscale.com` DRA `ResourceClaimTemplate`s (no `nvidia.com/gpu` extended-resource request; `dcgm-level4` and `nccl-loopback-nvswitch` claim the GPU only, since the first runs `hostNetwork` and the second uses NVLink only), portable IB NCCL env (no HCA pinning) plus QP/channel tuning on the B200 NCCL collectives, arm64/GPU taint tolerations; GB300 additionally gets the ComputeDomain block and uses `topology.nks.nscale.com/accelerator-domain` as the topology key wherever other platforms use `nvidia.com/gpu.clique` (Nscale runs no GFD, so that label never exists). `dcgm-level4` connects to NKS's `nvidia-platform/nvidia-dcgm-dra` DCGM Service instead of `gpu-operator/nvidia-dcgm`, unless `dcgmHost` is set, which wins on every platform |

OCI GB200 is the one OCI architecture that requests no NIC by default. The
shapes do not consistently expose `nvidia.com/mlnxnics`, and a stock cluster
has no NetworkAttachmentDefinition for it to attach, so a default request would
fail [detection](#oci-rdma-network) or, with a NetworkAttachmentDefinition in
place, leave every worker pod Pending on `Insufficient nvidia.com/mlnxnics`.
Sites running the SR-IOV device plugin set `mlnxPerNode` to their own per-node
count.

Setting `mlnxPerNode: 0` is a supported opt-out on **any** platform whose
templates request `nvidia.com/mlnxnics` (Azure, OCI, TogetherAI, Forge): at
zero, the resource and the network attachment annotation are both omitted
rather than requested as `"0"`. On Nscale, which claims its NICs through DRA,
zero likewise omits the `rdma.nscale.com` claim while keeping the GPU claim.

AWS B300 (`p6-b300.48xlarge`) defaults to **eight GPUs per node** and requests
**16** `vpc.amazonaws.com/efa` devices per worker: AWS documents 17 network cards,
of which the primary is ENA-only and the 16 secondary cards support EFA (see the
[AWS EFA instance specifications](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa-acc-inst-types.html)).
Confirm the device plugin advertises 16 allocatable devices; the H100 request for
32 cannot schedule on these nodes. The five NCCL entries use
`public.ecr.aws/hpc-cloud/nccl-tests:cuda13.0.2-efa1.48.0-ofiv1.19.0-ncclv2.30.4-1-testsv2.18.3`,
which carries native `sm_103` code and meets the
[AWS P6-B300 software requirements](https://docs.aws.amazon.com/dlami/latest/devguide/p6-support-dlami.html)
(CUDA 13.0, R580, EFA 1.44.0, AWS OFI NCCL 1.17.1). WorkloadRuns get the same EFA
request and transport settings on the caller's image, which must carry a compatible
EFA/NCCL stack. Images with multiple network plugins may also need explicit
[NCCL plugin selection](https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html#nccl-net-plugin)
in `spec.env`. For example, NGC PyTorch `26.04-py3` installs the AWS plugin as
`libnccl-net-ofi.so`; set `NCCL_NET_PLUGIN=ofi` to select it. Otherwise that image
can use sockets even with EFA devices allocated. Verify EFA provider selection
and GDR data channels in the NCCL logs, not just successful workload completion.

The training entries have no AWS B300 override yet. Their PyTorch 25.08
image bundles EFA 1.38.1 / OFI NCCL 1.14.0, below those minimums. Nemotron 5 on
AWS B300 renders the base entry with no EFA devices or plugin cleanup. This
training path is not supported and has not been validated on B300 hardware.

RTX PRO 6000 defaults to **eight GPUs per node**, including on GCP. The default
is architecture-based, not machine-shape detection. On smaller G4 nodes, set
`gpusPerNode` explicitly in the Certification or WorkloadRun to the node's
allocatable GPU count (for example, `gpusPerNode: 4` for `g4-standard-192`).
Without that setting, nodes advertising fewer than eight allocatable GPUs are
excluded by the capacity filter; an all-four-GPU target fails rather than
silently reducing the request. An explicit smaller request on an eight-GPU
node tests only the requested GPUs and does not establish full-device coverage.

The GCP NCCL overlay selects `NCCL_P2P_LEVEL=SYS` for eight GPUs and `PHB` for two
or four GPUs, following [Google's G4 guidance](https://docs.cloud.google.com/compute/docs/accelerator-optimized-machines#g4_series).
Offline rendering also uses the eight-GPU default and does not validate node
capacity. Set `gpusPerNode: 4` explicitly when previewing the four-GPU
configuration. The live controller applies the capacity filter.

The GCP RTX PRO 6000 override sets `NCCL_NET_PLUGIN=none` in addition to
`NCCL_IB_DISABLE=1`. The PyTorch image includes an external RDMA plugin that
otherwise attempts to initialize even on this TCP-only network.

### AWS RTX PRO 6000

The AWS RTX cell targets `g7e.48xlarge`: eight RTX PRO 6000 Blackwell Server
Edition GPUs and four EFA interfaces per node. Its override matches **both**
`platform: aws` and `gpuArchitecture: rtxpro6000`. The H100 fallback explicitly
excludes RTX, so a g7e worker never inherits the p5 request for 32 EFA devices.
The global RTX defaults and the GCP TCP profile are unchanged.

Install the AWS EFA device plugin and confirm each target node advertises
`nvidia.com/gpu: 8` and `vpc.amazonaws.com/efa: 4`. Requesting the EFA resource
provides device access; installing a host driver alone does not advertise the
resource or make it available to a pod. Do not use GCP's `NCCL_NET_PLUGIN=none`
profile: the AWS cell selects the OFI plugin and the EFA provider. Multi-node
NVLink and NVLS are disabled because g7e GPUs communicate through PCIe and EFA.

EFA requests scale with `gpusPerNode`: one interface per pair of GPUs,
rounded up. GPU counts are from the
[AWS G7e specifications](https://aws.amazon.com/ec2/instance-types/g7e/).
EFA interface limits follow the instance's network-card count, with one EFA
interface per card:

| Instance type | GPUs per instance | Maximum EFA interfaces | EFA request using all GPUs |
| --- | --- | --- | --- |
| `g7e.8xlarge` | 1 | 1 | 1 |
| `g7e.12xlarge` | 2 | 1 | 1 |
| `g7e.24xlarge` | 4 | 2 | 2 |
| `g7e.48xlarge` | 8 | 4 | 4 |

Smaller shapes require an explicit `gpusPerNode` override; the RTX architecture
still defaults to eight GPUs and does not detect the instance size. Keep the
requested GPU count within the node's physical capacity. The smaller
`g7e.2xlarge` and `g7e.4xlarge` do not support EFA. AWS also lists
[GPUDirect RDMA support](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa.html)
for multi-GPU G7e sizes, but not `g7e.8xlarge`. Target nodes must advertise
enough EFA devices for the request; the overlay does not fall back to TCP.
Live validation and bandwidth baselines cover only full eight-GPU
`g7e.48xlarge` workers; the table does not qualify smaller shapes.

The NCCL all-reduce, all-gather, all-to-all, EFA loopback and Nemotron 5 8B
entries pin NVIDIA PyTorch 26.03 with EFA 1.46.0 and NCCL 2.29.7 by digest.
Its CUDA 13.2 stack needs a compatible host driver. SM120 kernels are required;
the rejected AWS NCCL candidate compiled only for SM80–SM103 could not run
these RTX GPUs (see ADR-109).
The two-node validation used host driver 580.173.02 with the image's CUDA
compatibility libraries. The catalog forwards PATH and LD_LIBRARY_PATH to
SSH-launched MPI ranks so those image paths remain available. The
`nccl-loopback-nvswitch` entry has no dedicated AWS RTX override and renders
its generic base configuration. On g7e, that configuration can exercise PCIe;
success does not validate NVSwitch hardware. Use `nccl-loopback` for the
validated EFA loopback path.

Nemotron 5 56B also has no dedicated AWS RTX override. Once its minimum of
32 GPUs is met (four eight-GPU nodes), it renders the generic base configuration:
`nvcr.io/nvidia/pytorch:25.08-py3`, with no `vpc.amazonaws.com/efa` request or
AWS RTX EFA transport settings. It does not receive the 8B image or DDP
stream-ordering backport. Rendering succeeds at that size, but 56B has not
been validated on AWS RTX; do not treat that render as supported EFA training.

The AWS RTX 8B runtime includes a guarded Megatron core_v0.15.2 DDP stream
ordering backport after the clone init container. It prevents parameter-buffer
copies racing initialization and optimizer setup; the original parameter
integrity checks stay enabled. The patch writes only the inspected training.py
version. Other sources are left unchanged only when the `get_model` function's
AST matches the inspected synchronized implementation, including both waits
around DDP construction; otherwise the init container fails with an explanation.
The source restriction also applies to `sourceRepo` and pre-seeded workspaces
(see [Megatron-LM source](../operations/deployment.md#training-categories-megatron-lm-source)).
The guard takes `/mnt/workspace/.megatron-lm.lock` to serialize against other
patch guards, then reads and validates the source under that lock.
Workspace preparation takes the same lock in `megatron-clone`, added by
[PR #555](https://github.com/NVIDIA/cluster-readiness-engine/pull/555).
The guard writes a sibling temporary file and atomically replaces the target,
preserving its permissions. Concurrent
readers see a complete file; concurrent guards recheck the updated digest.
If lock acquisition raises `OSError`, the guard logs the failure and continues
without the lock, matching workspace preparation. Concurrent guards can then
install the same verified patch independently; each replacement remains atomic,
and the source-hash and AST checks still apply.
The init command times out after 1200 seconds if preparation cannot finish.
Two-node validation
passes all sixteen ranks and ten training steps. This is a smoke test;
training qualification and thresholds remain separate work. See ADR-109 for
the upstream references, source guards and CUDA graph limitation.

WorkloadRun applies the same EFA resource policy and transport settings
while preserving the caller's workload image. Its MPI, torch and exec workers
request `IPC_LOCK` for RDMA memory registration; only MPI also requests
`SYS_CHROOT` for sshd. The caller must supply an
EFA-capable image with compatible CUDA, libfabric and aws-ofi-nccl libraries;
the overlay does not install userspace libraries into arbitrary images. MPI
transport settings reach worker ranks through `mpirun -x`, while torch and
exec workloads receive container environment variables.

Bandwidth thresholds are specific to the node count, software and topology;
collect live baselines before setting them. Offline rendering checks the
selected configuration, not the physical fabric or its performance.

The live controller tracks which overrides matched in `status.orchestration.appliedOverrides`. `nvcrectl workflow render` and `nvcrectl certification render` also write the same information to the `nvcrectl.nvidia.com/applied-overrides` annotation on each rendered Workflow, and omit the annotation when no override matched.

### OCI RDMA network

On OCI, the NCCL entries, and the Nemotron entries on L40S, L40 and GB300,
request `nvidia.com/mlnxnics` and attach one Multus secondary network per NIC
(on GB200 too, once `mlnxPerNode` is set above its default of 0)
through the `k8s.v1.cni.cncf.io/networks` annotation. The platform owns that
network and names it: AICR installs the NVIDIA Network Operator into
`nvidia-network-operator`, for example, and a switchdev cluster provides a
`host-device` network rather than an SR-IOV one. So the controller does not
assume a name. It lists NetworkAttachmentDefinitions and keeps those annotated
`k8s.v1.cni.cncf.io/resourceName: nvidia.com/mlnxnics`, the annotation Multus
needs to hand each attachment the VF allocated for that resource
([ADR-103](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/103-oci-rdma-network-detection.md)).

The candidates are the matching NetworkAttachmentDefinitions in the
Certification's namespace if there are any, otherwise the matching ones in
every namespace.

| Candidates | Result |
|---|---|
| Exactly one | Attached. |
| Several, all equivalent | The first `<namespace>/<name>` in sorted order is attached, and a Normal `SecondaryNetworkDetection` event names the ones skipped. Equivalent means the same `spec.config` apart from its top-level `name`; an empty or non-JSON `spec.config` is never equivalent. |
| Several that differ | The Certification fails with `SecondaryNetworkDetectionFailed`, naming the candidates and the `spec.config` keys that differ. |
| None | The Certification fails with `SecondaryNetworkDetectionFailed`, saying what it found. |

Both failures happen before that category's Workflow is created. The no-match
message lists the ways forward: create a NetworkAttachmentDefinition for
`nvidia.com/mlnxnics`, name the networks explicitly with
[`networkAttachments`](../api-reference/certification.md), or set
`mlnxPerNode: 0`, which certifies without RDMA NICs unless
`rdmaDeviceAccess: hostPath` is also set on a cluster that exposes
`/dev/infiniband` without a device plugin. `hostPath` on its own still requests
`nvidia.com/mlnxnics` and attaches the network, so it does not avoid
detection. The message for several that differ suggests `networkAttachments` or
removing the NetworkAttachmentDefinitions that should not be used. When one of
them is `network-operator/sriov-net`, it also asks you to delete that one if it
is left over from an earlier setup or was made by hand as a workaround. A
cluster that serves no NetworkAttachmentDefinition API fails as no match does,
and a refused list fails with a message asking you to restore the chart's
grant. Any other list error is retried; `--dry-run` fails on it instead.

Detection runs only for a category whose rendered Workflow attaches the
network. Other platforms, `dcgm-level4`, Nemotron on H100, A100, B200, B300
and H200, `mlnxPerNode: 0`, and an explicit `networkAttachments` list never
list NetworkAttachmentDefinitions. The manager role grants `get` and `list` on
`network-attachment-definitions.k8s.cni.cncf.io` for it, and nothing more.

`nvcrectl certification render` without `--dry-run` has no cluster to look at.
It attaches `network-operator/sriov-net`, the name NVCRE used before detection,
and says so on stderr. With `--dry-run` it detects the network as the
controller does, using your kubeconfig, and prefers NetworkAttachmentDefinitions
in the Certification's namespace or, for a manifest that sets none, in
`--namespace` (`default` unless given). If you may not list
NetworkAttachmentDefinitions, it prints a warning and renders the default
instead of failing, since the controller lists with its own service account.

If Multus runs with `namespaceIsolation: true`, a pod may only reference a
NetworkAttachmentDefinition in its own namespace, or in a namespace listed in
`globalNamespaces`. `default` is allowed implicitly only while
`globalNamespaces` is unset. Either list the platform network's namespace in
`globalNamespaces`, or provide a matching NetworkAttachmentDefinition in the
Certification's namespace, which detection then prefers over any elsewhere.

### Akamai RTX PRO 6000 TCP

See [ADR-101](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/101-akamai-rtxpro6000-tcp.md) for the transport and qualification decisions.

Akamai RTX PRO 6000 overlays configure NCCL for TCP over `eth0`, disable InfiniBand and the external network plugin, and request standard `nvidia.com/gpu` resources. They do not request RDMA devices or add a GPU taint toleration: the inspected LKE GPU nodes have no GPU taint. The same transport settings apply to the three NCCL collectives, Nemotron 5 8B, and MPI or Torch WorkloadRuns.

Set `gpusPerNode: 1` explicitly for the one-GPU development VMs. The architecture default remains eight GPUs per node for production shapes. Multi-GPU renders set `NCCL_P2P_DISABLE=1` until the VM PCIe peer-to-peer paths are qualified; one-GPU renders leave P2P settings unset. These overlays have no measured performance baseline yet. The hardware-qualified run used two nodes with one GPU each; multi-GPU nodes are not yet qualified.

The generic NCCL defaults (`maxBytes: 16G`, `numIterations: 100`,
`numCycles: 10`) are too large for the observed Akamai TCP bandwidth to finish
within the one-hour launcher timeout. Set all three options explicitly for
Akamai all-reduce, all-gather, and alltoall. This bounded configuration matches
the completed two-node all-reduce run:

```yaml
spec:
  gpusPerNode: 1
  nodesPerJob: 2
  maxBytes: 1G
  numIterations: 20
  numCycles: 1
  categories:
    - domain: communication
      variant: nccl-all-reduce
```

Use the same bounds when selecting `nccl-all-gather` or `nccl-alltoall`;
those collectives still require hardware validation. Increase the workload
only after measuring its duration on the target network. The completed
all-reduce run checked sizes from 8 bytes through 1 GiB with zero correctness
errors; no performance threshold was enforced.

## Host RDMA device access

Requesting no NIC resource leaves the workload with no RDMA access at all.
That is the right default where the site runs a device plugin and just needs
to name it, but some clusters expose the Mellanox devices only as host device
nodes under `/dev/infiniband`, with no SR-IOV policy and no plugin to request.
OCI GB200 shapes are the common case.

For those, set `rdmaDeviceAccess: hostPath` on the Certification (or on one
category's `options`). It mounts the host's `/dev/infiniband` into the workload
container and runs that container privileged:

```yaml
spec:
  rdmaDeviceAccess: hostPath
```

The mount and the privileged flag are one unit rather than two independent
choices. Kubelet builds a container's device cgroup allowlist from the
resources it requests, so a `hostPath` mount on its own makes
`/dev/infiniband/uverbs0` visible while `open()` on it still fails with
`EPERM`. No capability changes that, including `IPC_LOCK`, which the
communication entries already grant.

The option applies on every platform and architecture, because how a cluster
plumbs RDMA is a property of the cluster rather than of its cloud. Leaving it
unset, or setting it to `devicePlugin`, renders exactly what it rendered
before: the NIC resource request described above and nothing else. Workload
pods will be rejected in namespaces that enforce a `restricted` or `baseline`
[Pod Security Standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/),
which forbid privileged containers. See
[ADR-088](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/088-oci-gb200-roce-and-rdma-device-access.md).

A privileged container is root-equivalent on the node. It runs without the
runtime's default seccomp filter and without AppArmor or SELinux confinement,
so the pod-level `seccompProfile: RuntimeDefault` the training entries set no
longer constrains the worker container even though the pod spec still shows
it. The `/dev/infiniband` mount names the devices the workload uses; it is not
what limits access. Treat the option as granting the workload root on the
node, because that is what `privileged: true` does.

Some CRI-O runtimes reject a privileged container whose GPUs come from the
NVIDIA device plugin, on the grounds that the device paths are already present
on the host. The catalog records one such configuration: the OCI arm of the
DCGM entry turns `privileged` off for exactly that reason. Every container this
option patches requests `nvidia.com/gpu`, so if your runtime enforces that
rule, the pods fail to start rather than running without the mount. Try one
category before turning the option on for a whole certification.

The value is case-sensitive and is validated by `nvcrectl` as well as by the
API server, so `rdmaDeviceAccess: HostPath` is an error rather than a render
that silently omits the mount.

### When a node has no RDMA devices

The volume is declared `type: Directory`, so kubelet refuses to start the pod
on a node where `/dev/infiniband` does not exist rather than mounting an empty
directory that NCCL would later report as "no devices found". Kubelet retries
the mount indefinitely, so the pod stays in `ContainerCreating` with a
`FailedMount` event naming the path.

`timeoutPerJob` bounds that wait and is the only thing that does. The
communication entries time the Job out after one hour by default; the
training entries after 24 hours, a backstop chosen because a healthy training
run can legitimately exceed an hour. In both cases the Workflow reports the
failure. Set `spec.timeoutPerJob` (or `categories[].options.timeoutPerJob`)
to tighten either bound; on a training category that is the difference
between learning about a missing `/dev/infiniband` in minutes and in a day.
Watch for a pod stuck in `ContainerCreating` with a `FailedMount` event, and
try one category before enabling the option across a whole certification.

`startupStallTimeoutSeconds`, which the training entries do set, does **not**
cover this case on any entry. Startup-stall detection is anchored on the
GoodputMeasurement, and the GoodputMeasurement only starts sampling once
worker-0 reaches `Running`, which a pod held in `ContainerCreating` never
does.

## On-prem clusters

`onprem` is the detection fallback: a node with an empty `spec.providerID` (and no Forge hostname or NKE site-name label) resolves to the `onprem` platform. Nodes provisioned by bare-metal stacks such as BCM typically carry no providerID, so they land here without any configuration.

For GB200/GB300 (NVL72) targets, the on-prem override contributes:

- **Tolerations** for the `kubernetes.io/arch=arm64:NoSchedule` and `nvidia.com/gpu=present:NoSchedule` taints common on NVL72 deployments, on the MPI launcher pod as well as the node pods. Without them, workload pods never schedule on tainted arm64 nodes.
- **A portable InfiniBand NCCL environment** without HCA pinning: NCCL auto-detects HCAs when `NCCL_IB_HCA` is unset, so the same override works across sites with different HCA layouts.
- **A NIC resource request**, detected automatically or configured via `nicResourceName`. Resource names vary by RDMA device plugin (`rdma/ib`, `nvidia.com/mlnxnics`, and others are all in the wild), so when the field is unset the controller inspects the target nodes: if exactly one candidate resource (any `rdma/*` name, or the exact name `nvidia.com/mlnxnics`) is allocatable at the resolved `mlnxPerNode` count on every target node, that name is requested. Detection never guesses or over-commits: with zero candidates, with several, or when candidates exist but no node set can cover the requested count, no NIC resource is requested (pods still schedule, without an explicit NIC allocation) and a Normal `NICResourceDetection` event on the Certification or WorkloadRun explains what was found, naming the requested count when candidates fall below it. Set `nicResourceName` to override detection or to resolve an ambiguous fleet. The per-container count always comes from `mlnxPerNode`, detected or not. GB200, GB300, B200, and B300 default `mlnxPerNode` to 8; sites running a shared-device plugin (one pooled resource per pod) should set `mlnxPerNode: 1`; with the default of 8, a pooled resource advertised as `rdma/ib: 1` is not detected, because the resulting request could never schedule.

For x86 HGX B200/B300 targets, the on-prem override is deliberately smaller: a toleration for the `nvidia.com/gpu` taint with any value (`operator: Exists`, since generic on-prem fleets do not agree on `present`), applied to the MPI launcher as well as the node pods, and the same NIC resource request, detected or set via `nicResourceName`, with the count from `mlnxPerNode`. It sets no NCCL environment: an HGX chassis has no multi-node NVLink, so the NVL72 tuning does not apply, and fabric-specific tuning (RoCE GID index, HCA selection) waits on multi-node validation. Sites running a shared RDMA device plugin (one pooled resource such as `rdma/rdma_shared_device_a` advertised at a large count) should set `mlnxPerNode: 1`; the default of 8 schedules, but consumes eight quota units for the same device access. On a WorkloadRun, `spec.resources` and the NIC request merge by resource name and the NIC request's count wins, so when both name the same resource, `mlnxPerNode` sets the count and `spec.resources` should carry only other resources. See [ADR-094](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/designs/094-hgx-b300-architecture-and-onprem-nic-override.md).

An architecture missing from the catalog's GPU defaults table (`gpu-defaults.yaml`) has no node shape and falls back to four GPUs and no NICs unless `gpusPerNode` and `mlnxPerNode` are set. The controllers emit a Warning event, reason `GPUArchitectureDefaults`, on the Certification or WorkloadRun whenever the detected architecture is missing from the table, naming the fallback and the counts the run is actually sized at; every `nvcrectl` render path and `nvcrectl cluster info` print the same message to stderr. Set `gpusPerNode` and `mlnxPerNode` explicitly to correct the counts, and file an issue so the architecture can be added.

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
**Metal3 detection trap.** Nodes with a `metal3://` providerID resolve to the `mistral` platform, not `onprem`. An on-prem site provisioned by Cluster API + Metal3 therefore silently picks up the Mistral site-specific override, which pins HCA names (`NCCL_IB_HCA`, `UCX_NET_DEVICES`) and the `rdma/ib` resource name. To preview what a generic on-prem render looks like regardless of providerID, pass the platform explicitly: `nvcrectl certification render --platform onprem <cert.yaml>`. An x86 HGX B200/B300 site provisioned the same way matches neither the Mistral block (gated on GB300) nor the HGX block (gated on `onprem`), so the controller applies no toleration and no NIC request there.
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
