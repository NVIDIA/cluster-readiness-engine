# ADR-054: Multi-Scale NCCL Cluster Validation

> **Status:** Proposed

## Context

GPU cluster validation requires testing at multiple scales to isolate failures at different layers of the interconnect hierarchy:

1. **Intra-node**: NVLink within each node (all GPUs on a single node)
2. **Intra-rack**: NVLink Switch System (L1) across nodes in a rack
3. **Pairwise/Combinatorial**: Inter-rack fabric, rail mapping, transceiver health
4. **Full-scale**: Spine/core switches, adaptive routing, sustained load under thermal stress

CRE currently runs NCCL tests at a single scale (all target nodes in one group). There is no way to test racks independently, validate pairwise inter-rack links, or control NCCL test parameters (message size, iterations, cycles) per category.

The existing orchestration constructs — topology-aware partitioning, combinatorial grouping, maxConcurrent execution — already support all required partitioning strategies. The gap is exposing these through the catalog and user-facing API.

## Decision

Extend the existing 3 NCCL catalog entries (`nccl-all-reduce`, `nccl-all-gather`, `nccl-alltoall`) with a `testScale` option that controls the orchestration strategy. No new catalog entries are needed.

### New CategoryOptions

| Field | Type | Default | Purpose |
|-------|------|---------|---------|
| `testScale` | `string` | `"full-scale"` | Orchestration mode |
| `maxBytes` | `string` | `"16G"` / `"32G"` (GB200/GB300) | NCCL `-e` max message size |
| `numIterations` | `int32` | `100` | NCCL `-n` timed iterations per message size |
| `numCycles` | `int32` | `10` | NCCL `-N` run cycles (each printed separately) |
| `minBusBandwidthGBps` | `float64` | nil | Auto-fail if measured BusBW is below threshold |
| `maxConcurrent` | `int32` | `0` (unlimited) | Max simultaneous jobs per Workflow |
| `groupSize` | `int32` | `2` | Combinatorial group size (for `testScale=combinatorial`) |

### testScale Modes

| Mode | Orchestration | NodesPerJob | Use Case |
|------|--------------|-------------|----------|
| `intra-node` | Simple partitioning | 1 | Validate NVLink within each node independently |
| `intra-rack` | Topology `nvidia.com/gpu.clique` | All nodes in clique | Validate NVLink Switch System (L1) per rack |
| `pairwise` | Combinatorial groupSize=2 | 2 | Validate every inter-rack node pair |
| `combinatorial` | Combinatorial groupSize=N | Configurable | Arbitrary group sizes for rail testing (4, 8, etc.) |
| `full-scale` | Simple partitioning | All nodes | Validate spine/core, adaptive routing (default) |

### NCCL Test Parameter Changes

- Templatize `-e` (max message size) as `{{ .MaxBytes }}`
- Templatize `-n` (timed iterations) as `{{ .NumIterations }}`
- Templatize `-N` (run cycles) as `{{ .NumCycles }}`
- Add `-d uint8` for alltoall (4x more elements stresses packet routing)
- Add `NCCL_CUMEM_ENABLE=1` for GB200/GB300
- Change all timeouts from 1800s to 3600s

### Generic Performance Thresholds

Thresholds use CEL expressions evaluated against measured metrics. The `thresholds` field
is a `map[string]string` where keys are standardized metric names and values are CEL expressions
using a `value` variable (float64).

```yaml
thresholds:
  busBandwidthGBps: "value >= 900"
  goodputRatio: "value >= 0.85"
  avgTFLOPsPerGPU: "value >= 1000"
  avgStepTimeSec: "value <= 3.0"
```

Available threshold keys:

| Key | Unit | Source |
|-----|------|--------|
| `busBandwidthGBps` | GB/s | BandwidthMeasurement (max across message sizes) |
| `algBandwidthGBps` | GB/s | BandwidthMeasurement (max across message sizes) |
| `goodputRatio` | ratio (0-1) | GoodputMeasurement |
| `avgTFLOPsPerGPU` | TFLOPS | GoodputMeasurement |
| `avgStepTimeSec` | seconds | GoodputMeasurement |

The evaluator uses `cel-go` (already a dependency for node health monitoring via CEL expressions).
Unknown keys are rejected with `ValidationFailed` reason `UnknownThresholdKey`. Invalid CEL
expressions are rejected with reason `InvalidThresholdExpression`.

Thresholds flow: `CategoryOptions.Thresholds` → `BuildConfig.Thresholds` → catalog template →
`WorkflowSpec.Validation.Performance.Thresholds` → propagated to each Job's measurement configs →
evaluated by Job controller after workload success.

## Implementation

### Orchestration Template Logic

Each NCCL catalog YAML generates the orchestration spec based on `testScale`:

```yaml
orchestration:
{{- if eq .TestScale "intra-node" }}
  execution: {}
{{- else if eq .TestScale "intra-rack" }}
  topology:
    topologyKey: nvidia.com/gpu.clique
{{- else if eq .TestScale "pairwise" }}
  combinatorial:
    groupSize: 2
{{- else if eq .TestScale "combinatorial" }}
  combinatorial:
    groupSize: {{ .GroupSize }}
{{- else }}
{{- end }}
{{- if .MaxConcurrent }}
  execution:
    maxConcurrent: {{ .MaxConcurrent }}
{{- else }}
  execution: {}
{{- end }}
  iterations: 1
```

