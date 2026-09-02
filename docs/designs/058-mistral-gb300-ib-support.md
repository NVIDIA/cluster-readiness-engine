# ADR-058: Mistral GB300 SKU Support (InfiniBand)

> **Status:** Proposed

## Context

Mistral is a new CSP running NVCRE certifications on bare-metal Grace+GB300 nodes provisioned via Cluster API + Metal3. An example target node reports:

- `spec.providerID: metal3://<uuid>` — a prefix not currently recognized by [`workflow_detect.go`](../../pkg/controller/workflow_detect.go), so platform detection falls through to `onprem` and no overrides match.
- `nvidia.com/gpu.product: NVIDIA-GB300`, `kubernetes.io/arch: arm64`, `nvidia.com/gpu.count: 4`.
- `rdma/ib: 64` as allocatable/capacity — the host exposes InfiniBand via `k8s-rdma-shared-dev-plugin`, not SR-IOV. `nvidia.com/mlnxnics` is `0`.
- Four Mellanox HCAs (`mlx5_0..mlx5_3`). NCCL/UCX/PMIX/SHARP tuning is required per Mistral's site guidance.

No existing platform override targets Mistral, and no catalog path emits `rdma/ib` requests — every InfiniBand override today (Azure, TogetherAI) uses `nvidia.com/mlnxnics`.

## Decision

