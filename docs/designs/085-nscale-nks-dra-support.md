# ADR-085: Nscale NKS Support on the DRA Stack

> **Status:** Accepted

## Context

The in-tree Nscale integration (`platform: nscale`, added alongside the OCI and Mistral
overrides) targets a service Nscale has deprecated. It detects Nscale as a variant of
OpenStack — `openstack://` providerID plus an `nscale.com/rdmashare` allocatable to
disambiguate from generic on-prem OpenStack (`pkg/controller/workflow_detect.go:61-65`) — and
renders an `rdma/shared`-style device-plugin request (`pkg/catalog/entries/_lib/deps/nscale-rdmashare-comm.yaml`)
tuned with pinned HCA names for a RoCE bond fabric
(`pkg/catalog/entries/_lib/nccl/nscale-roce-env.yaml`).

I inspected a representative NKS cluster (k8s v1.36.4, 2 × B200 bare-metal GPU nodes plus 2
worker VMs) and confirmed that **every signal the current code depends on has changed**:

| Dimension | In-tree today | NKS today | Consequence |
|---|---|---|---|
| providerID | `openstack://` | `nscale://placement-server/<group>/<inst>-NNNNNN`, `nscale://instance/<uuid>` | No prefix match → detected as **`onprem`** |
| Discriminator | `nscale.com/rdmashare` allocatable | absent entirely | A corrected prefix alone still wouldn't disambiguate |
| RDMA | `nscale.com/rdmashare: "8"` | `rdma.nscale.com` **DeviceClass** (DRA, `dra.net`) | Unsatisfiable; pods Pending |
| GPUs | `nvidia.com/gpu: {{ .GpusPerNode }}` | `gpu.nvidia.com` **DeviceClass**; no `nvidia.com/gpu` allocatable | Unsatisfiable; pods Pending |
| GPU arch | `nvidia.com/gpu.product` label | **absent**; product only in ResourceSlice attrs | Arch `unknown` → `gpusPerNode` falls back to 4, actual 8 |
| Interconnect | RoCE (`^mlx5_bond`, `UCX_NET_DEVICES=mlx5_0:1..11`, 12 HCAs) | **InfiniBand** (`ipoib`, `ibs1`–`ibs8`, ConnectX-7, MTU 4092) | Wrong fabric, devices, count |

Rendering a certification with `--platform nscale` against this cluster does not merely
under-tune the fabric the way an unmatched onprem GB300 does (ADR-075) — the node isn't even
recognized as Nscale, so none of the existing Nscale override fires and detection instead
falls through to plain `onprem`.

Corroborating detail: `GPUCluster` has `devicePlugin: null`, `gfd: null`,
`dra.computeDomains.enabled: true` — no device plugin or GFD DaemonSet exists, which is *why*
both the allocatable and the product label are gone. `NicClusterPolicy` deploys only the
DOCA/OFED driver; the absent `rdmaSharedDevicePlugin` was the source of `nscale.com/rdmashare`,
so its removal is the deprecation concretely. `DeviceClass/rdma.nscale.com` is Helm-managed by
Nscale's own `nvidia-platform-0.2.0` chart. ComputeDomain is `resource.nvidia.com/v1beta1` —
already the version we emit (`pkg/catalog/entries/_lib/deps/gb200-compute-domain-and-dra-comm.yaml`).

Nscale also ships a GB300 SKU on the same stack, so the replacement has to cover B200 and
GB300 from one platform definition, not just the B200 hardware inspected above.

