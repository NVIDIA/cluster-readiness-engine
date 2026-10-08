# ADR-091: Per-GPU GEMM Burn-In as a `compute` Catalog Entry

> **Status:** Proposed

## Context

ADR-000 lists four ClusterMAX 2.0 validation categories and marks Raw Compute and Thermal Stress as **Supported**, on the grounds that "GEMM benchmarks are already supported as TrainJob workloads and can be added to the catalog as entries under a `compute` domain" (`docs/designs/000-adr.md:698`). ADR-001 repeats the claim in its capability table (`docs/designs/001-adr-abridged.md:184`).

The second half of that sentence is true. The first half is not, in the sense a reader will take it. There is no `compute` domain. `pkg/catalog/entries/` holds exactly three: `communication`, `diagnostics`, `training`. The catalog is embedded at compile time (`//go:embed all:entries`, `pkg/catalog/loader.go:86`), so an operator cannot add a GEMM entry without building a new binary. Issue #323 closed on the same confusion for custom entries generally. Today a GEMM burn-in is reachable through `WorkloadRun`, which takes a user-supplied workload spec, and not through a `Certification` category.

Raw compute is also the one category where per-node reporting loses the signal. A node-level TFLOPS average passes when seven GPUs are healthy and the eighth is clocked down by a thermal fault. That is the failure operators most want a burn-in to find, and it is invisible at the granularity NVCRE reports today.

NVCRE does have a TFLOPS threshold key. `avgTFLOPsPerGPU` is in the threshold registry (`pkg/threshold/evaluator.go:34`), but its value comes from `GoodputMeasurement.Status.AvgTFLOPSPerGPU` (`pkg/controller/job_threshold_helpers.go:93`), which is one scalar per Job parsed from training step logs. The name says per GPU; the quantity is a per-Job average divided by a GPU count. It cannot name the slow GPU.

[ADR-090](090-component-result-sharding.md) settles where per-component result rows are stored and how they leave the cluster. This record is the first catalog entry that produces them, and it is the case the ADR-090 design was proved against.

## Decision

**1. Add a `compute` domain with a `cutlass-gemm` variant.** `pkg/catalog/entries/compute/cutlass-gemm.yaml`, registered by the existing walker. The domain is an open set: `parsePath` derives it from the file path (`pkg/catalog/loader.go:835-847`) and `Register` keys a map on it (`pkg/catalog/catalog.go:203-204`). `CertificationSpec.Domain` is a plain `string` with no enum marker (`api/v1alpha1/certification_types.go:51`). Adding a domain is additive and needs no CRD change.

**2. The entry is per node: `numNodes: 1`.** It follows `diagnostics/dcgm-level4` (`dcgm-level4.yaml:71`) rather than the communication or training entries. A GEMM burn-in is embarrassingly parallel, so a multi-node Job buys nothing and costs fault attribution: one slow GPU in a 64-node Job fails all 64 nodes.

**3. Results are emitted per GPU, through ADR-090's mechanism.** One row per GPU per sample, carrying the GPU UUID, the node, achieved TFLOPS, and an outcome. Not through `GoodputMeasurement`, whose per-Job scalar is the thing this entry exists to improve on.

**4. No new threshold registry key.** `Registry` is a closed five-key vocabulary and `ValidateKeysError` rejects anything outside it (`pkg/threshold/evaluator.go:30-36`, `pkg/controller/job_controller.go:1061`). Every key in it is a per-Job scalar, which is the shape this entry deliberately does not produce. The per-component verdict rule belongs to the ADR-090 follow-on that defines the declaration vocabulary, and inventing a scalar key here would be vocabulary that record then has to deprecate.

**5. Correct the "Supported" claim.** `docs/designs/000-adr.md:698` and `docs/designs/001-adr-abridged.md:184` change to describe GEMM as reachable through `WorkloadRun` and planned as a catalog entry, matching how the same table already marks RL, inference and finetuning as planned.

## Implementation

### Shard arithmetic

ADR-090's bound is 768 KiB of gzip output per shard, with a measured 204 B row and 18.4x compression in node-major emission order. At `numNodes: 1`, Job count equals node count, so a 1000-node fleet produces 1000 Jobs and 1000 shards, the same object count as `diagnostics/dcgm-level4`.

| Emission | Rows per node | Raw | Gzipped | Shards per node |
|---|---|---|---|---|
| One row per GPU, H100 (`gpusPerNode: 8`) | 8 | 1.6 KiB | ~89 B | 1 |
| One row per GPU, GB200 (`gpusPerNode: 4`) | 4 | 816 B | ~44 B | 1 |
| Sampled at 1 Hz for 30 min, H100 | 14,400 | 2.9 MiB | ~160 KiB | 1 |

