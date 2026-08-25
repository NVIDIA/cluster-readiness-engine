# ADR-070: WorkloadRun MPI Transport-Layer Overrides for AWS GB300 (RoCE)

> **Status:** Accepted

## Context

The WorkloadRun platform override table ([`pkg/platform/overrides/workloadrun.yaml`](../../pkg/platform/overrides/workloadrun.yaml)) already gives an AWS + GB300 run most of what the certification catalog gives it: the generic GB200/GB300 block adds the ComputeDomain + DRA claims, the AWS + GB300 block adds the RoCE `ResourceClaimTemplate` via [`_lib/deps/gb300-roce-torch.yaml`](../../pkg/catalog/entries/_lib/deps/gb300-roce-torch.yaml), the AWS-wide block strips the EFA OFI plugin (`rm -rf /opt/amazon`, `unset NCCL_NET_PLUGIN`), and the AWS + MPI block forwards `-x NCCL_NET_PLUGIN=none` to workers.

What it does not give is the MPI **transport layer**. The catalog's AWS + GB300 communication override ([nccl-all-reduce.yaml:414-440](../../pkg/catalog/entries/communication/nccl-all-reduce.yaml#L414-L440)) additionally pins OpenMPI's own traffic to TCP on `eth0` — `--mca pml ob1`, `--mca btl tcp,self`, `--mca btl_tcp_if_include eth0`, `--mca oob tcp`, `--mca oob_tcp_if_include eth0` from [`_lib/nccl/aws-gb300-roce-mpirun-args.yaml`](../../pkg/catalog/entries/_lib/nccl/aws-gb300-roce-mpirun-args.yaml) — and forwards the RoCE NCCL env (`NCCL_SOCKET_IFNAME=eth0`, `NCCL_IB_GID_INDEX=3`, `NCCL_NVLS_ENABLE`, `NCCL_CUMEM_ENABLE`, `NCCL_NET_GDR_C2C`) as `-x` flags via `toMpiArgs` on [`_lib/nccl/aws-gb300-roce-env.yaml`](../../pkg/catalog/entries/_lib/nccl/aws-gb300-roce-env.yaml).

Without those pins, OpenMPI is free to select UCX/verbs over the mlx5 devices for its own point-to-point and collective components. [`_lib/nccl/oci-roce-mpirun-args.yaml`](../../pkg/catalog/entries/_lib/nccl/oci-roce-mpirun-args.yaml) documents the failure mechanism on GB300 RoCE: UCC calls `mca_coll_ucc_comm_query` during `MPI_Init`, and UCX opens the mlx5 devices with incorrect GID settings before `NCCL_IB_GID_INDEX` takes effect — SIGSEGV before the workload runs a single step. A WorkloadRun is *more* exposed than the catalog: the catalog pins `image: public.ecr.aws/hpc-cloud/nccl-tests:latest` (plain OpenMPI, no UCC compiled in), while WorkloadRun images are arbitrary and the common ones (NGC PyTorch with HPC-X) ship UCC/HCOLL-enabled OpenMPI. NCCL itself only needs TCP from MPI — it drives RDMA directly through the RoCE claims that the existing override already provides.