We already create `resource.k8s.io/v1` `ResourceClaimTemplate`s with a `deviceClassName`
(`_lib/deps/gb300-roce-comm.yaml`, for AWS GB300's `roce.networking.k8s.aws` DeviceClass) and
hold RBAC for `resourceclaimtemplates` and `computedomains`
(`helm/cluster-readiness-engine/templates/manager-role.yaml:114-135`). This is not new
machinery for NVCRE — Nscale is simply the first platform where GPUs themselves, not just the
NIC, are DRA-claimed.

CLAUDE.md requires a user-approved ADR before implementation, and this is a platform rewrite
spanning detection, catalog fragments, and RBAC. **This ADR covers only the design** — no
code, no catalog changes, no tests are part of this record. The implementation plan is a
separate exercise once this ADR is approved.

## Decision

1. **Detect Nscale from the `nscale://` providerID prefix alone.** Retire the
   allocatable-based disambiguation in `nodePlatform`
   (`pkg/controller/workflow_detect.go:61-65`); it existed only because `openstack://` was
   ambiguous between plain OpenStack on-prem and legacy nscale. NKS's own prefix removes the
   ambiguity outright, so the `openstack://` case goes back to unconditionally returning
   `onprem` and a new `nscale://` case is added to the same switch.

2. **Derive GPU architecture from `gpu.nvidia.com` ResourceSlices when the
   `nvidia.com/gpu.product` label is absent.** Read the `productName` device attribute from
   the node's `gpu.nvidia.com`-driven ResourceSlices and parse it with the same
   architecture-string rules `gpu.ParseProduct` already applies to the label
   (`pkg/gpu/product.go:12`), feeding the result into the same vote `gpu.MajorityArchitecture`
   performs today (`pkg/gpu/majority.go:33`). This is arch only, not count: `gpu-defaults.yaml`
   already has `b200: {gpusPerNode: 8}` and `gb300: {gpusPerNode: 4}`
   (`pkg/catalog/entries/_lib/gpu-defaults.yaml:32-43`), so deriving a count from ResourceSlice
   attributes would add a second source of truth for the same number with no gain. This needs
   new `resourceslices` (`resource.k8s.io`) read RBAC — nothing in the manager role grants it
   today.

3. **GPUs and RDMA as DRA claims**, deleting the base `nvidia.com/gpu` request on this
   platform. The Nscale override supplies `ResourceClaimTemplate`s for both the `gpu.nvidia.com`
   DeviceClass and the `rdma.nscale.com` DeviceClass, on the same shape as the existing AWS
   GB300 RoCE claim (`_lib/deps/gb300-roce-comm.yaml`), and removes `resources.limits/requests["nvidia.com/gpu"]`
   from the rendered container (base entry request at
   `pkg/catalog/entries/communication/nccl-all-reduce.yaml:53,55`) by nulling it in the
   fragment's TrainingRuntime patch: the request lives in the runtime dependency, not in
   `jobTemplate`, and `mergeMaps` deletes a key on an explicit null.

4. **Portable IB profile, no HCA pinning.** The DRA claim injects only the HCAs it actually
   claims, so pinning device names the way the legacy `nscale-roce-env.yaml` does
   (`^mlx5_bond` exclusion, explicit `mlx5_0..11`) recreates exactly the brittleness ADR-075
   rejected for generic on-prem IB. The new Nscale env fragment follows ADR-075's
   `onprem-ib-env.yaml` shape instead: fabric-portable NCCL/OMPI hygiene vars, no
   `NCCL_IB_HCA`/`UCX_NET_DEVICES` pin, and NCCL's documented auto-detection fills the gap.

   `NCCL_NET` is left unset. Pinning it to `IB` selects NCCL's built-in IB transport, which
   drops the HPC-X plugin's CollNet and with it SHARP: 4-node all-reduce busBW was 336 GB/s
   pinned and 549 GB/s unset. The retained trade-off is that a run whose HCAs are unusable
   falls back to NCCL's Socket transport over `eth0` rather than failing. Such a run lands
   far below any sensible busBW threshold, and recording the transport NCCL chose in
   `BandwidthMeasurement`, so a Socket fallback fails explicitly, is future work.

5. **Scope the new blocks to `platform: nscale` × `gpuArchitecture`, ordered after the
   arch-only GB200/GB300 ComputeDomain block, in all eight catalog entries.** Not only the
   three MPI collectives: the two loopback variants, `dcgm-level4` and both Nemotron training
   entries request `nvidia.com/gpu` the same way and are equally unschedulable on NKS without
   a claim. The existing GB200/GB300 block
   (`nccl-all-reduce.yaml:352-357` and the equivalent WorkloadRun block,
   `pkg/platform/overrides/workloadrun.yaml:8-13`) has no platform matcher and already fires on
   Nscale GB300 nodes; the new Nscale blocks are appended after it in every entry's
   `overrides:` list so their `trainer.args` / dependency replacements win, the same ordering
   discipline ADR-058 and ADR-075 both rely on.

6. **Remove the legacy Nscale path entirely rather than deprecate it.** No legacy Nscale
   clusters exist. Delete `_lib/deps/nscale-rdmashare-comm.yaml`, `_lib/nccl/nscale-roce-env.yaml`,
   the three legacy Nscale override blocks (`nccl-all-reduce.yaml:302-318`, and the equivalent
   blocks in `nccl-all-gather.yaml` and `nccl-alltoall.yaml`), the allocatable-check branch in
   `nodePlatform`, and the matching synthetic-node plumbing that exists solely to fabricate the
   `nscale.com/rdmashare` allocatable for offline rendering
   (`pkg/render/nodes.go:22-25`, `pkg/workloadrun/workloadrun.go:1316-1323`,
   `pkg/certification/certification.go:576-581`). `platform.NScale` and its entry in
   `platform.Names()` (`pkg/platform/platforms.go:26,32`) are kept — the name now means NKS on
   DRA, not the deprecated service — but every fragment and code path backing the old semantics
   is deleted, following ADR-066's precedent that this codebase removes cleanly rather than
   accreting a compatibility shim for an integration nothing runs.

7. **Add `--gpu-arch` to `certification render` and `workloadrun render`.** Both commands
   already accept `--platform` (`pkg/certification/certification.go:135`,
   `pkg/workloadrun/workloadrun.go:122`) but have no GPU-architecture override; the sibling
   `nvcrectl workflow render` command already has both flags for exactly this reason
   (`pkg/render/render.go:96-97`). Offline `certification render`/`workloadrun render` have no
   cluster to query ResourceSlices against, so without this flag an offline Nscale render can
   only resolve to `unknown` architecture and hard-fails the override match in decision 5.

8. **Per-node entries keep their tolerate-everything toleration.** `nccl-loopback`,
   `nccl-loopback-nvswitch` and `dcgm-level4` tolerate every taint in their base runtime
   (`tolerations: [{operator: Exists}]`). Toleration lists carry no `name`, so `mergeMaps`
   replaces them wholesale, and the shared fragment's arm64/GPU tolerations would silently
   narrow what those entries tolerate. The `nccl-loopback` block therefore lists a trailing
   `deps/tolerate-all-runtime-patch.yaml` after the Nscale fragment, restoring the base
   toleration (a strict superset of the keyed ones); `nccl-loopback-nvswitch` and
   `dcgm-level4` use a fragment that sets no tolerations at all (decision 9).

9. **Entries that use no NIC claim the GPU only, and `dcgm-level4` points `--host` at
   the NKS DCGM Service.** The dcgm pod runs `hostNetwork`, so an `rdma.nscale.com`
   (`dra.net`) claim has no pod network namespace to move a netdev into, and level-4
   diagnostics are intra-node. `nccl-loopback-nvswitch` exercises NVLink/NVSwitch only, so
   claiming the rail NICs would block concurrent NIC users and add DraNet's NIC-move timing
   to pod start for nothing. Both use a GPU-only fragment (`deps/nscale-gpu.yaml`).
   `nccl-loopback` keeps the RDMA claim: with SHM and P2P disabled its traffic crosses the
   NIC. NKS
   runs the DRA-flavoured DCGM hostengine as `nvidia-platform/nvidia-dcgm-dra` rather than the
   GPU Operator's `gpu-operator/nvidia-dcgm`, so the Nscale block's `jobTemplatePatch` replaces
   the `--host` value with `nvidia-dcgm-dra.nvidia-platform.svc:5555`. A leading `test` op
   pins the base value at that index, so reordering the base args fails the render instead of
   overwriting the wrong argument. The Service is `internalTrafficPolicy: Local`, so each diag
   reaches the hostengine on its own node, as the GPU Operator Service does elsewhere.

10. **GB300 on Nscale uses `topology.nks.nscale.com/accelerator-domain` as its topology key.**
    The arch-only GB200/GB300 blocks (`_lib/overrides/gb200-topology-key.yaml` in
    `dcgm-level4`, the ComputeDomain block in both Nemotron entries, and the intra-rack /
    diagnose `orchestration` in the three collectives) set `nvidia.com/gpu.clique`. That label
    is written by GFD, which Nscale does not run, so `partitionTopology` would fail every
    topology-mode Workflow with `missing topology label`. A `platform: nscale` ×
    `gpuArchitecture: gb300` block appended after each of those replaces the key with the
    NVLink-domain label NKS publishes. In the collectives the block is wrapped in the same
    `testScale` conditional as the base `orchestration`, because an orchestration override
    replaces the whole `topology`/`diagnose` object and must only be emitted when the base
    emits one (repeating `strictDomain` and `minGroupSize`). On Nscale B200 the intra-rack and
    diagnose scales inherit the collectives' arch-agnostic `nvidia.com/gpu.clique` key, so
    partitioning fails there as it does on any platform without GFD; this is pre-existing and
    equally true on AWS H100. Wiring B200 to `fabric-tier-0` (see Notes) stays future work.

11. **`OrchestrationOverrideSpec` gains an optional `diagnose` field.** Overrides could
    replace `topology` but not `diagnose`, so decision 10 was impossible for the diagnose test
    scale without a CRD change. The field mirrors `topology`: nil leaves the base value, non-nil
    replaces it wholesale (`mergeOrchestration`). Workflow and WorkloadRun CRDs regenerate.

12. **Nscale B200 NCCL collectives carry QP and channel tuning.** On Nscale B200,
    `nccl-all-reduce`, `nccl-all-gather` and `nccl-alltoall` set
    `NCCL_IB_QPS_PER_CONNECTION=8`, `NCCL_MIN_NCHANNELS=32` and `NCCL_MIN_CTAS=64`
    (`_lib/nccl/nscale-b200-ib-env.yaml`). Measured with `nccl-all-reduce` at 4 nodes, they
    lift busBW from 336 to 385 GB/s on Ring and from 549 to 580 GB/s with SHARP, matching
    Slurm burn-in on the same hardware. That evidence is all-reduce only; applying the
    profile to all-gather and alltoall is a deliberate choice, not a measurement. The
    catalog does not pin `NCCL_ALGO`: it certifies the algorithm NCCL picks, which is what
    users' jobs get. There is no node-count gate; whether the profile changes the
    single-node NVLS result is checked on hardware. The profile is B200-only because
    GB300 has not been measured (Note 6), and does not apply to the loopback entries,
    training or WorkloadRun. It follows the Azure + H100 layering: a `platform: nscale` ×
    `gpuArchitecture: b200` block after the Nscale block replaces `trainer.args` with the
    IB env followed by the tuning env, and adds no dependencies, because the Nscale block
    already supplies the claims. Folding the variables into `nscale-ib-env.yaml` instead
    would also apply them to GB300, the loopbacks, training and WorkloadRun.

## Implementation

**This risk is checked first, before any of the above is built**, because it can change
decision 3's shape: `mergeMaps` deletes a key when the override value is JSON `null`
(`pkg/controller/workflow_detect.go:657-661`), which is how the Nscale override is meant to
remove the base entry's `nvidia.com/gpu` request. That `null` has to survive the round trip
from catalog YAML through Go template rendering into `map[string]any` and back out through
`RawExtension` marshaling. If it doesn't — if the YAML parser or the `apiextensionsv1.JSON`
`RawExtension` layer normalizes an explicit null away before `mergeJobTemplate`
(`pkg/controller/workflow_detect.go:616-629`) ever sees it — then deletion-by-null isn't
available here, the GPU request can't be removed from the shared base entry, and it has to move
out of the base entry into every platform's override fragment instead, which is materially
larger than the single-line removal decision 3 assumes. The implementation PR verifies this
first with a throwaway override before writing any catalog fragment.

Once that's confirmed, implementation follows the shape ADR-075 and ADR-058 established:

- **Detection** (`pkg/controller/workflow_detect.go`): add the `nscale://` case to
  `nodePlatform`; delete the `openstack://` allocatable branch, reverting it to the bare
  `platformOnPrem` return already used by every other unmatched prefix.
- **GPU architecture from ResourceSlices**: a client-backed helper, parallel to ADR-075's
  `detectNICResource`/`resolveNICResourceName` (`pkg/controller/nic_detect.go`), that lists
  `gpu.nvidia.com` ResourceSlices for the target node set, reads `productName` per device, and
  feeds the parsed architecture into the same majority vote `detectGPUArchitecture`
  (`pkg/controller/workflow_detect.go:118-123`) already performs from labels. Wired at the same
  seams ADR-075 wired NIC detection: `createWorkflowForCategory`
  (`pkg/controller/certification_controller.go`) and `buildWorkflowSpec`
  (`pkg/controller/workloadrun_controller.go`), plus the CLI `--dry-run` paths. Offline render
  without `--dry-run` has no node list and falls back to the new `--gpu-arch` flag from decision 7.
- **RBAC**: add a `resource.k8s.io` / `resourceslices` rule (`get`, `list`, `watch`) to
  `helm/cluster-readiness-engine/templates/manager-role.yaml`, generated via
  `+kubebuilder:rbac` markers and `make manifests`, not hand-edited.
- **Catalog fragments** (`pkg/catalog/entries/_lib/`):
  - `deps/nscale-gpu-rdma.yaml`: two `ResourceClaimTemplate`s — `gpu.nvidia.com` and
    `rdma.nscale.com` — plus the `resourceClaims`/`resources.claims` wiring and the
    `nvidia.com/gpu: null` deletion in the TrainingRuntime, mirroring `deps/gb300-roce-comm.yaml`.
    The GPU count comes from `gpusPerNode` and the RDMA count from `mlnxPerNode`, the NIC-count
    knob other IB/RoCE platforms use, with an `nscale`/`gb300` `platformOverrides` entry of 4 in
    `gpu-defaults.yaml` (Note 3). At `mlnxPerNode: 0` the RDMA claim and its references are
    omitted rather than requested as count 0, the ADR-087 opt-out; the GPU claim is unaffected.
    It sets no `mlPolicy`, so it serves MPI and torch entries alike (no `-comm` suffix).
  - `deps/nscale-gpu-rdma-training.yaml`: `lib`-includes the fragment above and adds the IB env
    to the runtime container (merged by name into the base env), because torchrun inherits the
    container env and has no `mpirun -x` to ride on — the same split ADR-075 used.
  - `deps/nscale-gpu.yaml`: GPU-only claim for `dcgm-level4` and `nccl-loopback-nvswitch`
    (decision 9), no tolerations.
  - `deps/tolerate-all-runtime-patch.yaml`: platform-neutral patch restoring
    `tolerations: [{operator: Exists}]` (decision 8).
  - `nccl/nscale-ib-env.yaml`: unpinned IB env, mirroring `_lib/nccl/onprem-ib-env.yaml`.
  - `nccl/nscale-b200-ib-env.yaml`: B200 QP/channel tuning (decision 12).
- **Per-entry override blocks**, each appended last per decision 5:
  - `nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`: `nscale-gpu-rdma.yaml` +
    `trainer.args` replacement carrying the IB env as `-x` pairs; then, on B200, a block
    replacing `trainer.args` with the IB env plus `nscale-b200-ib-env.yaml` (decision 12);
    then the GB300 topology block inside the `testScale` conditional (decision 10).
  - `nccl-loopback`: `nscale-gpu-rdma.yaml` + `tolerate-all-runtime-patch.yaml`, and
    `trainer.env` replaced by the IB env plus `NCCL_SHM_DISABLE`/`NCCL_P2P_DISABLE`
    re-listed, since lists replace.
  - `nccl-loopback-nvswitch`: `nscale-gpu.yaml`, and `trainer.env` replaced by the IB env.
  - `dcgm-level4`: `nscale-gpu.yaml` and the `--host` patch; then the GB300 topology block.
  - `nemotron5-8b`, `nemotron5-56b`: deps-only `nscale-gpu-rdma-training.yaml`; then the GB300
    topology block.
  - `pkg/platform/overrides/workloadrun.yaml` carries the claim and env blocks; its GB200/GB300
    block sets no clique key, so it gets no topology block (see Notes).
- **API**: `Diagnose *DiagnoseSpec` on `OrchestrationOverrideSpec` and the matching branch in
  `mergeOrchestration` (decision 11).
- **CLI flags**: add `--gpu-arch` to `certification render` and `workloadrun render`
  following the existing `pkg/render/render.go:96-97` flag definitions and validation shape.
- **Removal**: delete `_lib/deps/nscale-rdmashare-comm.yaml`, `_lib/nccl/nscale-roce-env.yaml`,
  and the synthetic-allocatable plumbing named in decision 6.
- **Regenerate**: `make manifests generate` after the RBAC and (if any) CRD-adjacent changes.

### Testing plan

- **Render verification** per CLAUDE.md's nvcrectl render procedure: build `bin/nvcrectl`,
  create temp cert YAMLs for Nscale B200 and Nscale GB300, render with `--platform nscale
  --gpu-arch b200` / `--gpu-arch gb300`, and grep for the markers this override owns: the
  `gpu.nvidia.com` and `rdma.nscale.com` `ResourceClaimTemplate`s, the absence of
  `nvidia.com/gpu` in the rendered container's `resources`, the absence of `NCCL_IB_HCA`/
  `UCX_NET_DEVICES`, and — for GB300 — that the GB200/GB300 ComputeDomain block's claims and
  the Nscale RDMA claim coexist without name collision. Confirm the
  `nvcrectl.nvidia.com/applied-overrides` annotation lists the Nscale block and
  `detected-platform: nscale`.
- **testutil goldens**: cases under `pkg/platform/testdata/build-overrides/` (Nscale B200,
  Nscale GB300); render cases under `pkg/certification/testdata/certification-render-nscale/`
  covering every entry on both architectures — `nscale-{b200,gb300}-nccl` (all-reduce),
  `-nccl-scales` (all-gather at `intra-rack` and alltoall at `diagnose`, pinning the topology
  and diagnose keys: `gpu.clique` on B200 as the control, `accelerator-domain` on GB300),
  `-loopback` (both loopback variants: replaced `trainer.env`; the restored
  tolerate-everything toleration on `nccl-loopback`, the GPU-only claim on
  `nccl-loopback-nvswitch`), `-dcgm` (GPU-only claim, `--host` repointed at `nvidia-dcgm-dra`) and `-training` (both Nemotron
  entries: IB env in the runtime container env, cpu/memory surviving the GPU null,
  ComputeDomain + gpu/rdma claims on GB300). The projection records trainer args and env,
  runtime container env, tolerations, claims and the orchestration keys.
- **Integration cases** under `cmd/integration/testdata/reconcile/`:
  `certification-nscale-gb300-nccl` (the null-survival round trip),
  `certification-nscale-b200-dcgm` (hostNetwork entry with the GPU-only fragment),
  `certification-nscale-gb300-nemotron5-8b` (nodes labelled
  `topology.nks.nscale.com/accelerator-domain` and deliberately without `gpu.clique`, so a
  missed topology override fails at partition instead of reaching `JobRunning`; also the
  nested training fragment and container-env merge through the API server), and
  `certification-nscale-b200-loopback` (`trainer.env` replacement plus the toleration restore).
- **Detection goldens**: unit cases for the `nscale://` prefix match, the reverted
  `openstack://` → `onprem` path (no allocatable branch), and the ResourceSlice-based
  architecture vote (single product, mixed labeled/unlabeled nodes, no ResourceSlices present,
  malformed `productName`).
- **UAT**: new fixtures under `test/uat/testdata/nscale/{b200,gb300}/nccl/expected_pods.yaml`
  with a KWOK node profile carrying an `nscale://` providerID and `gpu.nvidia.com` /
  `rdma.nscale.com` DeviceClasses instead of allocatable resources, plus
  `TestNScaleB200NCCL`/`TestNScaleGB300NCCL` mirroring the existing per-CSP tests.
- **Removal check**: no `nscale.com/rdmashare` or `nscale-roce-env` string remains anywhere
  under `pkg/` or `docs/` once implementation lands.
- **`make verify-doc-links`** and **`make lint`** stay green with this ADR added — no code is
  touched by this record, so neither should report anything new.

## Rationale

- **One prefix removes an ambiguity that only existed because of the old service.** The
  `openstack://` + allocatable disambiguation was a workaround for Nscale sharing a providerID
  scheme with generic OpenStack. NKS's own `nscale://` prefix is unambiguous on its own; keeping
  the old disambiguation logic around would be dead weight once no allocatable is left to check.
- **Architecture detection needs a cluster-visible fallback, not a heavier default.** Falling
  back to "unknown" already silently mis-sizes GB300 at 4 GPUs when the real count is 8. Reading
  the same fact (`productName`) the label would have carried, from the place NKS actually
  publishes it, keeps `gpu-defaults.yaml` as the single source of truth for the *count* while
  fixing the *architecture* miss at its source.
- **DRA claims are the correct primitive here, not a new special case.** NVCRE already emits
  `ResourceClaimTemplate`s with a `deviceClassName` for AWS GB300 RoCE. Extending that same
  mechanism to GPUs themselves is the natural consequence of Nscale removing its device plugin,
  not a new integration pattern.
- **Unpinned IB is the only choice consistent with existing precedent.** ADR-075 arrived at the
  same conclusion for generic on-prem GB300 IB, for the same reason: HCA names are a per-site
  (here, per-fleet) fact, and NCCL's own documented behavior with `NCCL_IB_HCA` unset is to
  auto-detect.
- **Ordering after the GB200/GB300 block, not instead of it, avoids duplicating ComputeDomain
  logic.** The existing block already contributes the ComputeDomain and its DRA channel
  regardless of platform; Nscale only needs to add the GPU/RDMA claims and fabric env on top,
  the same layering ADR-075 used for onprem tolerations and env over that same base block.
- **Removal over deprecation matches ADR-066's precedent and this repo's stated practice**:
  carrying a second, permanently-unused code path (here, for a service that no longer exists)
  is a worse trade than the one-line detection consequence in Consequences below.

## Consequences

- **Breaking behavior change for any `openstack://` node that used to resolve to `nscale`.**
  After this change such a node detects as `onprem`, the same outcome any other unrecognized
  providerID gets today. No such cluster is known to exist; this is stated as a fact, not
  mitigated.
- **CRD/RBAC surface grows by one rule** (`resourceslices`, read-only) and the manager needs it
  wherever architecture detection can run against a live cluster (both controllers, both
  `--dry-run` CLI paths). The Workflow and WorkloadRun CRDs also gain one optional override
  field (`overrides[].orchestration.diagnose`, decision 11).
- **A GB300 NKS cluster whose nodes lack `topology.nks.nscale.com/accelerator-domain` fails
  every topology-mode Workflow** (intra-rack and diagnose collectives, `dcgm-level4`, both
  Nemotron entries) with `PartitionError: node … missing topology label`. This is stated, not
  mitigated: the alternative — silently falling back to name-sorted partitioning — would
  certify MNNVL placement that never happened. Full-scale collectives and the loopbacks set no
  topology key and are unaffected.
- **`dcgm-level4` on NKS depends on the `nvidia-platform/nvidia-dcgm-dra` Service** that NKS
  ships, not the GPU Operator's `gpu-operator/nvidia-dcgm`. `nvcrectl setup status` still
  checks only the latter, so on NKS it reports DCGM missing even when `dcgm-level4` can run.
- **Training entries inherit `NCCL_SOCKET_IFNAME=eth0`** from `gb200-training-base-env.yaml`
  unchanged, as the on-prem override does; the IB env is appended, not substituted.
- **Catalog fragment count is roughly flat**: two legacy fragments (`nscale-rdmashare-comm.yaml`,
  `nscale-roce-env.yaml`) are deleted and two to three new ones are added (GPU+RDMA claims, IB
  env, possibly a training variant), so this is a swap, not net growth, beyond the shared
  ComputeDomain machinery that already exists.
- **Offline render diverges from a live cluster whenever GPU architecture detection would
  fire.** `certification render`/`workloadrun render` without `--dry-run` has no node list, so
  `--gpu-arch` is mandatory for a correct Nscale render; the same certification applied to a
  live Nscale cluster resolves architecture from ResourceSlices instead. This is the same
  offline/`--dry-run` gap ADR-075 already documents for NIC-resource auto-detection.
- **If the `mergeMaps` null-survival risk in Implementation resolves unfavorably**, decision 3's
  GPU-request removal moves from the base entry into every platform-specific fragment that
  needs it, which is a materially larger diff than assumed here; that outcome would be reported
  back before the catalog fragments are written, not discovered after.
- **A pre-existing latent bug, exposed by Nscale but not specific to it, gets fixed as part of
  this work and widens the diff beyond Nscale's own files.** `buildConfigFromFlags`
  (`nvcrectl certification run --category`) persists the GPU product it discovers at creation
  time directly into the Certification's `spec.target.nodeSelector`, alongside
  `nvidia.com/gpu.present`. That selector drives every future reconcile's node discovery, so
  baking in a point-in-time detected value ties all future discovery to whatever a node
  happened to report at creation. On every platform shipped before Nscale this was silently
  safe, because the value written was always true of the real nodes. Nscale is the platform
  where it stops being safe outright: its GPUs are DRA-claimed with no device plugin/GFD to
  ever write `nvidia.com/gpu.product`, so a Certification created against a live Nscale cluster
  would persist a selector its own nodes can never satisfy again. The fix drops
  `nvidia.com/gpu.product` from the persisted selector, keeping only `nvidia.com/gpu.present`;
  `DiscoverGPUNodes` already requires every discovered node to report the same product before
  reaching this code, so `gpu.present` alone reselects the identical homogeneous set on every
  platform. Because `buildConfigFromFlags` is the one code path every platform's
  `certification run --category` goes through, this fix changes the persisted `nodeSelector`
  shape uniformly, not just for Nscale — every existing UAT fixture that asserts a
  `Certification`'s full `nodeSelector` (one per platform, via
  `test/uat/testdata/*/*/nccl/expected_certification.yaml`) loses its `nvidia.com/gpu.product`
  line as a direct consequence, independent of anything else this ADR changes.

