# ADR-075: On-Prem GB200/GB300 Override (Generic NVL72 Bare Metal)

> **Status:** Accepted

## Context

Nodes provisioned by bare-metal stacks (for example BCM) carry no `spec.providerID`, so platform detection resolves them to `onprem` (`pkg/controller/workflow_detect.go:43-50`; nodes whose hostname or NKE site label marks them as Forge are detected separately). There is no `platform: onprem` matcher anywhere in the catalog entries, and none in the WorkloadRun override matrix (`pkg/platform/overrides/workloadrun.yaml`). The only override that applies to an on-prem GB200/GB300 target today is the platform-agnostic GB200/GB300 ComputeDomain block (`pkg/catalog/entries/communication/nccl-all-reduce.yaml:352-357` for the catalog, `pkg/platform/overrides/workloadrun.yaml:8-13` for WorkloadRun).

Rendering a GB300 certification with `--platform onprem` and diffing against `--platform mistral` shows three gaps:

- No InfiniBand NCCL environment. The render carries only the base vars (`NCCL_DEBUG`, `NCCL_NVLS_ENABLE`, `NCCL_CUMEM_ENABLE`, `NCCL_P2P_NET_CHUNKSIZE`, `NCCL_MNNVL_ENABLE`). The fabric is not disabled, just untuned.
- No NIC resource on the worker containers, so pods get no RDMA device allocation from whatever device plugin the site runs.
- No tolerations. GB200/GB300 nodes are arm64 and typically tainted, so the pods may not schedule at all.

The closest existing model is the Mistral GB300 InfiniBand override (ADR-058), but it hardcodes site specifics that do not transfer to generic on-prem: pinned HCA names (`NCCL_IB_HCA` and `UCX_NET_DEVICES` set to `mlx5_0:1,...,mlx5_3:1` in `pkg/catalog/entries/_lib/nccl/mistral-ib-env.yaml`) and the `rdma/ib` resource name (`pkg/catalog/entries/_lib/deps/mistral-gb300-ib-runtime-patch-comm.yaml:25`), which depends on the device plugin a site runs.

A field team supporting an on-prem GB300 NVL72 deployment has offered to contribute an override modeled on the Mistral block and to validate it on real hardware. This ADR defines the shape that contribution should take.

## Decision

