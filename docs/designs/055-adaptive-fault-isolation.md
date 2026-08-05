# ADR-055: Adaptive Fault Isolation for NCCL Diagnostics

> **Status:** Accepted

## Context

Large GPU clusters (200+ nodes) require efficient fault isolation when NCCL communication tests fail. The current `testScale: pairwise` mode generates all C(N,2) node pairs upfront and stores them in the Workflow status. At 232 nodes this produces 26,796 groups, exceeding Kubernetes' etcd object size limit (1.5 MB) and causing the Workflow controller to crash-loop.

Exhaustive pairwise testing is also unnecessary from an algorithmic perspective. Combinatorial group testing theory establishes that identifying d defective items among N items requires at minimum Ω(d log(N/d)) tests [1][2]. For N=232 and d=5, this lower bound is ~35 tests — three orders of magnitude fewer than the 26,796 pairs that exhaustive pairwise generates.

GPU clusters have natural topology (NVLink cliques/racks) that provides a free hierarchical decomposition. Testing each topology domain as a unit immediately localizes faults to a small subset of nodes, after which targeted refinement identifies the specific defective nodes.

## Decision

Introduce a new `testScale: diagnose` mode that implements topology-aware hierarchical group testing in three adaptive stages. The algorithm is near-optimal in total tests (~1.5× the information-theoretic lower bound) while minimizing wall-clock time through aggressive parallelism.

### Algorithm

**Round 1 — Stage 1a: Intra-Domain Screening (1 parallel round)**

Test each topology domain (clique/rack) as a single NCCL group, all in parallel. This validates the NVLink/NVSwitch layer within each rack. Domains that pass are cleared; their nodes are marked healthy. Failed domains advance to Stage 2.

When no topology labels are available (e.g., H100, B200 clusters), the algorithm partitions nodes into disjoint groups of size K = ⌈√N⌉. This choice is optimal for unknown defective count: it minimizes the maximum of Stage 1 groups and Stage 2 work, and scales naturally across cluster sizes (N=64 → K=8, N=232 → K=16, N=1000 → K=32) [3].

**Round 2 — Stage 1a-no-nvl: Intra-Domain Screening without NVLink (1 parallel round)**

Re-run the same intra-domain groups with `NCCL_MNNVL_ENABLE=0`. This isolates fabric (RoCE/EFA) issues from NVLink issues. A domain that passed Stage 1a but fails here has a fabric problem, not an NVLink problem. Failed domains' nodes are tracked as "no-NVL suspects" — subsequent bisection and confirmation for these nodes also run with MNNVL disabled.

**Round 3 — Stage 1b: Inter-Domain Screening (1 test)**

Select one representative node from each healthy domain (passed both Stage 1a and 1a-no-nvl). Run a single NCCL all-reduce across all representatives. This validates the RoCE/EFA fabric between racks. If this test passes, the inter-domain fabric is healthy. If it fails, the representative set advances to Stage 2 bisection alongside any failed intra-domain groups.

Representatives from failed domains are excluded — their faults are already localized to the node level.

**Rounds 4-7 — Stage 2: Binary Splitting (2-4 parallel rounds)**

Two independent bisection workstreams run concurrently in a single group pool:
- **Intra-domain**: For each failed domain from Stage 1a, recursively bisect the node set.
- **Inter-domain**: If Stage 1b failed, bisect the representative set to isolate which rack-pair has a bad fabric link.

Both workstreams share the same round advancement — all groups must complete before the next round of splits. Since intra-domain (⌈log₂ 18⌉ ≈ 4 rounds) and inter-domain (⌈log₂ 13⌉ ≈ 4 rounds) have similar depth, coupling wastes minimal time.

Bisection continues until groups reach `minGroupSize` (default 2). Small suspect groups are padded with one known-healthy node to ensure NCCL tests produce meaningful bandwidth measurements.

This stage reuses the existing `Bisect()` function from `pkg/orchestration/bisect.go`, applying Hwang's generalized binary splitting [4] within each failed group.