## Alternatives Considered

- **`nscale-legacy`: keep the old `platform: nscale` behavior under a new name, add
  `nscale-nks` alongside it.** This would work — the old and new signal sets are fully
  disjoint (providerID scheme, discriminator, resource model, and label are all different), so
  there's no detection collision. Rejected because nothing runs the old stack: it would put a
  permanent name in `platform.Names()` backed by fragments that can never be validated against
  real hardware again, for a service Nscale itself has deprecated. Decision 6's clean removal
  costs one documented detection change (the `openstack://` fallback) against carrying that
  dead weight indefinitely.
- **Detect Nscale from GPU/RDMA DeviceClass presence instead of the providerID prefix.**
  Rejected as the primary signal: DeviceClasses are cluster-scoped and would require an extra
  cluster read on every detection call where the providerID is already in hand on the node
  object being inspected. The `nscale://` prefix is sufficient and free.
- **Derive GPU count as well as architecture from ResourceSlices.** Deferred, per decision 2:
  `gpu-defaults.yaml` already carries the correct count for both known architectures, and
  deriving it a second way from ResourceSlice attributes buys nothing over the existing default
  while adding a second source of truth to keep in sync.

## Notes

Carried here as explicit unknowns with a validation gate, following ADR-075's "a field
contribution with real-hardware validation is expected":

