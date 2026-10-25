# ADR-109: AWS RTX PRO 6000 workload overlays

> **Status:** Accepted design; communication and training smoke validation
> completed on two g7e nodes. Merge and release remain pending.
> Approved by the engineer on 2026-10-20.

Issue: https://nvidia.atlassian.net/browse/NKX-18019
Inspected upstream commit: `765ff4a` (2026-10-20).

## Context

NVCRE already registers AWS and the RTX PRO 6000 architecture. The architecture
defaults to eight GPUs and zero Mellanox resources per node. Neither needs
registration changes for this issue.

The three NCCL communication entries and NCCL loopback have an AWS override
whose GPU predicate excludes only GB200/GB300. It therefore catches RTX PRO
6000 and injects the H100 EFA image and 32 EFA devices per worker. Rendering
the official checksum-verified v0.8.1 CLI confirms this for all-reduce,
all-gather and all-to-all. A successful render is not evidence of g7e support.

AWS documents four network cards for g7e.48xlarge, one EFA/EFA-only interface
per card, and EFA/RDMA support. AWS recommends at least CUDA 12.9, EFA installer
1.45.0, NCCL 2.28.3 and EFA device plugin 0.5.10 for G7e. The current H100
workload image embeds CUDA 12.8.1, EFA 1.43.2 and NCCL 2.27.7, below those
recommendations. GPU architecture alone does not establish the attached
interfaces or software versions of NKX's g7e nodes.

EC2's instance-type API independently confirms eight 96 GiB GPUs, four network
cards and at most four EFA interfaces on g7e.48xlarge. The diagnostic cluster's
reviewed launch template attaches one EFA interface on card zero and one
EFA-only interface on each of cards one through three. Actual node resources,
driver/plugin versions and the communication transport still require live
verification. Reservation identifiers, account details and temporary access
configuration belong in the private operational record, not this ADR.

## Decision

Add an explicit AWS × RTX PRO 6000 cell based on observed NKX nodes. Keep
hardware discovery as a prerequisite to committing transport settings. Scope
this issue to the overlay and its validation; leave measured bandwidth floors
to NKX-18020 and Nemotron qualification thresholds to NKX-18021.

1. Prepare a two-node g7e diagnostic cluster from the B40 qualification pool.
   Reuse those nodes for the dependent qualification measurements. Reserve two
   slots for a bounded session, and verify the reservation AZ before provisioning. Prepare the
   cluster with manual Terraform apply, review the plan, and establish the
   compatible released runtime. Confirm deletion/cleanup before lease expiry
   so expired accounting does not leave running instances behind.
2. Inspect both nodes: eight allocatable GPUs, product labels and taints,
   network interfaces and EFA allocatable resource counts, GPU driver, EFA
   device plugin, host software and networking. Verify GPU-to-GPU PCIe P2P and
   intended inter-node transport in diagnostic output. Do not assume the GCP
   eth0/TCP profile or the H100 EFA count is correct.
3. If the nodes expose the expected four EFA devices, use a dedicated g7e
   EFA profile and compatible pinned workload image. If NKX provisions TCP-only
   nodes, record that finding and adjust the proposal explicitly before
   implementation; do not silently call TCP fallback an EFA success.
4. Keep all AWS RTX overrides jointly scoped by `platform: aws` and
   `gpuArchitecture: rtxpro6000`. Exclude RTX from the broad H100 NCCL/loopback
   overrides so foreign dependencies cannot survive alongside the new cell.
   Preserve existing H100/GB200/GB300 and GCP RTX behavior.
5. Cover NCCL all-reduce, all-gather, all-to-all and supported loopback entries;
   cover Nemotron 5 8B so the dependent AWS training configuration has a real
   overlay. Include 56B only when its resource/image requirements are verified;
   otherwise state the supported scope explicitly. Use framework-specific
   runtime patches and the same shared transport settings in the applicable
   Certification and WorkloadRun paths.

## Implementation

After design approval and node inspection:

- Add dedicated `pkg/catalog/entries/_lib/nccl/aws-rtxpro6000-*-env.yaml` and
  communication/training runtime patches under `_lib/deps/`.
- Add explicit cell overrides to the communication and training entries above.
  Forward MPI variables with `-x`; launcher container env alone does not reach
  worker ranks. Preserve complete args lists when replacing atomic arrays.
  Compose env fragments without duplicate variable ownership (ADR-100).
- Add framework-appropriate overrides in
  `pkg/platform/overrides/workloadrun.yaml`, preserving a caller's workload
  image while documenting its required EFA/CUDA stack for EFA runs.
  Request `IPC_LOCK` on each EFA worker; preserve MPI's `SYS_CHROOT` when
  restating capabilities. Keep capabilities out of the shared resource fragment.
- Add `pkg/render/nodes/aws-rtxpro6000.yaml` from observed node facts, plus
  code-grounded documentation of the supported cell and software requirements.
