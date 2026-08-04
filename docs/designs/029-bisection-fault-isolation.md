# ADR-029: Bisection Fault Isolation

> **Status: Superseded and removed.** The standalone `spec.orchestration.bisection`
> field, `BisectionSpec`/`BisectionStatus` types, and `testScale: bisection` have been
> removed from the API. Binary-search fault isolation is now provided exclusively as
> **Stage 2 of Adaptive Fault Isolation** ([ADR-055](055-adaptive-fault-isolation.md)),
> which composes bisection with topology-aware intra/inter-domain screening and
> buddy-pair confirmation. The underlying algorithm (`pkg/orchestration/bisect.go`)
> is retained and used by AFI. This ADR is preserved for historical context.

## Context

When a multi-node workload fails on a large cluster, the failure could be caused by any subset of the participating nodes — a bad GPU, a faulty NIC, a misconfigured driver. The operator knows *something* is wrong but has no localization. Running the workload on all N nodes and observing failure provides no actionable information about *which* nodes are at fault.

The existing orchestration strategies (partition, topology-aware, combinatorial) generate a fixed set of groups at partition time and run them statically. None of them support **adaptive group generation** where subsequent groups depend on the results of previous groups. A binary search approach — testing all nodes, then halving the failing set, then halving again — can isolate faulty nodes in O(log N) rounds, which is far more efficient than testing each node individually (O(N)) or testing all pairs (O(N²)).

## Decision

Add a `bisection` orchestration strategy that implements binary-search fault isolation. The controller starts with a single group spanning all target nodes. If the job fails, it splits the node set in half and runs each half. Succeeded halves are marked healthy; failed halves are split again. This continues until groups reach a user-specified `minGroupSize`, at which point the remaining failing groups identify the suspect nodes.

The bisection strategy supports an optional `topologyKey` for topology-aware splitting. When set, bisection splits by topology domains (e.g., racks) rather than individual nodes, ensuring that nodes within the same domain stay together during splits. This is critical for workloads like NCCL benchmarks where intra-domain topology must be preserved.

## Implementation

### API

Add `BisectionSpec` to `OrchestrationSpec`:

```go
type BisectionSpec struct {
    // minGroupSize is the smallest group size at which bisection stops.
    // Groups at this size that still fail contain suspect faulty nodes.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Required
    MinGroupSize int `json:"minGroupSize"`

    // topologyKey is the node label key for topology-aware bisection.
    // When set, bisection splits by topology domains rather than individual nodes.
    // Nodes within the same domain stay together during splits.
    // When empty, nodes are split by name order.
    // +optional
    TopologyKey string `json:"topologyKey,omitempty"`
}
```

Mutually exclusive with `Combinatorial` (CEL validation rule). Not mutually exclusive with `Topology` — bisection has its own `topologyKey` field, keeping the configuration self-contained.

Add `BisectionStatus` to `OrchestrationStatus`:

```go
type BisectionStatus struct {
    CurrentRound int      `json:"currentRound"`
    HealthyNodes []string `json:"healthyNodes,omitempty"`
    SuspectNodes []string `json:"suspectNodes,omitempty"`
    Converged    bool     `json:"converged,omitempty"`
}
```

### Example

```yaml
spec:
  orchestration:
    target:
      nodeSelector:
        nvidia.com/gpu.present: "true"
    bisection:
      minGroupSize: 2
      topologyKey: nvidia.com/gpu.clique
  jobTemplate:
    spec:
      workload:
        trainJob:
          trainer:
            numNodes: 8  # overridden dynamically per round
```

With 8 target nodes across 4 racks:

```
Round 0: [all 8 nodes]           → FAIL
Round 1: [R1+R2: 4 nodes]        → PASS (healthy)
         [R3+R4: 4 nodes]        → FAIL
Round 2: [R3: 2 nodes]           → PASS (healthy)
         [R4: 2 nodes]           → FAIL (suspect — at minGroupSize)
Result: R4 nodes are suspect, all others healthy
```

### Bisection algorithm

New file `pkg/orchestration/bisect.go` with a pure function:

```go
func Bisect(input BisectInput) BisectResult
```

**Without topology** (`TopologyKey == ""`): Sort nodes by name, split at midpoint.

**With topology** (`TopologyKey != ""`): Group nodes by topology domain label value. Sort domains by name. Split the domain list at midpoint — each half gets all nodes from its domains. If a group spans a single domain and has more nodes than `minGroupSize`, fall back to node-level split within that domain.