1. **GB300 `productName` string is assumed, not observed.** The B200 cluster inspected for this
   ADR reports its product via ResourceSlice attributes; the equivalent GB300 string has not
   been observed on real NKS hardware. `architecture: Blackwell` is shared between B200 and
   GB300 in the attributes seen so far, so that attribute alone cannot disambiguate — the
   architecture-derivation logic in decision 2 must key on `productName` specifically, and the
   exact GB300 value needs confirmation before the parser's mapping table is finalized.
2. **GB300 taints are unconfirmed.** The inspected B200 nodes carry none. GB300 is presumably
   arm64 Grace, like the on-prem GB300 case ADR-075 covers; if NKS GB300 nodes are tainted, the
   Nscale override block needs the same tolerations ADR-075 adds for generic on-prem.
3. **GB300 RDMA count per node — resolved.** GB300 NKS nodes advertise 4 `rdma.nscale.com`
   devices in their ResourceSlices. That count is recorded as the `nscale`/`gb300`
   `mlnxPerNode` override in `gpu-defaults.yaml`, which sizes the claim.
4. **`dra.net/state` was `down` on 7 of 8 compute HCAs** on the inspected node (only `ibs5` was
   `up`). Likely unconfigured IPoIB netdevs over an otherwise healthy link, but this should be
   confirmed before any bandwidth figure from that cluster is treated as a baseline.