GPU counts are from `pkg/catalog/entries/_lib/gpu-defaults.yaml:24-52`. A node needs about 72,600 rows to fill a shard, which at 8 GPUs is roughly 2.5 hours of 1 Hz sampling. This entry sits well inside the range ADR-090 already covers and exposes no case that design does not handle.

### Entry shape

The trainer runs a GEMM harness under `numProcPerNode: 1`, with `nvidia.com/gpu` requested at `{{ .GpusPerNode }}` so one process addresses every GPU on the node and can attribute each result to a UUID. This is the `dcgm-level4` shape (`dcgm-level4.yaml:31-35`).

The image is not pinned in this record. It needs a container carrying a CUTLASS profiler build or an equivalent harness that reports per-device achieved TFLOPS on stdout, and the tag is chosen and verified at implementation the way ADR-057 pinned the DCGM tag.

`meta.yaml` carries `timeoutPerJob`, since the catalog-wide 1h default is sized for short runs and a sustained thermal soak is the point of this entry.

### Tests

A rendered-output case under `pkg/catalog/testdata/` through `testutil.TestCaseParser`, and a UAT golden under `test/uat/testdata/<csp>/<gpu>/` following the existing NCCL layout.

## Rationale

The domain is the right unit because ClusterMAX treats raw compute as its own category and the catalog already maps one domain to one capability class. Putting GEMM under `diagnostics` would group an active stress workload with a vendor diagnostic tool and make the `compute` claim in ADR-000 permanently false.

Per node rather than per fleet is what makes the result actionable. The burn-in exists to find the one GPU that is slow, and both the Job boundary and the result row have to be narrow enough to say which.

Declining to add a threshold key is the part most likely to be questioned, so to be direct about the cost: until the follow-on record lands, this entry fails only when the workload fails, not when a GPU is merely slow. The rows are still in the bundle and an operator can gate on them with their own tooling, which is exactly the egress path the customer asked for. The alternative is shipping a scalar key that averages away the signal this entry was built to surface.

## Consequences

### Positive

- The `compute` category in ADR-000's ClusterMAX table becomes true rather than aspirational.
- First catalog entry producing per-component rows, which exercises ADR-090 end to end on a real workload rather than a synthetic one.
- Per-node Jobs give per-node fault attribution for free through the existing failed-node recording.

### Negative

- A 1000-node run creates 1000 Jobs and 1000 shard ConfigMaps. Same object count as `dcgm-level4` at that scale, and the open question on ADR-090's PR about coalescing shards per Workflow applies here identically.
- No automatic pass or fail on a slow GPU until the per-component verdict record lands. Stated above; repeated here so it is not discovered late.
- This entry cannot ship before ADR-090, since it has nowhere to put its rows.

## Alternatives Considered

### A variant under `diagnostics`

Rejected. `diagnostics` is DCGM today, meaning vendor tooling that reports its own verdict. A GEMM burn-in is an active stress workload whose verdict NVCRE computes. Filing it there keeps ADR-000's `compute` claim false indefinitely.

### Multi-node Jobs with `nodesPerJob`

Rejected. There is no collective in a GEMM burn-in, so grouping only widens the blast radius of one bad GPU.

### Reuse `GoodputMeasurement` and `avgTFLOPsPerGPU`

Rejected. That path parses training step logs into one per-Job scalar (`pkg/controller/job_threshold_helpers.go:88-96`). It would make the entry gateable today at the cost of the per-GPU granularity that motivates it, which is the whole requirement.

### Add a per-GPU pattern to `LogProfile`

Rejected for now. `LogPatternSet` is a fixed set of named optional patterns (`api/v1alpha1/logprofile_types.go:69-113`), each mapping to one known event. A per-component result is not one more named event; it is the generic declaration mechanism the ADR-090 follow-on defines. Bolting a `gemmResult` pattern alongside `bandwidthResult` would be the fixed-field regex approach the customer asked to move away from.

### Leave GEMM to `WorkloadRun`

Rejected as the end state, though it is accurate as the current state. `WorkloadRun` runs the workload but puts the burden of writing the spec on the operator, and a certification category is what makes the capability a product claim rather than a recipe.

## References

- [ADR-090: Sharded Component Results and the Report Bundle](090-component-result-sharding.md)
- [ADR-000: NVCRE Architecture for GPU Cluster Certification](000-adr.md), ClusterMAX alignment table
- [ADR-010: Certification Catalog with init() Registration](010-certification-catalog.md)
- [ADR-016: NCCL All-Reduce Certification Catalog Entry](016-nccl-all-reduce-catalog.md)
- [ADR-057: DCGM Level-3 Diagnostics, A100 Configuration](057-dcgm-level3-a100.md)
- [ADR-021: Performance Threshold Enforcement](021-performance-threshold-enforcement.md)
