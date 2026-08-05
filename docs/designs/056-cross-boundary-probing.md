# ADR-056: Cross-Boundary Probing for Infrastructure Fault Detection

> **Status:** Accepted

## Context

ADR-055 introduced adaptive fault isolation via topology-aware screening and bisection. The algorithm efficiently identifies **node-level faults** (bad GPU, bad NIC) in O(d log N) tests. However, it has a blind spot for **infrastructure-level faults** — faults in the interconnect (NVSwitch trunks, spine switches, cables) rather than individual nodes.

The gap manifests as a "both-halves-pass" (BHP) pattern during bisection:

1. A screening group of K nodes fails (e.g., 18-node NVLink clique)
2. Bisection splits into two halves (9+9)
3. Both halves pass individually
4. All suspects are cleared — but the original failure was real

The fault lies in the infrastructure **connecting** the two halves, not in any individual node. Since each half uses only a subset of the interconnect paths, the fault doesn't manifest in subset tests.

This pattern can occur at any topology layer:
- **Intra-rack with MNNVL**: Bad NVSwitch trunk between node groups within a clique
- **Intra-rack without MNNVL**: Bad RoCE/EFA link within a rack
- **Inter-rack**: Bad spine switch or cable between racks

### Research Basis

Recent work on edge-faulty group testing [5] models defects as edges (connections) rather than nodes. Nodes sharing a working connection are correlated — a failed NVSwitch creates a partition. The key finding: correlation from shared edges reduces the number of tests needed compared to independent node faults.

Hypergraph group testing [6] extends this to NVSwitch failures that affect multiple nodes simultaneously (hyperedges). When defective sets are constrained by the cluster's physical topology, fewer tests are needed than the unconstrained case.

Network tomography [7] shows that different probe subsets exercise different infrastructure paths. The pattern of pass/fail across probes localizes the faulty link — this is the foundation for cross-boundary probing.

## Decision

Extend the diagnose algorithm with a new **Stage 2b: Cross-Boundary Probing** that triggers whenever bisection detects the BHP pattern. Instead of clearing all suspects, the algorithm probes the boundary between the two passing halves using mixed groups that exercise cross-boundary infrastructure paths.

### Algorithm

**Detection**: During `handleDiagnoseBisection`, when a bisection round completes and sibling sub-groups (e.g., `bisect-R-0a` and `bisect-R-0b`) both succeed, this signals a BHP. The parent group's failure is confirmed as an infrastructure fault candidate.

**Cross-Boundary Probing** (O(log S) rounds where S = min(|A|,|B|)):

Given two passing halves A and B:

1. **Round 1 — Mixed group test**: Build two non-overlapping mixed groups that exercise cross-boundary paths:
   - Mix-1: A[0:n/2] + B[0:n/2] — first half of each side
   - Mix-2: A[n/2:] + B[n/2:] — second half of each side

   Both run in parallel (no node overlap). This exercises different subsets of cross-boundary infrastructure.

2. **Narrowing**:
   - If both mixed groups fail → infrastructure fault spans the full boundary. Record and report.
   - If one fails, one passes → fault is localized to the boundary segment exercised by the failing group. Recursively split the failing mixed group for finer localization.
   - If both pass → transient failure (race). Clear and continue.

3. **Termination**: When mixed groups reach `minGroupSize` or probe rounds exceed ⌈log₂(min(|A|,|B|))⌉, record the remaining boundary as an `InfrastructureFault`.

**MNNVL Handling**: Cross-boundary probes inherit the MNNVL setting from the stage that triggered bisection:
- BHP from intra-screening (MNNVL on) → probes run with MNNVL on (testing NVSwitch infrastructure)
- BHP from no-NVL bisection (MNNVL off) → probes run with MNNVL off (testing fabric infrastructure)
- BHP from inter-screening → probes run with MNNVL off (testing inter-rack fabric)

### Performance

Cross-boundary probing adds O(log S) rounds only when BHP is detected — the common case (node-level faults) is unchanged.

| Scenario | Extra Rounds | Extra Tests | Wall Clock Added |
|----------|-------------|-------------|------------------|
| No BHP (node fault found) | 0 | 0 | 0 |
| BHP in 18-node clique | 2-3 | 4-6 | ~8-12 min |
| BHP in 8-node inter-domain | 2 | 4 | ~8 min |
| BHP, fault spans full boundary | 1 | 2 | ~4 min |

### Fault Detection Coverage