5. **Kubeflow Trainer / JobSet is not installed on the inspected cluster**, which is a
   deployment prerequisite but gates end-to-end validation of this design.
6. **GB300 NCCL tuning is deferred until hardware is measured.** GB300 NVL72 runs
   multi-node collectives over MNNVL inside a rack and reaches IB only between racks, so
   decision 12's B200 numbers do not carry over.

**Historical citation note.** ADR-075 cites Nscale's allocatable-based detection as precedent
for its own NIC-resource auto-detection (`075-onprem-gb200-gb300-override.md`, Alternatives
Considered, citing `pkg/controller/workflow_detect.go:61-65`). Decision 6 here deletes that
code. ADR-075 is not edited — it is an accepted record of a decision made at the time — but the
citation is now historical: the pattern it pointed to no longer exists once this ADR is
implemented.

**RoCE is deferred, and the deferral has a cost worth stating now.** Fabric is currently
*implied* by `platform × arch` across the catalog — hence "AWS + H100 (EFA)", "OCI (RoCE)", and
now "Nscale (InfiniBand)". Nscale is the first platform where that implication would break on
its own: it exposes fabric encapsulation as a `dra.net` device attribute rather than a fixed
platform choice, so a future Nscale SKU on RoCE instead of IB cannot be told apart by
`platform × gpuArchitecture` alone. `WhenSpec` has no fabric axis today and the CEL override
environment exposes only five variables (`pkg/controller/workflow_detect.go:481-488`), so
"Nscale on RoCE" would need a new `interconnect` axis threaded through `OverrideContext` and
`WhenSpec`. It is detectable without new cluster plumbing beyond what decision 2 already adds:
among `dra.net` devices with `rdma == true`, take the majority `encapsulation` attribute. This
ADR defers that work; the Nscale blocks it adds get re-scoped once RoCE support actually lands.

