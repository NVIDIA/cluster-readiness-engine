# ADR-007: Feature — Topology-Aware Multi-Group Orchestration

## Context

GPU clusters are not flat. Nodes are organized by NVLink cliques, network switch domains, and rack boundaries. A benchmark that needs 8 GPUs should run on 8 GPUs connected by NVLink, not 8 GPUs spread across different switches. Burn-in testing must respect these topology boundaries.

A single Workflow might target 64 nodes but the benchmark requires 8 nodes per run. The Workflow needs to partition the target nodes into groups, run the benchmark against each group, and aggregate results.

Options considered:
1. User manually creates one Workflow per group
2. Workflow-level multi-group support with simple name-sorted chunking
3. Workflow-level multi-group with topology-aware partitioning

## Decision

Implement multi-group orchestration in the Workflow controller with two partitioning strategies: simple (name-sorted chunking) and topology-aware (greedy domain-based allocation using node labels).

## Implementation

- **OrchestrationSpec** (`api/v1alpha1/workflow_types.go`): `GroupingSpec` with `strategy` (all-at-once, topology-aware) and `nodesPerGroup` count. `topologyKey` specifies the node label used for domain grouping (e.g., `topology.kubernetes.io/zone`, `nvidia.com/nvlink-domain`).
- **Partitioning package** (`pkg/orchestration/partition.go`): Two strategies:
  - **Simple**: Sort nodes by name, chunk into groups of `nodesPerGroup`. Last group borrows from overflow if undersized.
  - **Topology-aware**: Group nodes by topology label value. Greedy allocation ensures each group contains nodes from the same domain. Supports multi-domain groups when a single domain is too small.
- **Workflow controller** (`pkg/controller/workflow_controller.go`): Discovers target nodes from nodeSelector or nodeNames. Partitions into groups. Creates one Job per group with the group's nodes set via node affinity. Runs groups according to `execution` policy (concurrent or sequential).

Node filtering:
- Only GPU-equipped nodes (`nvidia.com/gpu.present=true`) are included
- Nodes matching the target selector but lacking GPUs are skipped

Group naming: Jobs are named `<workflowName>-job` for single-group, `<workflowName>-group-<index>-job` for multi-group.

Golden file tests cover 6 scenarios: exact fit, overflow borrowing, single-node jobs, multi-domain topology, and error cases for invalid inputs.

## Rationale

- **Topology correctness.** NVLink-connected GPUs within a clique outperform GPUs across switches by 10-100x for collective operations. Running NCCL benchmarks across topology boundaries gives meaningless results. Topology-aware grouping ensures benchmarks test the right hardware boundaries.
- **Automatic partitioning.** Users specify `nodesPerGroup` and `topologyKey`. The controller handles the partitioning math. No manual group creation, no risk of miscounting nodes.
- **Standard Kubernetes labels.** Topology keys use existing node labels (`topology.kubernetes.io/zone`, vendor-specific labels). No custom labeling required.
- **Overflow borrowing.** Real clusters don't always divide evenly. Allowing the last group to be undersized (borrowing missing nodes from the previous group) ensures all nodes are tested even with uneven counts.

## Consequences

### Positive
- A single Workflow can test an entire 64-node cluster with 8-node NCCL benchmarks
- Topology boundaries are respected automatically
- Golden file tests make partitioning logic easy to verify and regression-proof
- Works with any node label, not just NVIDIA-specific labels

### Negative
- Partitioning happens at Workflow creation time — adding nodes mid-run requires a new Workflow
- Sequential group execution can be slow for large clusters
- Topology-aware partitioning requires nodes to be pre-labeled

### Mitigations
- Concurrent execution mode runs all groups simultaneously for speed
- Node labeling is typically handled by the GPU Operator or node provisioning
- Iteration support (ADR-002) allows re-running each group multiple times

## Alternatives Considered

### User manually creates Workflows per group
**Rejected** because: Error-prone for large clusters. A 64-node cluster with 8-node groups requires 8 Workflows. Users must manually partition nodes, avoid overlaps, and aggregate results. This is exactly the work the controller should automate.

### Scheduler-based placement (topology spread constraints)
**Rejected** because: Kubernetes topology spread constraints control pod placement, not Job grouping. They can spread pods across domains but can't group a set of pods to run together in the same domain. Burn-in needs explicit group-level control.

## Notes

- The `all-at-once` strategy runs all target nodes as a single group (no partitioning)
- Workflow test specs must include the `Grouping` field (required by CRD validation)
- GPU node filtering uses `nvidia.com/gpu.present=true` label, not GPU resource requests

## References

- `pkg/orchestration/partition.go` — partitioning strategies
- `pkg/orchestration/partition_test.go` — golden file tests
- `pkg/controller/workflow_controller.go` — multi-group Job creation
