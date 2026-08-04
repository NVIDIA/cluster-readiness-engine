# ADR-060: Azure H100 Multi-Node NCCL Support

> **Status:** Proposed

## Context

Azure ND H100 v5 nodes ship with 8× H100 SXM5 over NDR InfiniBand and the Mellanox NIC is exposed through the SR-IOV device plugin as `nvidia.com/mlnxnics`. The communication catalog already has an "Azure (InfiniBand); excludes A100" override on every NCCL variant ([nccl-all-reduce.yaml:296-334](../../pkg/catalog/entries/communication/nccl-all-reduce.yaml#L296-L334) and the sibling files). That override pulls in the right dependency — [`_lib/deps/azure-ib-with-topo-comm.yaml`](../../pkg/catalog/entries/_lib/deps/azure-ib-with-topo-comm.yaml) bundles the `nvidia.com/mlnxnics: "8"` request and the `azure-h100.xml` topology configmap — but it only exports four NCCL env vars (`NCCL_DEBUG`, `NCCL_IB_PCI_RELAXED_ORDERING`, `NCCL_SOCKET_IFNAME`, `NCCL_TOPO_FILE`).

A field-validated workflow (saved as `azure.yaml` in the repo root during testing) demonstrates that on Azure H100 ND v5 the test runs only as expected with **17** `-x` env vars: the four already present plus thirteen tuning vars covering UCX transport pinning (`UCX_TLS=tcp`, `UCX_NET_DEVICES=eth0`, `UCX_IB_PCI_RELAXED_ORDERING=on`, `UCX_MEM_EVENTS=n`), OpenMPI hostname/HCOLL handling (`OMPI_MCA_orte_keep_fqdn_hostnames=t`, `OMPI_MCA_coll_hcoll_enable=0`), and NCCL channel/QP/NVLS tuning (`NCCL_NVLS_ENABLE=1`, `NCCL_MIN_NCHANNELS=32`, `NCCL_NET_GDR_LEVEL=5`, `NCCL_IB_QPS_PER_CONNECTION=4`, `NCCL_IGNORE_CPU_AFFINITY=1`, `NCCL_P2P_NET_CHUNKSIZE=2097152`, `CUDA_DEVICE_ORDER=PCI_BUS_ID`).

The same gap exists across all three multi-node NCCL variants (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`). Loopback variants are single-node and out of scope.

## Decision

1. **Add an explicit `Azure + H100` override to each multi-node NCCL catalog entry** (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`) following the existing per-architecture pattern (mirrors the `Azure + A100` block already present in those files).
2. **Append the H100 override after the generic `Azure (non-A100)` block.** Both overrides match for an H100 node; the generic one fires first and brings the topo dependency, then ours layers the full args list. This preserves the non-A100 fallback for any future Azure GPU SKU we have not yet tuned.
3. **Provide the full `trainer.args` list in the H100 override**, not a delta. Kubeflow `Trainer.Args` is an unnamed list and strategic-merge replaces it (per [ADR-012](./012-platform-gpu-overrides.md) "lists replace" semantics) — the same approach the existing `Azure + A100` and `AWS + GB200` overrides take.
4. **Do not redeclare the dependency.** `azure-ib-with-topo-comm.yaml` is already added by the generic non-A100 override; adding it again from the H100 block would only run through the merge-by-name path with no effect.
5. **Match by `gpuArchitecture: { equals: h100 }`** rather than by node label. This is consistent with the rest of the override table ([nccl-all-reduce.yaml:262-266](../../pkg/catalog/entries/communication/nccl-all-reduce.yaml#L262-L266) for GCP+H100, [`workflow_detect.go`](../../pkg/controller/workflow_detect.go) does the parsing).

## Implementation

- **Catalog entries** — append `Azure + H100` override block after the existing `Azure (non-A100)` block in each file:
  - `pkg/catalog/entries/communication/nccl-all-reduce.yaml`
  - `pkg/catalog/entries/communication/nccl-all-gather.yaml`
  - `pkg/catalog/entries/communication/nccl-alltoall.yaml`
- **Args set carried verbatim from the validated `azure.yaml`** (in mpirun `-N N --allow-run-as-root --mca plm_rsh_args -o StrictHostKeyChecking=no -x …` form), then the per-variant `/usr/local/bin/<all_reduce|all_gather|alltoall>_perf_mpi` binary plus the same `-b 8 -e {{ .MaxBytes }} -f 2 -n {{ .NumIterations }} -N {{ .NumCycles }}` parameter block already used by the sibling overrides.
- **No code changes** in `pkg/controller/`, `internal/`, or `pkg/gpu/`. `azure://` providerID detection ([`workflow_detect.go:46`](../../pkg/controller/workflow_detect.go#L46)) and `NVIDIA-H100-80GB-HBM3 → h100` parsing ([`gpu/product.go:25`](../../pkg/gpu/product.go#L25)) are already in place, as is `--platform azure` in ncrectl.
- **Sample**:
  - Restore `config/samples/cre_v1alpha1_certification_azure_a100_nccl.yaml` to its committed A100 form (it was edited locally to point at H100 during testing).
  - Add `config/samples/cre_v1alpha1_certification_azure_h100_nccl.yaml` covering the multi-node communication variants.
- **Integration test**: `cmd/integration/testdata/reconcile/certification-azure-h100-nccl/` with `input_client_objects.yaml` (two `azure://` H100 nodes + Certification), `input_config.yaml` (wait `InProgress`, collect Certification + Workflow), and `expected.json` generated via `TESTUTIL_UPDATE_EXPECTED=true make test-integration` and reviewed by hand before commit.
- **`make embed-ncrectl`** after catalog YAML changes to refresh `pkg/setup/embedded/` per CLAUDE.md.

## Rationale

- **Layered override over a single replace.** Keeping the generic Azure (non-A100) block ensures any future Azure SKU (e.g., H200) renders with topo + mlnxnics + the four base NCCL vars by default, instead of falling all the way through to no Azure-specific configuration.
- **Full args list in the override.** Strategic-merge on Kubeflow `Trainer.Args` replaces, so building the arg list inline is the only deterministic way to set the env vars. This is the same shape the other Azure-specific overrides use today.
- **Match parity with the working `azure.yaml`.** Operators have a known-good rendering they can compare against; the override is a one-to-one transcription so the CLI output is byte-identical (modulo node names and template-driven counts).
- **Scoped to multi-node NCCL.** The 17-var set is meaningful because each var targets cross-node IB or hostname routing. Loopback variants stay on the existing minimal config — adding the IB tuning there is noise.

## Consequences

- **Override ordering becomes load-bearing for Azure H100.** A future edit that reorders the Azure (non-A100) block after the H100 block would zero out the H100 args (because the non-A100 block carries a shorter args list). Mitigation: integration golden file `certification-azure-h100-nccl/expected.json` will fail on regression.
- **Other Azure non-A100, non-H100 GPUs (H200, B200) keep the four-var minimum.** Acceptable — adding tuning for those SKUs is a future ADR with its own field validation.
- **Same args duplicated across the catalog files.** Could be hoisted into a shared `_lib/nccl/azure-h100-mpiargs.yaml` later (matches the [`nccl/togetherai-ib-mpiargs.yaml`](../../pkg/catalog/entries/_lib/nccl/togetherai-ib-mpiargs.yaml) precedent), but doing so now would entangle ADR-060 with the unrelated decision of when to extract a lib. Leave inline for now; promote to lib in a follow-up if another variant lands.

## Alternatives Considered

- **Replace `Azure (non-A100)` with `Azure + H100` outright.** Rejected — removes the fallback for any future Azure SKU and breaks the precedent set by the existing `Azure + A100` block which also appends rather than replaces.
- **Use `jobTemplatePatch` (RFC 6902) to append the extra `-x` flags onto the base list.** Rejected — the patch path indices would silently rot if the upstream `Azure (non-A100)` args order changes; full-list replacement is more robust.
- **Promote the args set into `_lib/nccl/azure-h100-mpiargs.yaml` immediately.** Rejected for this PR — see Consequences. The per-file duplication is small (~25 lines each) and easy to factor later.
- **Add a UAT case (`test/uat/azure_h100_nccl_test.go`).** Deferred — no Azure-shaped KWOK fixtures exist yet, and integration coverage already protects the override.
- **Tune for H200/B200 in the same change.** Deferred — no field-validated working spec available; will track separately.

## Notes

- The exact 17 `-x` vars and their order are taken verbatim from the field-validated `azure.yaml` to make `ncrectl certification render --platform azure` output diffable against it.
- `mlnxnics: "8"` (from the existing dep) and `numProcPerNode: {{ .GpusPerNode }}` align with H100 SXM5 8-GPU baseboards; if a different Azure H100 SKU appears with a different fan-out, the catalog's `gpusPerNode` parameter handles it without override changes.

## References

- ADR-012: Platform and GPU architecture overrides
- ADR-031: Platform-aware NCCL communication configuration
- ADR-046: Shared template library
- ADR-047: Standardize NCCL on AWS
- ADR-058: Mistral GB300 SKU support — closest recent precedent for adding a single-CSP, single-SKU override across multiple catalog entries.