**Topology labels.** Nscale publishes `topology.nks.nscale.com/*` node labels:
`accelerator-domain` (NVLink/MNNVL scope),
`fabric-tier-0` (leaf switch *group* — a node label can't name one switch, since each node
touches one leaf per rail), `fabric-tier-1` (spine), and `fabric-tier-2` (super-spine). The
mapping a future design should use: **B200 → `fabric-tier-0`** (no NVLink beyond the node, so
the leaf group is the finest real topology boundary — analogous to AWS's
`network-node-layer-1`); **GB300 → `accelerator-domain`** (MNNVL must stay inside one NVLink
domain, which outranks network locality). Tiers 1 and 2 are explicitly **not** usable as a
`topologyKey` — recorded here so a future reader doesn't reach for the wrong one. Decision 10
wires the GB300 → `accelerator-domain` half of this mapping wherever the catalog already sets a
clique key. Still unwired: a B200 → `fabric-tier-0` key (B200's intra-rack and diagnose scales
inherit the collectives' arch-agnostic `gpu.clique` key, so they fail partitioning on Nscale as
on any platform without GFD, a pre-existing gap also true on AWS H100), and a GB300 topology block in
`pkg/platform/overrides/workloadrun.yaml` alongside the AWS/GCP/Azure ones (the WorkloadRun
GB200/GB300 block sets no clique key). Validation gate: the `accelerator-domain` label must be
observed on real GB300 NKS nodes before topology-mode results from such a cluster are trusted.