`minGroupSize` is always measured in **nodes**, not domains. Splitting stops when a group has ≤ `minGroupSize` nodes.

### Dynamic group replacement

This is the key architectural change. The existing `handleIterationComplete()` resets group phases but keeps the same groups. For bisection, a new `handleBisectionRoundComplete()` method:

1. Collects succeeded group nodes → `bisectionStatus.HealthyNodes`
2. Collects failed groups → calls `Bisect()` → **replaces** `orch.Groups` with new halved groups (all Pending)
3. Updates `orch.TotalGroups` and `orch.CurrentIteration`
4. If converged → sets suspect nodes → Workflow Failed
5. If no failures → Workflow Succeeded

The existing `launchPendingGroups()` and `updateStatusFromJobs()` work unchanged — they operate generically on `[]GroupStatus`.

### Dynamic numNodes override

Each bisection round produces smaller groups. The workload template's `numNodes` must match the current group size. A new `SetNumNodes(spec, numNodes)` method on the `Adapter` interface allows the controller to override the replica count when creating jobs for bisection groups.

### Files modified

- `api/v1alpha1/workflow_types.go` — `BisectionSpec`, `BisectionStatus`, CEL rule
- `pkg/orchestration/bisect.go` — new: `Bisect()` function with node and topology modes
- `pkg/orchestration/bisect_test.go` — new: 11 golden file test cases
- `pkg/workload/adapter.go` — `SetNumNodes` interface method
- `pkg/workload/trainjob.go`, `pytorchjob.go`, `mpijob.go`, `tfjob.go`, `jaxjob.go` — implement `SetNumNodes`
- `pkg/workload/kubeflow_helpers.go` — `setWorkerReplicas` helper
- `pkg/controller/workflow_controller.go` — bisection branches in `discoverAndPartition()`, `handleIterationComplete()`, `createJobForGroup()`

## Rationale

- **Binary split only**: Always halves. Simpler than a configurable split factor, well-understood O(log N) behavior, and minimizes the number of concurrent jobs per round.
- **`topologyKey` on `BisectionSpec`**: Self-contained — bisection controls its own topology awareness without depending on the separate `TopologySpec` field. Avoids implicit coupling between two optional fields.
- **Reuse of iteration loop**: The existing iteration mechanism (`CompletedIterations`, `handleIterationComplete()`) provides the control flow. Bisection rounds map to iterations, with `handleBisectionRoundComplete()` replacing the group list between rounds instead of resetting phases.
- **No per-node result tracking**: Failed groups are bisected; succeeded groups are marked healthy. No complex per-node state matrix. The suspect nodes are simply those in groups that failed at `minGroupSize`.
- **`SetNumNodes` adapter method**: Each round has different group sizes, requiring the workload's replica count to change. A dedicated adapter method is cleaner than mutating raw JSON or making users configure this manually.

## Consequences

**Positive:**
- Isolates faulty nodes in O(log N) rounds — far more efficient than exhaustive testing
- Topology-aware splitting preserves domain locality for workloads that need it
- Generic — works for any workload type, not just NCCL
- Reuses the existing group lifecycle (`launchPendingGroups`, `updateStatusFromJobs`)
- `BisectionStatus` provides clear observability: healthy nodes, suspect nodes, round count

**Negative:**
- First use of dynamic group replacement — `orch.Groups` was previously immutable after partition
- `SetNumNodes` adds a new method to the `Adapter` interface — all 5 adapters must implement it
- The `iterations` field on `OrchestrationSpec` is effectively ignored for bisection (rounds are algorithm-determined)
- Large clusters may produce many concurrent groups in early rounds (mitigated by `maxConcurrent`)

## Alternatives Considered

1. **Configurable split factor (K-way split)**: Split into K subgroups instead of 2. Faster isolation but more concurrent jobs and harder to reason about. Binary split is the well-understood baseline; K-way can be added later if needed.
2. **Per-node result tracking**: Maintain a status map of node → pass/fail across rounds. More observability but significant status complexity. The simpler "just bisect failures" model was chosen.
3. **Topology and Bisection as separate fields**: Allow `spec.orchestration.topology` to work with bisection. Rejected because it creates implicit coupling — `BisectionSpec.topologyKey` is self-contained and clearer.

## References

- ADR-007: Topology-Aware Multi-Group Orchestration
- ADR-028: Combinatorial Node Grouping
- [Git Bisect Algorithm](https://git-scm.com/docs/git-bisect) — same O(log N) binary search principle applied to commits
- `pkg/orchestration/partition.go` — existing Group struct reused by bisection