**Round 7 — Stage 3: Confirmation (1 parallel round)**

Each suspect node (from groups that failed at `minGroupSize`) is paired with a known-healthy node and tested. If the pair fails, the suspect is confirmed defective. If it passes, the suspect is cleared.

This stage eliminates false positives caused by transient failures in earlier stages. All confirmation tests run in parallel.

### Performance

| Scenario | Total Tests | Rounds | Wall Clock (est.) |
|----------|------------|--------|-------------------|
| d=1, topology, no fabric fault | ~22 | 7 | ~28 min |
| d=5, spread across cliques | ~55 | 7 | ~28 min |
| d=0, fabric fault between 2 racks | ~23 | 7 | ~28 min |
| d=3, no topology (N=232) | ~57 | 7 | ~28 min |
| All healthy (no faults) | 28 | 3 | ~12 min |
| Exhaustive pairwise (current) | 26,796 | ~230 | ~19 hours |

The algorithm is within 1.5× of the information-theoretic lower bound of ~35 tests for N=232, d=5.

## Implementation

### API

New `testScale` value on `CategoryOptions`:

```yaml
categories:
  - domain: communication
    variant: nccl-all-reduce
    options:
      testScale: diagnose
      minGroupSize: 2  # optional, default 2
```

New `DiagnoseSpec` on `OrchestrationSpec`:

```go
type DiagnoseSpec struct {
    MinGroupSize int    `json:"minGroupSize,omitempty"` // default 2
    TopologyKey  string `json:"topologyKey,omitempty"`  // e.g., "nvidia.com/gpu.clique"
}
```

New `DiagnoseStatus` on `OrchestrationStatus`:

```go
type DiagnoseStatus struct {
    Stage               string   `json:"stage"`               // intra-screening, intra-screening-no-nvl, inter-screening, bisection, confirmation
    Round               int      `json:"round"`
    HealthyNodes        []string `json:"healthyNodes,omitempty"`
    SuspectNodes        []string `json:"suspectNodes,omitempty"`
    NoNVLSuspectNodes   []string `json:"noNVLSuspectNodes,omitempty"`   // suspects from no-NVL stage, run with MNNVL=0
    RepresentativeNodes []string `json:"representativeNodes,omitempty"`
}
```

### Status Size

The algorithm never stores more than ~30 groups simultaneously:

| Stage | Max Groups | Estimated Size |
|-------|-----------|----------------|
| Screening | 13-16 | ~8 KB |
| Bisection | ~30 | ~15 KB |
| Confirmation | ~10 | ~5 KB |
| DiagnoseStatus | — | ~5 KB |
| **Total** | — | **< 35 KB** |

Well within etcd's 1.5 MB limit at every stage.

### Topology Fallback

| Cluster Type | Stage 1 Grouping | Group Size |
|-------------|-----------------|------------|
| GB200/GB300 (NVLink cliques) | Group by `nvidia.com/gpu.clique` | Natural clique size |
| Other topology labels | Group by configured topology key | Natural domain size |
| No topology (H100/B200) | Disjoint groups of ⌈√N⌉ | Auto-computed |

### Key Files

- `api/v1alpha1/certification_types.go` — `TestScaleDiagnose` constant, kubebuilder enum, `MinGroupSize` on `CategoryOptions`
- `api/v1alpha1/workflow_types.go` — `DiagnoseSpec`, `DiagnoseStatus`, stage constants
- `pkg/orchestration/diagnose.go` — `ScreenGroups()`, `BuildConfirmationGroups()`
- `pkg/controller/workflow_controller.go` — `initDiagnose()`, `handleDiagnoseRoundComplete()`, `diagnoseNextGroups()`, `diagnoseSetGroups()`, `diagnoseDone()`
- `pkg/catalog/entries/communication/nccl-*.yaml` — diagnose testScale template conditional
- `pkg/catalog/catalog.go`, `pkg/catalog/loader.go` — `MinGroupSize` in BuildConfig/TemplateData