| Fault Type | Approach | Complexity |
|------------|----------|------------|
| Bad node | Bisection (ADR-055) | O(d log N) |
| Bad NVSwitch trunk | Cross-boundary probing | O(log S) |
| Bad fabric link (intra-rack) | No-NVL + cross-boundary | O(log S) |
| Bad spine/leaf switch | Inter-screening + cross-boundary | O(log R) |
| NVLink vs fabric differentiation | No-NVL stage (ADR-055) | 1 round |

## Implementation

### API Changes

New stage constant:
```go
DiagnoseStageCrossBoundary = "cross-boundary"
```

New fields on `DiagnoseStatus`:
```go
// InfrastructureFaults records detected infrastructure-level faults
// where bisection both-halves-pass but the full group fails.
InfrastructureFaults []InfrastructureFault `json:"infrastructureFaults,omitempty"`

// CrossBoundaryState tracks in-progress cross-boundary probing.
CrossBoundaryState *CrossBoundaryState `json:"crossBoundaryState,omitempty"`
```

New types:
```go
type InfrastructureFault struct {
    Domain string   `json:"domain"`            // topology domain where detected
    GroupA []string `json:"groupA"`            // one side of the boundary
    GroupB []string `json:"groupB"`            // other side of the boundary
    Stage  string   `json:"stage"`             // which stage triggered detection
}

type CrossBoundaryState struct {
    PendingProbes []CrossBoundaryProbe `json:"pendingProbes,omitempty"`
    OriginStage   string               `json:"originStage"` // for MNNVL inheritance
}

type CrossBoundaryProbe struct {
    Domain     string   `json:"domain"`
    HalfA      []string `json:"halfA"`
    HalfB      []string `json:"halfB"`
    ProbeRound int      `json:"probeRound"`
}
```

### Status Size Impact

Each `InfrastructureFault` is ~200 bytes. `CrossBoundaryState` is transient (cleared after probing). Even with 10 infrastructure faults, this adds < 3 KB — negligible vs the 35 KB budget.

### State Machine

Updated flow:
```
intra-screening → [skip or] intra-screening-no-nvl → inter-screening
    → bisection ⇄ cross-boundary → confirmation → complete
```

Cross-boundary probing is a sub-loop within bisection: detect BHP → probe → record fault → return to bisection for remaining groups.

### Key Files

- `api/v1alpha1/workflow_types.go` — New types and stage constant
- `pkg/controller/workflow_controller.go` — BHP detection in `handleDiagnoseBisection`, new `handleDiagnoseCrossBoundary`
- `pkg/orchestration/diagnose.go` — `BuildCrossBoundaryGroups()`, `DetectBothHalvesPass()`
- `pkg/report/report.go` — Infrastructure fault display

## Rationale

1. **Targeted probing over exhaustive pairwise**: Full pairwise testing of a K-node domain requires O(K²) tests. Cross-boundary probing uses O(log S) tests by leveraging the bisection split as a signal for where the fault boundary lies.

2. **Same algorithm at every layer**: The BHP detection and mixed-group probing pattern is identical for NVSwitch faults (intra-rack), fabric faults (intra-rack without NVL), and spine switch faults (inter-rack). Only the MNNVL setting differs.

3. **No false negatives for node faults**: Cross-boundary probing only activates when bisection clears all suspects. If a node is genuinely faulty, bisection identifies it before BHP detection triggers.

4. **Backward compatible**: New fields are optional with `omitempty`. Existing DiagnoseStatus objects deserialize correctly. The new stage only appears in new workflows.

5. **Minimal wall-clock impact**: BHP is uncommon (most faults are node-level). When it occurs, 2-3 extra rounds add ~8-12 minutes to a process that already takes ~28 minutes.

## Consequences

- Infrastructure faults are now detected and reported, rather than silently cleared
- Report output includes a new "Infrastructure Faults" section with boundary node groups
- Operators can use the boundary information to investigate specific switches/cables
- The algorithm remains near-optimal for node-level faults (zero overhead when no BHP detected)

## References

[1]-[4] See ADR-055.

[5] H. Nikpey, S. Srinivasavaradhan, S. Muthukrishnan, and S. Jaggi, "Group Testing with Correlation Under Edge-Faulty Graphs," IEEE Trans. Inf. Theory, 2024.

[6] A. De Bonis, "Group Testing in Arbitrary Hypergraphs and Related Combinatorial Structures," arXiv:2307.09608, 2023.

[7] Y. Huang, N. Feamster, and R. Teixeira, "Practical Issues with Using Network Tomography for Fault Diagnosis," ACM SIGCOMM CCR, vol. 38, no. 5, pp. 53-58, 2008.