The controller side is already structured for this: `applyWRPreTemplateOverrides()` ([workloadrun_controller.go:574-584](../../pkg/controller/workloadrun_controller.go#L574-L584)) prepends override `mpiArgs` ahead of user `mpiArgs`, and `buildJobTemplate()` injects the launch baseline (`-N`, `--allow-run-as-root`, `--mca plm_rsh_args …`, `-x NCCL_DEBUG=INFO`, `-x NCCL_MNNVL_ENABLE=…`). Only the AWS + GB300 transport block is missing. This is issue #175.

## Decision

1. **Source the transport pins from `_lib/` fragments, not Go.** `pkg/platform` already renders `entries/_lib/` fragments through `catalog.TemplateFuncsWithLib()`; duplicating mpirun args in Go constants would create a second source of truth that drifts from the catalog (the package doc in [`overrides.go`](../../pkg/platform/overrides.go) states single-sourcing as the design goal, per ADR-059).

2. **Split the existing fragments into image-agnostic transport subsets, keeping catalog output byte-identical.** `lib` supports recursive inclusion ([loader.go:537-543](../../pkg/catalog/loader.go#L537-L543)), and each subset is a contiguous run of the existing fragment, so the parent fragments become head + `{{ lib … }}` with no rendered diff:
   - `_lib/nccl/roce-tcp-mpi-transport-args.yaml` — the five MCA pairs (`pml ob1`, `btl tcp,self`, `btl_tcp_if_include eth0`, `oob tcp`, `oob_tcp_if_include eth0`), extracted from the tail of `aws-gb300-roce-mpirun-args.yaml`. `oci-roce-mpirun-args.yaml` composes the same fragment (its middle five pairs are identical).
   - `_lib/nccl/mpi-disable-ucc-hcoll-args.yaml` — `--mca coll_ucc_enable 0`, `--mca coll_hcoll_enable 0`, extracted from the tail of `oci-roce-mpirun-args.yaml`, carrying over its SIGSEGV comment.
   - `_lib/nccl/aws-gb300-roce-transport-env.yaml` — the six transport vars (`NCCL_DEBUG_SUBSYS`, `NCCL_SOCKET_IFNAME`, `NCCL_IB_GID_INDEX`, `NCCL_NVLS_ENABLE`, `NCCL_CUMEM_ENABLE`, `NCCL_NET_GDR_C2C`), extracted from the contiguous middle of `aws-gb300-roce-env.yaml`. `PATH`/`LD_LIBRARY_PATH` (image paths), `NCCL_DEBUG`, and `NCCL_MNNVL_ENABLE` stay in the parent fragment only.

3. **Add one AWS + GB300, MPI-only override block to `overrides/workloadrun.yaml`**, guarded by `{{- if eq .FrameworkType "mpi" }}` like the existing AWS + MPI plugin block, whose `mpiArgs` are: `--mca orte_keep_fqdn_hostnames true` (inline — it sits mid-fragment in the catalog's arg order so it cannot ride the shared tail), the transport fragment, the UCC/HCOLL-disable fragment, and `toMpiArgs` of the transport env fragment.

4. **Never assume the nccl-tests image.** Unlike certification, `spec.framework.mpi.mpirunPath` is user-supplied and the image is arbitrary. The override therefore carries no `--prefix /opt/amazon/openmpi`, no `-x PATH`/`-x LD_LIBRARY_PATH`, and no `/usr/bin/env -u OMPI_MCA_*` launcher wrapper (all present in the catalog's block, all specific to `public.ecr.aws/hpc-cloud/nccl-tests`). Command-line `--mca` values outrank image-baked `OMPI_MCA_*` env vars, which makes the wrapper unnecessary for the parameters we pin.

5. **Scope: `platform: aws` + `gpuArchitecture: gb300` + MPI framework only.** Torch/exec never launch through mpirun and receive env through `BaseNCCLEnvVars` on the runtime container; their remaining gap (`NCCL_IB_GID_INDEX=3` as trainer env) is real but has a different mechanism and is tracked separately. Other RoCE CSPs (OCI, GCP) have no field-validated WorkloadRun configuration yet and follow later using the same fragments.

## Implementation

- **Fragment split** (byte-identical refactor):
  - `pkg/catalog/entries/_lib/nccl/roce-tcp-mpi-transport-args.yaml` (new), included from `aws-gb300-roce-mpirun-args.yaml` and `oci-roce-mpirun-args.yaml`.
  - `pkg/catalog/entries/_lib/nccl/mpi-disable-ucc-hcoll-args.yaml` (new), included from `oci-roce-mpirun-args.yaml`.
  - `pkg/catalog/entries/_lib/nccl/aws-gb300-roce-transport-env.yaml` (new), included from `aws-gb300-roce-env.yaml`.
- **Override block** appended to `pkg/platform/overrides/workloadrun.yaml` after the existing "AWS + MPI (NCCL plugin via mpirun -x)" block, so plugin cleanup and transport pins compose in a stable order. No changes to `pkg/platform/overrides.go` or `runtime.go` — `mpiArgs` parsing and `applyWRPreTemplateOverrides()` already handle it. Resulting launcher arg order: controller baseline → platform pins → user `mpiArgs` → `binary` → `args`.
- **Byte-identity verification before any golden touch**: run `make test-integration` *without* `TESTUTIL_UPDATE_EXPECTED` and diff `ncrectl certification render --platform aws` for a GB300 cert against the pre-change output. The catalog goldens and `test/uat/testdata/aws/gb300/nccl/expected_pods.yaml` must not change; if they do, the split is wrong — fix the fragments, do not regenerate.
- **pkg/platform goldens**: `testdata/build-overrides/mpi-with-mnnvl/expected.json` (and any other `frameworkType: mpi` case) gains the new block; regenerate with `TESTUTIL_UPDATE_EXPECTED=true` after user approval and hand-review the added args against the catalog fragment.
- **Integration test**: new case `cmd/integration/testdata/reconcile/workloadrun-mpi-aws-gb300/` — two `aws://` GB300 nodes (`NVIDIA-GB300` product label) + an MPI WorkloadRun mirroring `workloadrun-mpi/`; `expected.json` captures the Workflow trainer args including the transport pins and `-x` env.
- **UAT**: new `test/uat/aws_gb300_workloadrun_test.go` + `test/uat/testdata/aws/gb300/workloadrun/input_workloadrun.yaml`, reusing the existing `aws/gb300/nodes.yaml` KWOK fixtures; asserts the launcher pod args contain `--mca pml ob1` and `-x NCCL_IB_GID_INDEX=3` as spot checks (mirrors `aws_gb200_workloadrun_test.go` lifecycle assertions).
- **Docs**: update `site/content/docs/` WorkloadRun page's platform-overrides table with the AWS + GB300 MPI row.

## Rationale

- **Fragments over Go constants.** ADR-059 deliberately put WorkloadRun overrides in YAML referencing `_lib/` so CSP tuning has one home. A GID index or MCA flag fixed in the catalog after a field incident must flow to WorkloadRun without a second edit.
- **Byte-identical split de-risks the refactor.** Extracting contiguous tails/middles means the catalog side is provably unchanged (goldens run un-regenerated), so this ADR ships one behavioral change — the WorkloadRun block — not two.
- **UCC/HCOLL disables belong in the WorkloadRun path even though the AWS catalog block omits them.** The catalog controls its image (no UCC compiled in); WorkloadRun cannot, and NGC PyTorch's HPC-X OpenMPI hits exactly the `MPI_Init` SIGSEGV the OCI fragment documents. Disabling a component the image lacks is a no-op, so the pin is safe on both kinds of image.
- **No launcher wrapper.** OpenMPI parameter precedence (command line > environment) covers every parameter we pin. The catalog's `/usr/bin/env -u` wrapper exists to neutralize *unpinned* baked-in vars in one known image; generalizing it would hardcode a second binary path into arbitrary images.
- **Prepend-before-user ordering keeps an escape hatch.** User `mpiArgs` land after platform pins, so an operator with a custom fabric setup can still pass their own values, subject to OpenMPI's duplicate-parameter handling.

## Consequences

- **The shared tail fragments become load-bearing for both renderers.** An edit to `roce-tcp-mpi-transport-args.yaml` now changes catalog comm entries *and* every AWS GB300 MPI WorkloadRun. Guarded on both sides: catalog integration goldens + UAT `expected_pods.yaml`, and the new `workloadrun-mpi-aws-gb300` golden.
- **Images that bake `OMPI_MCA_btl_tcp_if_exclude` can still fail** — we pin the `_include` form and include/exclude are mutually exclusive in OpenMPI. Accepted and documented rather than neutralized (see Rationale on the wrapper); the error message is explicit when it happens.
- **AWS GB300 torch/exec WorkloadRuns remain without `NCCL_IB_GID_INDEX`.** Out of scope here; needs a `jobTemplate` env override (a different mechanism than `mpiArgs`) and its own validation.
- **OCI and GCP GB300 WorkloadRuns keep today's behavior.** The fragments are shaped for them to adopt (OCI's block would be transport + UCC-disable + its env), but each needs field validation first — same deferral ADR-060 applied to H200/B200.
- **`pkg/platform` build-overrides goldens churn once.** Expected and reviewed; the render-all test (`TestBuildOverridesRendersForEveryPlatform`) also catches any fragment/template breakage at unit-test speed.

## Alternatives Considered

- **Duplicate the args as Go constants in `pkg/platform`.** Rejected — creates a second source of truth for transport tuning that will drift from the catalog; contradicts the package's stated design (ADR-059).
- **Reuse `aws-gb300-roce-mpirun-args.yaml` wholesale in the WorkloadRun override.** Rejected — it duplicates flags the controller already injects (`-N`, `--allow-run-as-root`, `--mca plm_rsh_args`) and carries `--prefix /opt/amazon/openmpi`, which breaks any image that is not the nccl-tests image.
- **Wrap the launcher command with `/usr/bin/env -u OMPI_MCA_* <mpirunPath>` like the catalog.** Rejected — command-line `--mca` already outranks baked env for pinned parameters, and the wrapper both assumes coreutils `env` in the image and complicates the user-owned `mpirunPath` contract.
- **Apply the pins to all frameworks on AWS + GB300.** Rejected — torch/exec never invoke mpirun; forwarding `-x` args is meaningless there and the env-var route is a separate change.
- **Extend to OCI/GCP GB300 in the same change.** Deferred — no field-validated WorkloadRun runs on those platforms yet (ADR-060 precedent for deferring untuned SKUs).
- **Add a spec knob to opt out of platform transport pins.** Deferred — user `mpiArgs` ordering already provides a workaround, and no user has asked; a knob would become API surface we must support forever.

## Notes

- `NCCL_DEBUG` and `NCCL_MNNVL_ENABLE` are intentionally absent from the transport env fragment: `buildJobTemplate()` already injects both as `-x` flags (MNNVL from `spec.enableMNNVL`), and duplicating them would produce two `-x` entries with OpenMPI-defined precedence.
- The five-pair transport fragment matches the OCI fragment's middle exactly (same values, same order), which is what makes the recursive-`lib` composition byte-identical on both parents. If a future CSP needs a different interface name than `eth0`, parameterize the fragment with a template variable then — do not fork it.
- Byte-identity check used during review: render `communication/nccl-all-reduce` for AWS GB300 before and after the fragment split and `diff` the outputs; the reference counts in CLAUDE.md (`roce` ×6, `FI_EFA` ×0) still apply.

## References

- Issue #175 — WorkloadRun MPI on AWS GB300 missing transport-layer overrides
- ADR-012: Platform and GPU architecture overrides
- ADR-046: Shared template library for catalog entries
- ADR-047: Standardize NCCL communication entries on AWS
- ADR-059: WorkloadRun — simplified workload execution API (introduced `pkg/platform` and the YAML override table)
- ADR-060: Azure H100 NCCL support — precedent for deferring untuned platform/SKU combinations
- `pkg/catalog/entries/_lib/nccl/oci-roce-mpirun-args.yaml` — documents the UCC `MPI_Init` SIGSEGV mechanism on GB300 RoCE