- Request one EFA interface per pair of selected GPUs, rounded up, through
  the shared resource fragment: one/two/four interfaces for two/four/eight
  GPUs. EC2 reports one EFA interface for g7e.8xlarge and g7e.12xlarge, two
  for g7e.24xlarge, and four for g7e.48xlarge. AWS lists one/two/four/eight
  GPUs respectively for those EFA-capable sizes, so the request stays within
  the interface limit when `gpusPerNode` stays within physical GPU capacity.
  Smaller shapes need an explicit `gpusPerNode` override because the RTX
  architecture default remains eight. The g7e.2xlarge and g7e.4xlarge sizes
  have no EFA support. A positive resource request requires the device plugin
  to advertise sufficient capacity; no TCP fallback is provided. Smaller
  shapes and partial GPU allocations have render checks but no live
  performance qualification.
- Add TestCaseParser unit/integration cases for eight GPUs, the observed
  networking count, MPI and torch paths, and absence of H100's 32-device patch.
  Include regression renders for AWS H100/GB200/GB300 and GCP RTX.
- In the AWS RTX 8B runtime only, order Megatron core_v0.15.2's DDP
  initialization stream before parameter copies and before optimizer setup.
  Apply this two-wait backport in an init container after cloning Megatron.
  Guard changes to training.py with SHA-256 checks. Leave another source
  unchanged only when its `get_model` AST matches the inspected synchronized
  implementation, including wait placement around DDP construction; otherwise
  fail initialization. Retain all parameter integrity assertions.
  Hold `/mnt/workspace/.megatron-lm.lock` to serialize against other patch
  guards, and read the source only after acquiring it. Workspace preparation
  takes the same lock in `megatron-clone`, added by
  [PR #555](https://github.com/NVIDIA/cluster-readiness-engine/pull/555),
  coordinating workspace preparation with patching.
  Write and sync a sibling temporary file, preserve permissions, then use
  `os.replace` so
  readers cannot observe a truncated module. Remove the temporary file on
  failure; bound the whole init command with a 1200-second timeout.
  Concurrency tests verify two guards apply the patch once and a replacement
  failure leaves the original source intact.
- Identify golden changes before regeneration: new AWS cell expected output,
  catalog override counts, and offline available-combination/error lists from
  adding the mock node. Regenerate only deliberately changed expectations
  after permission and review every diff.

## Rationale

An explicit cell stops GPU-family defaults from selecting a different
machine's fabric. Shared fragments prevent Certification and WorkloadRun
settings from diverging. Live inspection is necessary because instance-type
capabilities do not prove which interfaces NKX attaches or advertises.

## Consequences

The additional AWS cell can run 16 ranks over two eight-GPU nodes. No new
public API, dependency upgrade or global RTX default is intended. Supporting
this cell requires a compatible image and verified host stack; it must not
be presented as production-qualified solely from offline goldens.

The `nccl-loopback-nvswitch` entry has no dedicated AWS RTX overlay and
renders its generic base configuration. On g7e it can exercise PCIe, so a
passing result does not establish NVSwitch support. The validated EFA
loopback is `nccl-loopback`.

Nemotron 5 56B likewise has no AWS RTX override. With at least four eight-GPU
nodes, it renders the generic `nvcr.io/nvidia/pytorch:25.08-py3` configuration
without an EFA resource request or AWS RTX EFA transport settings. It does not
inherit the 8B image or DDP backport. This render is not validated AWS RTX
training support.

## Alternatives considered

- Copy GCP's TCP overlay: rejected without node evidence; would potentially
  discard AWS EFA/RDMA and produce misleading low performance baselines.
- Reuse H100 unchanged: rejected because the 32-device request is wrong for
  the documented g7e hardware and the image is below G7e recommendations.
- Set NIC defaults globally on RTX: rejected because GPU architecture spans
  clouds with different transports and resource types.
- Add validator thresholds first: rejected; no valid AWS measurements exist.

## Validation and completion

Render all changed categories and WorkloadRun frameworks; inspect transport,
image, eight GPU requests, node count, tolerations and applied overrides.
Run the repository's build, lint and unit/integration checks. Complete a
two-node live NCCL all-reduce with zero validation errors, retain input,
render, report and launcher logs, and verify the selected transport from logs.
Exercise the training overlay before claiming training support. Prepare a
fork PR only when requested; merging and release creation remain human gates.
NKX-18019 completes when the overlay is merged, included in a released
nvcrectl/controller, and all-reduce passes on two g7e nodes. Afterward, collect
the three independent baseline runs for NKX-18020 on this setup.

## Live verification notes (2026-10-20)

Both diagnostic nodes advertise eight RTX PRO 6000 Blackwell Server Edition
GPUs and four EFA devices. The host driver is 580.173.02. PCIe peer access is
available within the GPU pairs; this is not an NVLink machine. Steady-state
GPU nodes have no GPU-specific taint.

The initial pinned AWS candidate (CUDA 13.0.2 / EFA 1.48.0 / NCCL 2.30.4)
initializes the OFI/EFA transport but fails CUDA kernel lookup on SM120.
`cuobjdump` confirms its NCCL library contains only SM80, SM86, SM89, SM90,
SM100 and SM103 kernels. A software version above the G7e recommendation is
insufficient evidence of RTX support. The current AWS Dockerfile also omits
SM120 from both NCCL and nccl-tests compilation.

Rebuilding the candidate's existing pinned sources with
`NVCC_GENCODE="-gencode=arch=compute_120,code=sm_120"` produces SM120 kernels.
The official NVIDIA PyTorch 26.03 image passes CUDA and SM120 NCCL kernel
execution on the observed 580.173.02 host driver. It also passes the full
two-node, sixteen-GPU all-reduce through 16 GiB, with zero validation errors.
Logs explicitly select RDMA, provider efa, fabric efa-direct and four NICs.
Use this existing digest-pinned image and its bundled MPI binaries; no
derived image needs to be published. The 16 GiB out-of-place bus bandwidth
was 33.36 GB/s in this diagnostic run; this is not a qualification threshold.
All-gather and all-to-all also pass through 16 GiB with zero validation
errors. EFA loopback passes separately on both nodes. MPI, torch and exec
WorkloadRun smoke tests pass with the caller image and environment preserved;
the torch run also verifies data-parallel hash exchange across all 16 ranks.
The full lint, build and unit/integration suite passes (245 integration cases).

The initial 8B training attempt reaches CUDA/EFA initialization but fails
Megatron core_v0.15.2's data-parallel parameter-hash assertion with this newer
image. Investigation and the training fix are recorded below.

SSH-launched MPI ranks also need the selected image's PATH and library path
forwarded explicitly. The first live attempt established that the image's
container environment alone does not make libnccl visible to remote ranks.
This forwarding belongs to the catalog's image-specific MPI command and must
not overwrite the paths of caller-supplied WorkloadRun images.

## Training verification notes (2026-10-22)

On a fresh two-node cluster, the unmodified 8B workload reproduces the
parameter-hash assertion. Before hash exchange, several ranks have zeros in
the first 64 of 4096 normalization weights, which should all be ones. This
establishes parameter corruption before comparison, rather than a reason to
relax the integrity check.

Megatron core_v0.15.2 creates DDP on a new CUDA stream without ordering that
stream against parameter initialization or subsequent optimizer setup. This
matches upstream issue #2044 and the synchronization fixes discussed in
PR #2852. The AWS RTX runtime backports two stream dependencies: the DDP
stream waits for the current stream before construction, and the current
stream waits for DDP construction before continuing. The source and result
hashes pin the transformation to the inspected version. An alternative
source is left unchanged only when the entire `get_model` AST matches the
inspected synchronized implementation. Comments and formatting do not affect
that fingerprint; moved waits, conditional waits and calls elsewhere do.
This applies to mirrored and pre-seeded workspaces too.
Other source revisions require inspection; different upstream fixes are not
assumed equivalent. Other clouds and caller-provided WorkloadRun code receive
no source patch.

With these dependencies, all sixteen ranks retain identical, correct
normalization weights and pass the original parameter-hash assertion. The
two-node run completes ten training steps with no skipped or NaN iterations
and both workers exit zero. A diagnostic wrapper observes the parameters and
calls the unchanged integrity check. A normal Certification then exercises the guarded init container without
the diagnostic wrapper, passes the original integrity check and ten steps,
and reports `Succeeded=True` / `AllWorkflowsSucceeded`. Both workers exit zero
without restarts.

This is an 8B smoke test, not training qualification or a performance floor.
PyTorch reports an AccumulateGrad stream-mismatch warning with this dedicated
DDP stream; it is not suppressed. CUDA graph capture is outside this validated
configuration. NKX-18021 owns training qualification and thresholds.

## References

- https://nvidia.atlassian.net/browse/NKX-18019
- https://nvidia.atlassian.net/browse/NKX-18020
- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/using-eni.html
- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/gpu-instances-started.html
- https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/efa.html
- https://aws.amazon.com/ec2/instance-types/g7e/
- https://github.com/NVIDIA/Megatron-LM/issues/2044
- https://github.com/NVIDIA/Megatron-LM/pull/2852
- `docs/designs/031-platform-aware-nccl-config.md`
- `docs/designs/100-catalog-env-fragment-ownership.md`
- `pkg/catalog/entries/_lib/deps/aws-efa-runtime-patch-comm.yaml`
- `pkg/platform/overrides/workloadrun.yaml`

- https://github.com/awslabs/awsome-distributed-ai/blob/main/micro-benchmarks/nccl-tests/nccl-tests.Dockerfile