For `intra-node`, the Certification controller sets `nodesPerJob=1` (one group per node). For `intra-rack`, the topology partitioner creates one group per `gpu.clique` domain, and `SetNumNodes` automatically sizes each Job to the group's node count. For `pairwise` and `combinatorial`, the combinatorial generator creates C(N,k) groups.

### Data Flow

```
CategoryOptions.TestScale → BuildConfig.TestScale → TemplateData.TestScale → {{ .TestScale }} in YAML
```

Same pattern as existing options (`enableCheckpoint`, `maxSteps`, etc.).

### Example Certification

```yaml
apiVersion: cre.nvidia.com/v1alpha1
kind: Certification
metadata:
  name: gb200-cluster-validation
spec:
  target:
    nodeSelector:
      nvidia.com/gpu.present: "true"
  categories:
    # Phase 1: Intra-node NVLink validation
    - domain: communication
      variant: nccl-all-reduce
      options:
        testScale: intra-node
        enableMNNVL: true
        thresholds:
          busBandwidthGBps: "value >= 680"
    - domain: communication
      variant: nccl-alltoall
      options:
        testScale: intra-node
        enableMNNVL: true

    # Phase 2: Intra-rack NVLink Switch validation
    - domain: communication
      variant: nccl-all-reduce
      options:
        testScale: intra-rack
        enableMNNVL: true
        thresholds:
          busBandwidthGBps: "value >= 900"
    - domain: communication
      variant: nccl-alltoall
      options:
        testScale: intra-rack
        enableMNNVL: true

    # Phase 3: Pairwise inter-rack validation
    - domain: communication
      variant: nccl-all-reduce
      options:
        testScale: pairwise
        maxConcurrent: 8
    - domain: communication
      variant: nccl-alltoall
      options:
        testScale: pairwise
        maxConcurrent: 8

    # Phase 3b: Combinatorial 4-node rail groups
    - domain: communication
      variant: nccl-all-reduce
      options:
        testScale: combinatorial
        groupSize: 4
        maxConcurrent: 4

    # Phase 4: Full-scale stress test
    - domain: communication
      variant: nccl-all-reduce
      options:
        testScale: full-scale
        maxBytes: "32G"
        numIterations: 10
        numCycles: 10
    - domain: communication
      variant: nccl-alltoall
      options:
        testScale: full-scale
        maxBytes: "32G"
        numIterations: 10
        numCycles: 10
```

Categories execute sequentially (Certification controller processes one Workflow at a time). Within each Workflow, groups execute in parallel (respecting `maxConcurrent`).

### Per-Group Bandwidth Reporting

For multi-group Workflows (pairwise, combinatorial, intra-rack), the `nvcrectl` certification report shows per-group bandwidth results instead of a single aggregate:

```
│  Bandwidth by group:                                           │
│    group-0 (node-1, node-2):  882.51 GB/s BusBW   ✓          │
│    group-1 (node-1, node-3):  879.30 GB/s BusBW   ✓          │
│    group-2 (node-2, node-3):  441.20 GB/s BusBW   ✗ LOW      │
```

For topology-partitioned Workflows (intra-rack), results are grouped by domain:

```
│  Bandwidth by domain:                                          │
│    clique-0 (18 nodes):  923.32 GB/s BusBW   ✓               │
│    clique-1 (18 nodes):  450.20 GB/s BusBW   ✗ LOW           │
```

Groups below `minBusBandwidthGBps` are flagged. Single-group Workflows (full-scale, intra-node) show the existing single-row format.

## Rationale

- **No new catalog entries**: Reuses the existing 3 NCCL entries. The `testScale` option controls orchestration via templates, keeping the catalog small.
- **No new controller logic**: All orchestration modes (topology, combinatorial, simple) are already implemented. Template-level changes only.
- **User-configurable thresholds**: `minBusBandwidthGBps` avoids hardcoding platform-specific baselines. Users set their own targets based on hardware and configuration.
- **Backward compatible**: Default `testScale=full-scale` preserves existing behavior. New options are all optional with sensible defaults.

## Consequences

- NCCL test parameters change from hardcoded values to template variables. Default `-N` changes from `64` to `10` (numCycles), and `-n` (numIterations) is explicitly set to `100` (was NCCL default of 20). This changes test duration and results for existing users.
- alltoall tests switch from default `float` to `-d uint8`. This changes bandwidth numbers for alltoall comparisons with prior runs.
- All timeouts increase from 1800s to 3600s. Tests take longer to time out on failure.

## Alternatives Considered

- **Separate catalog entries per phase** (e.g., `nccl-intra-rack`, `nccl-pairwise`): More explicit but duplicates 90% of the NCCL config across entries. Template variables are cleaner.
- **New controller-level "phases" concept**: Adds complexity to the Workflow controller. Certification's sequential category execution already provides phase ordering.
- **Hardcoded performance baselines**: Would require maintaining per-GPU/per-interconnect reference values. User-configurable thresholds are more flexible.

## References

- ADR-014: Integration tests with golden file comparison
- ADR-049: Kind + KWOK UAT tests
- Orchestration: `pkg/orchestration/partition.go`, `pkg/orchestration/combinatorial.go`
- Workflow controller: `pkg/controller/workflow_controller.go` (partitioning, group execution, maxConcurrent)