## Rationale

1. **Adaptive over exhaustive**: Group testing theory proves that adaptive algorithms achieve O(d log N) tests, exponentially better than the O(N²) exhaustive pairwise approach. The adaptive approach is both faster and produces the same diagnostic output.

2. **Three stages over two**: A confirmation stage adds one round (~4 minutes) but eliminates false positives. In production GPU clusters, transient NCCL failures from thermal throttling or driver issues are common. Without confirmation, operators waste time investigating healthy nodes.

3. **Dual-layer screening (intra + inter)**: GPU clusters have two failure domains — NVLink within racks and RoCE/EFA between racks. Testing both layers catches node-level faults (bad GPU, bad NVLink) and fabric-level faults (bad transceiver, misconfigured switch port). Inter-domain screening runs sequentially after intra-domain to avoid node overlap.

4. **Topology-aware grouping**: Using topology for Stage 1 grouping exploits the cluster's natural hierarchy for free. It also produces results aligned with physical failure domains, making remediation actionable.

5. **√N fallback**: For clusters without topology labels, ⌈√N⌉ minimizes the worst-case total tests across all stages for unknown defective count [3]. It requires no user configuration and scales automatically.

6. **Reuse of Bisect()**: Stage 2 reuses the existing bisection algorithm rather than implementing a new splitting strategy, reducing code duplication and leveraging proven logic.

7. **Per-clique bisection**: Failed screening cliques are bisected independently, not merged into one group. This preserves topology boundaries, enables parallel bisection across cliques, and converges in O(log K) rounds per clique (K = clique size) rather than O(log N) on a merged blob.

8. **Confirmation parallelism**: Each suspect is paired with a different healthy reference node (rotating through the healthy pool). This eliminates node overlap between confirmation groups, allowing all confirmations to run in parallel instead of sequentially.

9. **Suspect cleanup**: After confirmation, SuspectNodes is updated to contain only confirmed-faulty nodes. Cleared suspects are moved to HealthyNodes. This ensures the report shows accurate counts.

10. **Bandwidth threshold integration**: When `busBandwidthGBps` is configured, groups that pass NCCL (exit 0) but have bandwidth below the threshold are treated as failed and bisected. This catches degraded nodes that don't cause outright failures.

11. **Timeout enforcement**: Diagnose sets `timeoutPerJob: 4m` (5 iterations, 1 cycle). Jobs exceeding the timeout are marked failed, their workloads deleted to free GPUs, but the Job object is preserved for the report. A 30-second grace period allows BandwidthMeasurement controllers to parse results before the workflow advances stages.

## Consequences

- New `testScale: diagnose` value added to the enum; existing modes unchanged
- Diagnose mode replaces pairwise for large-scale fault isolation; pairwise remains available for small clusters where C(N,2) fits in status
- Report must handle multi-stage results from `DiagnoseStatus` + `BandwidthMeasurement` CRs
- The algorithm assumes the OR-model (any group containing a defective node will fail). If failures are pair-specific (node A fails only with node B), the screening stage may miss the fault. This is acceptable because NCCL hardware faults are node-level, not pair-level.

## References

[1] D.-Z. Du and F. K. Hwang, "Combinatorial Group Testing and Its Applications," 2nd ed., World Scientific, 2000.

[2] A. De Bonis, L. Gasieniec, and U. Vaccaro, "Optimal Two-Stage Algorithms for Group Testing Problems," SIAM J. Comput., vol. 34, no. 5, pp. 1253–1270, 2005.

[3] A. Coja-Oghlan, O. Gebhard, M. Hahn-Klimroth, and P. Loick, "Optimal Group Testing," Proc. 33rd Conf. Learning Theory (COLT), vol. 125, pp. 1374–1388, 2020.

[4] F. K. Hwang, "A Method for Detecting All Defective Members in a Population by Group Testing," J. Amer. Statistical Assoc., vol. 67, no. 339, pp. 605–608, 1972.