1. **Register `mistral` as a platform** by mapping the `metal3://` providerID prefix to `mistral` in [`workflow_detect.go`](../../pkg/controller/workflow_detect.go) and whitelisting it in `nvcrectl`'s valid-platform map.
2. **Introduce `rdma/ib` as a third RDMA resource axis** (alongside EFA and `mlnxnics`) via new shared dep libraries `mistral-gb300-ib-runtime-patch-{comm,training}.yaml`. Per-pod count: `rdma/ib: "8"` by default; reviewable in the dep file.
3. **Add `mistral + gb300` override blocks** to every catalog entry that has platform-specific behavior today: 5 communication NCCL variants (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`, `nccl-loopback`, `nccl-loopback-nvswitch`), 5 nemotron training variants, plus the diagnostics entry (`dcgm-level4`) where it requires a mistral-specific resource or env change.
4. **Reuse the existing `[gb200, gb300]` ComputeDomain override** rather than emitting it again — the shared override fires first in list order and contributes the `ComputeDomain` / DRA channel deps; the `mistral + gb300` override that follows only needs to layer the IB runtime patch, mpirun args, and ARM64 toleration.
5. **Share two NCCL env-var lib files** — `nccl/mistral-ib-mpiargs.yaml` (mpirun `-x` form for the NCCL test variants) and `nccl/mistral-ib-training-env.yaml` (pod-env form for nemotron trainers) — carrying the seven Mistral-supplied variables (`NCCL_IB_HCA`, `OMPI_MCA_btl_openib_warn_default_gid_prefix`, `CUDA_CACHE_DISABLE`, `NCCL_IB_PCI_RELAXED_ORDERING`, `PMIX_MCA_gds`, `SHARP_COLL_ENABLE_PCI_RELAXED_ORDERING`, `UCX_NET_DEVICES`).
6. **Keep the base pytorch image** (`nvcr.io/nvidia/pytorch:26.01-py3`) that every non-AWS override already inherits; the overrides do not set `image`.

## Implementation

- `pkg/controller/workflow_detect.go` — new `case strings.HasPrefix(providerID, "metal3://"): return "mistral"` before the default, immediately after the `kubevirt://` branch.
- `pkg/certification/certification.go` — add `"mistral": true` to the valid-platform set.
- `pkg/catalog/entries/_lib/deps/mistral-gb300-ib-runtime-patch-comm.yaml` — `TrainingRuntime` patch: `rdma/ib` request/limit on the node container, `kubernetes.io/arch=arm64` toleration.
- `pkg/catalog/entries/_lib/deps/mistral-gb300-ib-runtime-patch-training.yaml` — same for nemotron training runtimes.
- `pkg/catalog/entries/_lib/nccl/mistral-ib-mpiargs.yaml` and `.../nccl/mistral-ib-training-env.yaml` — the env-var libraries.
- 5 × communication + 5 × training catalog entries — append `mistral + gb300` override blocks at the tail of `overrides:` (after the broader `[gb200, gb300]` block so that ordering lets our overrides win).
- `dcgm-level4` diagnostics entry — add an override only if a mistral-specific resource or env change is required (skip intra-node GPU-only tests that are platform-agnostic today).
- `pkg/render/nodes/mistral-gb300.yaml` — mock node fixture mirroring [`pkg/render/nodes/aws-gb300.yaml`](../../pkg/render/nodes/aws-gb300.yaml) shape.
- Integration case `mistral-gb300-nccl-all-gather` under [`cmd/integration/testdata/reconcile/`](../../cmd/integration/testdata/reconcile/).
- UAT fixtures under `test/uat/testdata/mistral/gb300/nccl/` plus new `TestMistralGB300NCCL` test function.
- `make embed-nvcrectl` to refresh `pkg/setup/embedded/` after catalog YAML changes (per [CLAUDE.md](../../CLAUDE.md)'s "Critical Pitfalls").

## Rationale

- **Map `metal3://` outright.** No Mistral-specific label exists on the target nodes, and requiring operators to add one would shift per-tenant setup burden for no practical gain today. The `metal3://` prefix is currently unclaimed by any NVCRE platform; the trade-off is explicit and reversible (ADR-012's override precedence lets us add a label check later without breaking existing configs).
- **`rdma/ib` over `mlnxnics`.** The node's `nvidia.com/mlnxnics: 0` makes `mlnxnics`-based requests unschedulable. Using `rdma/ib` matches what the shared RDMA device plugin actually advertises.
- **Full-catalog coverage in one change.** Phased scoping would leave certain certifications (training, diagnostics) silently rendering incorrect manifests on Mistral until subsequent PRs, which is a worse operator experience than one larger atomic change. ADR-058 (Azure A100) landed full coverage for the same reasons.
- **Leverage `[gb200, gb300]` shared override.** Duplicating the ComputeDomain dep in our mistral-specific file would create drift if the GB200/GB300 DRA definition evolves.

## Consequences

- **No other `metal3://` CSP can coexist** without refactoring detection to check a label. Accept as a known constraint; document in this ADR.
- **`rdma/ib` count is a compile-time constant** in the dep libraries (defaulting to `8`). Sites with different device-plugin fan-out will need to edit the dep file or we promote the count to a catalog option in a follow-up.
- **ARM64 toleration widens scheduling.** The tolerations only apply to the NCCL/nemotron pods on mistral nodes; existing behavior for other platforms is unchanged.
- **Override ordering becomes load-bearing.** Any future edit that inserts a new override between the shared `[gb200, gb300]` block and our `mistral+gb300` block could silently change rendered output. The Implementation step appends at the tail to minimize this risk; integration golden files protect against regression.

## Alternatives Considered

- **Require a node label (`csp.nvidia.com/provider=mistral`) to disambiguate `metal3://`.** Rejected for now — adds operator burden with no present benefit. Will revisit if a second metal3-based CSP appears.
- **Reuse `nvidia.com/mlnxnics` for Mistral.** Rejected — not advertised by the target nodes; pods would remain Pending.
- **Phase by domain (comm first, training later, diagnostics later).** Rejected — leaves partially-correct certifications in the interim, which is harder for operators to reason about than one atomic rollout. ADR-058 established the precedent for per-SKU one-shot coverage.
- **Use a Mistral-specific container image.** Rejected — the default `nvcr.io/nvidia/pytorch:26.01-py3` image inherited by every non-AWS override already contains the `/usr/local/bin/*_perf_mpi` binaries and OpenMPI at `/usr/local/mpi`, which is sufficient given the NCCL tuning we supply via env.

## Notes

- Per-pod `rdma/ib` count, Nemotron6 dep parity with [`gb300-roce-torch.yaml`](../../pkg/catalog/entries/_lib/deps/gb300-roce-torch.yaml), and whether the `dcgm-level4` diagnostics entry needs a mistral-specific override are finalized during implementation; the plan enumerates them as open sub-questions.
- `GpusPerNode` default for GB300 is handled by [`pkg/gpu/`](../../pkg/gpu/) defaults. The example node reports `nvidia.com/gpu.count: 4`; we confirm this matches the catalog default before committing.

## References

- ADR-012: Platform and GPU architecture overrides
- ADR-031: Platform-aware NCCL communication configuration
- ADR-046: Shared template library
- ADR-047: Standardize NCCL on AWS
- ADR-058: Nemotron4-15B on Azure A100 (InfiniBand) — closest precedent for an IB-only CSP SKU addition.