1. **Add `onprem` + `gpuArchitecture in [gb200, gb300]` override blocks** to every catalog entry that has platform-specific behavior on NVL72 hardware: the 5 communication NCCL variants (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`, `nccl-loopback`, `nccl-loopback-nvswitch`) and the 2 training variants (`nemotron5-8b`, `nemotron5-56b`), and mirror the same blocks in the WorkloadRun override matrix (`pkg/platform/overrides/workloadrun.yaml`), so Certification and WorkloadRun stay in sync.
2. **Author the new pieces as shared `_lib/` fragments** referenced from both surfaces:
   - `_lib/nccl/onprem-ib-env.yaml`: an IB-friendly NCCL environment **without HCA pinning**. It carries the fabric-portable subset of the Mistral set (relaxed-ordering, PMIX, and OMPI hygiene vars plus the templated `NCCL_MNNVL_ENABLE`) and deliberately omits `NCCL_IB_HCA` and `UCX_NET_DEVICES`; NCCL auto-detects HCAs when unpinned. The exact final list is confirmed during real-hardware validation.
   - `_lib/deps/onprem-gb200-gb300-runtime-patch-comm.yaml` and `_lib/deps/onprem-gb200-gb300-runtime-patch-training.yaml`: TrainingRuntime patches carrying both tolerations (`kubernetes.io/arch=arm64:NoSchedule` and `nvidia.com/gpu=present:NoSchedule`) and an optional NIC resource block.
3. **Template the NIC resource, injected only when configured.** The resource name comes from a new `CategoryOptions` field, `nicResourceName` (resource names vary by device plugin: `rdma/ib`, `nvidia.com/mlnxnics`, `rdma/shared_ib`, and others are all in the wild). The count comes from the existing `mlnxPerNode` option. The dep fragments guard the block with `{{- if .NicResourceName }}`; when the name is unset, no NIC resource is injected and scheduling is unchanged.
4. **Keep the existing GB200/GB300 ComputeDomain block unchanged.** It already fires on onprem (it has no platform matcher) and contributes the ComputeDomain and DRA channel deps (`_lib/deps/gb200-compute-domain-and-dra-comm.yaml`, `-torch.yaml`). The new onprem blocks are appended at the tail of each `overrides:` list, after it, so the onprem replacement wins: `trainer.args` on the communication entries, and the `trainer.env` block on the training entries, which tune env rather than args (the same ordering discipline ADR-058 established).

## Implementation

- `api/v1alpha1/certification_types.go`: add `NicResourceName *string` to `CategoryOptions` (next to `MlnxPerNode`, currently at lines 159-165), validated as a Kubernetes extended resource name. `api/v1alpha1/workloadrun_types.go`: parallel `NicResourceName` field on `WorkloadRunSpec` next to `mlnxPerNode` (lines 209-215). Run `make manifests generate`.
- `pkg/catalog/catalog.go` (`BuildConfig`, `MlnxPerNode` precedent at lines 45-48) and `pkg/catalog/loader.go` (`TemplateData`, `MlnxPerNode` precedent at lines 94-96): add `NicResourceName string` and thread it through `Build`.
- `pkg/platform/overrides.go` (`OverrideConfig`, lines 31-38): add `NicResourceName`, populated by the WorkloadRun controller alongside the existing gpus/mlnx resolution (`pkg/controller/workloadrun_controller.go:241-261`) and the CLI path (`pkg/workloadrun/`).
- Certification-side resolution mirrors `mlnxPerNode`: global default with per-category override (`pkg/controller/certification_controller.go:437-466` and `:614-615`).
- New `_lib/` fragments listed in the Decision; per-entry override blocks appended at the tail of the 7 catalog entries; workloadrun.yaml gains a dependency block plus a non-MPI env block, mirroring the Mistral blocks at `pkg/platform/overrides/workloadrun.yaml:246-269`.
- Mock node fixture `pkg/render/nodes/onprem-gb300.yaml` (empty `providerID`), mirroring `pkg/render/nodes/mistral-gb300.yaml`.
- Docs: `docs/api-reference/certification.md` and `docs/api-reference/workloadrun.md` gain `nicResourceName`; the platform docs gain an on-prem section including the Metal3 detection note below. the on-prem example lives in `docs/concepts/platform-detection.md` (the repo no longer carries a `config/samples/` directory).

### Testing plan

- **Render verification** per CLAUDE.md's nvcrectl render procedure: build `bin/nvcrectl`, create temp cert YAMLs for GB200 and GB300 (and an H100 control that must not match), render with `--platform onprem`, and grep for the markers this override owns: both toleration keys, the configured NIC resource name (present only when `nicResourceName` is set), and the absence of `NCCL_IB_HCA`. Confirm the `nvcrectl.nvidia.com/applied-overrides` annotation lists the onprem block and `detected-platform: onprem`.
- **testutil goldens** (`testutil.TestCaseParser` per the `/cre-test` skill): new cases under `pkg/platform/testdata/build-overrides/` (onprem torch and MPI, with and without `nicResourceName`), new render cases under `pkg/certification/testdata/certification-render-onprem/`, and a new integration case `certification-onprem-gb300-nccl` under `cmd/integration/testdata/reconcile/` mirroring `certification-mistral-gb300-nccl`. Golden files are generated once the rendered output is reviewed, with maintainer approval per CLAUDE.md.
- **UAT**: new fixtures under `test/uat/testdata/onprem/gb300/nccl/expected_pods.yaml` with a KWOK node profile that ships no `providerID`, plus a `TestOnPremGB300NCCL` function mirroring the existing per-CSP tests in `test/uat/`.

## Rationale

- **Tolerations gate everything else.** Without them the pods never schedule on tainted arm64 nodes, so no amount of NCCL tuning matters. Both taints appear in practice on NVL72 deployments and both tolerations are already field-validated in the Mistral patch (`mistral-gb300-ib-runtime-patch-comm.yaml:29-37`).
- **Unpinned IB env is the only portable choice.** HCA names are per-site facts. NCCL's auto-detection is the documented behavior when `NCCL_IB_HCA` is unset, so omitting the pin degrades gracefully everywhere instead of breaking everywhere except one site.
- **Opt-in NIC resource, name as data.** There is no resource name NVCRE can safely assume for generic on-prem; injecting a wrong one makes pods permanently Pending (the exact failure ADR-058 called out for `mlnxnics` on Mistral). Making the name a `CategoryOptions` field and reusing `mlnxPerNode` for the count adds one small field instead of a new per-site override block per device plugin.
- **Shared `_lib/` fragments keep the two surfaces in sync.** The `pkg/platform` package renders the same fragments the catalog uses, by design ("a single source of truth", `pkg/platform/overrides.go:4-9`); authoring the onprem pieces anywhere else would create drift.

## Consequences

- **CRD schema change** to both Certification and WorkloadRun (new optional field), so `make manifests generate`, docs, and samples all update. Existing resources are unaffected: the field is optional and nil means today's behavior.
- **Onprem GB200/GB300 renders change**: tolerations and the IB env appear where previously only the ComputeDomain block did. No existing golden exercises onprem GB200/GB300 through a communication or training entry (the only onprem GB300 render golden today is the diagnostics case `pkg/certification/testdata/certification-render/single-category`, which this design does not touch), so the testing plan adds goldens rather than updating existing ones; each new golden is reviewed field by field before it lands.
- **`nicResourceName` unset means no RDMA device allocation**, matching today. The override still improves scheduling (tolerations) and tuning (env), so partial adoption is safe.
- **Override ordering stays load-bearing.** The onprem blocks are appended at the tail so their `trainer.args` replacement wins over the GB200/GB300 base block; inserting future blocks between them would silently change output. Golden files protect against regression, as in ADR-058.
- **The override matches every no-providerID cluster**, not only NVL72 bare metal, because `onprem` is the detection fallback. Scoping the `when` to `gpuArchitecture in [gb200, gb300]` bounds the blast radius to the hardware the contents were written for.

## Alternatives Considered

- **Copy the Mistral override verbatim.** Rejected: it pins site-specific HCA names (`NCCL_IB_HCA`, `UCX_NET_DEVICES` in `_lib/nccl/mistral-ib-env.yaml`) and hardcodes the `rdma/ib` resource name, which depends on the device plugin a site runs. On any other site those settings range from noisy to unschedulable.
- **Do nothing.** Rejected: on-prem NVL72 renders leave the fabric untuned and, on tainted arm64 nodes, produce pods that never schedule. The failure is silent (Pending pods, eventual timeout) and lands on exactly the users least able to debug rendered manifests.
- **Auto-detect the NIC resource from node allocatable.** Feasible in principle: the controllers already list target nodes, and detection by allocatable resource has precedent (NScale keys on `nscale.com/rdmashare`, `pkg/controller/workflow_detect.go:61-65`). Deferred rather than rejected: it needs a curated candidate list, a tiebreak when several RDMA-ish resources are advertised, and it cannot work in `nvcrectl certification render` without a cluster. A follow-up can layer detection on top of the field (field wins, detection fills the gap).

## Notes

- **Metal3 detection trap.** `metal3://` providerIDs map to `mistral` (`pkg/controller/workflow_detect.go:71-72`), so an on-prem site provisioned by Cluster API + Metal3 silently picks up the Mistral site-specific override (pinned HCAs) instead of this one. This ADR does not change detection; the platform docs must state the trap and the `--platform onprem` escape hatch for render, and a future ADR can revisit label-based disambiguation (anticipated in ADR-058).
- **Heterogeneous platforms fail fast.** `detectPlatformConsistent` returns an error when target nodes disagree on platform (`pkg/controller/workflow_detect.go:88-100`), so a mixed onprem/metal3 node set surfaces loudly rather than half-matching.
- **Diagnostics needs no onprem block.** `dcgm-level4` already tolerates all taints (`pkg/catalog/entries/diagnostics/dcgm-level4.yaml:41-42`) and is intra-node, so it schedules and runs on tainted arm64 nodes unchanged.
- **Defaults interaction.** `gb200`/`gb300` default `mlnxPerNode: 8` (`pkg/catalog/entries/_lib/gpu-defaults.yaml`). Sites running a shared-device plugin (one `rdma/ib`-style resource per pod) should set `mlnxPerNode: 1` alongside `nicResourceName`; the docs example will show both.
- **A field contribution with real-hardware validation is expected.** The env-var list, the default guidance for `nicResourceName`/`mlnxPerNode`, and the UAT expected output are finalized against that validation before merge.

## References

- ADR-012: Platform and GPU architecture overrides (override semantics, `onprem` fallback)
- ADR-046: Shared template library (`_lib/` fragments)
- ADR-058: Mistral GB300 SKU support (closest precedent; source of the tolerations and IB env shape)
- GitHub issue #318
- Code citations:
  - `pkg/controller/workflow_detect.go:43-50` (empty providerID resolves to onprem), `:61-65` (NScale allocatable-based detection), `:71-72` (`metal3://` resolves to mistral), `:88-100` (heterogeneous platform fail-fast)
  - `pkg/catalog/entries/communication/nccl-all-reduce.yaml:352-357` (GB200/GB300 ComputeDomain block), `:466-483` (Mistral block shape to mirror)
  - `pkg/platform/overrides/workloadrun.yaml:8-13` (GB200/GB300 block), `:246-269` (Mistral blocks to mirror)
  - `pkg/catalog/entries/_lib/nccl/mistral-ib-env.yaml` (pinned `NCCL_IB_HCA`/`UCX_NET_DEVICES`)
  - `pkg/catalog/entries/_lib/deps/mistral-gb300-ib-runtime-patch-comm.yaml:25` (`rdma/ib` name), `:29-37` (both tolerations)
  - `api/v1alpha1/certification_types.go:159-165` and `api/v1alpha1/workloadrun_types.go:209-215` (`mlnxPerNode` fields the new field sits beside)
  - `pkg/catalog/catalog.go:45-48`, `pkg/catalog/loader.go:94-96`, `pkg/platform/overrides.go:31-38` (plumbing points)
  - `pkg/controller/certification_controller.go:437-466`, `pkg/controller/workloadrun_controller.go:241-261` (option resolution)
  - `pkg/catalog/entries/_lib/gpu-defaults.yaml` (gb200/gb300 `mlnxPerNode: 8`)
  - `pkg/catalog/entries/diagnostics/dcgm-level4.yaml:41-42` (tolerate-all)
  - `pkg/render/nodes/mistral-gb300.yaml` (fixture to mirror)