## References

- ADR-012: Platform and GPU Architecture Overrides — override semantics and the `onprem`
  fallback this design's detection change interacts with.
- ADR-031: Platform-Aware NCCL Communication Benchmark Configuration — the `platform`/
  `gpuArchitecture` override axis this design extends to nscale.
- ADR-046: Shared Template Library for Catalog Entries — the `_lib/` fragment convention the new
  Nscale GPU/RDMA and IB-env fragments follow.
- ADR-058: Mistral GB300 SKU Support (InfiniBand) — precedent for platform-specific IB
  overrides and override-ordering discipline.
- ADR-075: On-Prem GB200/GB300 Override (Generic NVL72 Bare Metal) — direct precedent for
  unpinned IB env, tolerations-as-data, and client-backed auto-detection wired at the same
  controller/CLI seams this design reuses for GPU-architecture detection; also the source of
  the citation corrected in Notes.
- ADR-066: Remove the `kubeJob` Workload Type — precedent for removing an unused/deprecated
  path cleanly instead of accreting a compatibility shim.
- Code citations:
  - `pkg/controller/workflow_detect.go:41-76` (`nodePlatform`, including `:61-65` the
    allocatable-based Nscale branch this design removes, `:71-72` `metal3://` → `mistral`),
    `:105-123` (`nodeGPUArchitecture`/`detectGPUArchitecture`), `:481-488` (CEL override
    environment variables), `:616-629` (`mergeJobTemplate`), `:651-665` (`mergeMaps`, the
    null-deletes-key behavior decision 3 depends on).
  - `pkg/gpu/product.go:12` (`ParseProduct`), `pkg/gpu/majority.go:33` (`MajorityArchitecture`).
  - `pkg/catalog/entries/_lib/gpu-defaults.yaml:32-43` (`b200`/`gb300` `gpusPerNode` defaults).
  - `pkg/catalog/entries/_lib/deps/gb300-roce-comm.yaml` (DRA `ResourceClaimTemplate` +
    TrainingRuntime `resourceClaims` shape to mirror).
  - `pkg/catalog/entries/_lib/deps/nscale-rdmashare-comm.yaml`,
    `pkg/catalog/entries/_lib/nccl/nscale-roce-env.yaml` (legacy fragments removed by
    decision 6).
  - `pkg/catalog/entries/communication/nccl-all-reduce.yaml:53,55` (base `nvidia.com/gpu`
    request), `:302-318` (legacy Nscale block), `:352-357` (GB200/GB300 ComputeDomain block).
  - `pkg/platform/overrides/workloadrun.yaml:8-13` (GB200/GB300 block to order after).
  - `pkg/platform/platforms.go:18-33` (`NScale` constant, `Names()`).
  - `pkg/render/nodes.go:22-25`, `pkg/workloadrun/workloadrun.go:1316-1323`,
    `pkg/certification/certification.go:576-581` (synthetic-node plumbing for the legacy
    allocatable, removed by decision 6).
  - `pkg/certification/certification.go:135`, `pkg/workloadrun/workloadrun.go:122` (existing
    `--platform` flags), `pkg/render/render.go:96-97` (existing `--platform`/`--gpu-arch` flags
    on `nvcrectl workflow render`, the shape decision 7 adds to the other two commands).
  - `helm/cluster-readiness-engine/templates/manager-role.yaml:114-135` (existing
    `resourceclaimtemplates`/`computedomains` RBAC; `resourceslices` is added alongside it).
