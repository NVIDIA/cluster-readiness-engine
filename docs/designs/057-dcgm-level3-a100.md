# ADR-057: DCGM Level-3 Diagnostics — A100 Configuration

> **Status:** Accepted

## Context

The `diagnostics/dcgm-level3` catalog entry (ADR-020) runs NVIDIA DCGM Level-3 health checks — GPU memory, PCIe bandwidth, stress, power, page retirement/row remap, and NVLink — as part of burn-in certification. The base template is architecture-agnostic: GPU count is templated via `{{ .GpusPerNode }}`, and the `dcgmi diag --run 4` command auto-detects the GPU.

NVIDIA A100 SXM4 GPUs are a widely deployed architecture with the following characteristics:

- 8 GPUs per node
- NVLink 3rd generation with NVSwitch (600 GB/s bisection bandwidth)
- 40 GB or 80 GB HBM2e memory
- GPU product labels: `NVIDIA-A100-SXM4-40GB` or `NVIDIA-A100-SXM4-80GB`
- No Multi-Node NVLink (MNNVL)

The base `dcgm-level3` template already handles A100 correctly: `DefaultGpusPerNode("a100")` returns 8, and no special resources (EFA, hugepages, topology key, ComputeDomain) are needed. However, each supported GPU architecture should have an explicit override block for documentation and future extensibility, consistent with the architecture-per-architecture pattern.

## Decision

Add an explicit A100 override block to `dcgm-level3.yaml` and validate with catalog lookup and certification integration tests.

The A100 override confirms the base DCGM parameters are correct without adding architecture-specific modifications:

- No topology key — A100 does not use `nvidia.com/gpu.clique` (unlike GB200/GB300 with MNNVL)
- No additional dependencies — no ComputeDomain, DRA, or EFA runtime patches
- No environment variable overrides
- DCGM image `dcgm:4.5.2-1-ubuntu22.04` supports A100

## Implementation

### Override Block

Add a `when: gpuArchitecture in: [a100]` block to `pkg/catalog/entries/diagnostics/dcgm-level3.yaml`. Since no functional changes are needed for A100, the override is an empty confirmation block (`{}`-style). This ensures the override system explicitly recognizes A100 and provides a hook for future A100-specific tuning.

### Tests

1. **Catalog lookup test**: `pkg/catalog/testdata/lookup/diagnostics-dcgm-level3-a100/` with `nvidia.com/gpu.product: NVIDIA-A100-SXM4-40GB` and `gpusPerNode: 8`.
2. **Certification integration test**: `cmd/integration/testdata/reconcile/certification-aws-a100-dcgm/` with a single A100 node and a Certification CR using `diagnostics/dcgm-level3`.

## Rationale

- **Explicit override**: Even though A100 works with the base template, an explicit block documents that A100 was validated and provides a clear extension point.
- **Consistency**: All sibling GPU architecture tickets (H100, GB200, GB300, L40S) follow the same pattern of requiring an explicit override block.
- **Test coverage**: A100-specific tests catch regressions if the template or override system changes.

## Consequences

### Positive

- A100 SXM4 is explicitly documented as a validated GPU architecture for DCGM diagnostics
- Integration test prevents regressions for A100 rendering
- Pattern established for adding remaining architectures (H100, GB200, GB300, L40S)

### Negative

- The override block is functionally a no-op for the current template, adding minimal YAML to the catalog entry

## Alternatives Considered

### No override block (tests only)

The base template already works for A100. We could skip the override and just add tests. Rejected because the ticket close criteria explicitly require an override block, and the pattern is consistent across all architecture tickets.

## References

- ADR-020 — GPU Compute Stress Testing Catalog (DCGM base entry)
- ADR-012 — Platform/GPU Override System
