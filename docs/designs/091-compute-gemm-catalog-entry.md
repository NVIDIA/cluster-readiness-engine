# ADR-091: Per-GPU GEMM Burn-In as a `compute` Catalog Entry

> **Status:** Proposed

## Context

ADR-000 says GEMM benchmarks "are already supported as TrainJob workloads and can be added to the catalog as entries under a `compute` domain" (`docs/designs/000-adr.md:698`). That sentence is honest: the second half is future tense and says so. ADR-001's capability table flattens it to an unqualified **Supported** (`docs/designs/001-adr-abridged.md:184`), and the sibling rows there mark RL, inference and finetuning as planned, so the table does express partial states when it means to.

Taken as "Supported", it is not. There is no `compute` domain. `pkg/catalog/entries/` holds exactly three: `communication`, `diagnostics`, `training`. The catalog is embedded at compile time (`//go:embed all:entries`, `pkg/catalog/loader.go:86`), so an operator cannot add a GEMM entry without building a new binary. Issue #323 closed on the same confusion for custom entries generally. Today a GEMM burn-in is reachable through `WorkloadRun`, which takes a user-supplied workload spec, and not through a `Certification` category.

Raw compute is also the one category where per-node reporting loses the signal. A node-level TFLOPS average passes when seven GPUs are healthy and the eighth is clocked down by a thermal fault. That is the failure operators most want a burn-in to find, and it is invisible at the granularity NVCRE reports today.

`diagnostics/dcgm-level4` is the closest thing already in the catalog and it does not close this. It runs `targeted_stress` and `targeted_power` for 1200s each, one node per Job (`dcgm-level4.yaml:54-72`), which is real GPU stress. But both plugins judge on the peak: Targeted Stress fails when "maximum achieved performance is below the effective `target_perf_min_ratio * target_stress` threshold", and Targeted Power fails when "maximum observed board power is below `target_power_min_ratio * target_power`". A GPU that hits its target in the first minute and degrades for the next nineteen passes both, unless it also trips a thermal violation. Sustained per-GPU performance over the length of a soak is the gap, and it is the gap a burn-in is for.

NVCRE does have a TFLOPS threshold key. `avgTFLOPsPerGPU` is in the threshold registry (`pkg/threshold/evaluator.go:34`), but its value comes from `GoodputMeasurement.Status.AvgTFLOPSPerGPU` (`pkg/controller/job_threshold_helpers.go:93`), which is one scalar per Job parsed from training step logs. The name says per GPU; the quantity is a per-Job average divided by a GPU count. It cannot name the slow GPU.

[ADR-090](090-component-result-sharding.md) settles where per-component result rows are stored and how they leave the cluster. This record is the first catalog entry that produces them, and it is the case the ADR-090 design was proved against.

## Decision

**1. Add a `compute` domain with a `cutlass-gemm` variant.** `pkg/catalog/entries/compute/cutlass-gemm.yaml`, registered by the existing walker. The domain is an open set: `parsePath` derives it from the file path (`pkg/catalog/loader.go:835-847`) and `Register` keys a map on it (`pkg/catalog/catalog.go:203-204`). The field an operator writes, `CertificateCategory.Domain`, is a plain `string` carrying only `+kubebuilder:validation:Required` (`api/v1alpha1/certification_types.go:417-420`), and the generated CRD has no enum on it. Adding a domain is additive and needs no CRD change.

**2. The entry is per node: `numNodes: 1`.** It follows `diagnostics/dcgm-level4` (`dcgm-level4.yaml:71`) rather than the communication or training entries. A GEMM burn-in is embarrassingly parallel, so a multi-node Job buys nothing and costs fault attribution: one slow GPU in a 64-node Job fails all 64 nodes.

**3. Results are emitted per GPU, through ADR-090's mechanism.** One row per GPU per sample, carrying the GPU UUID, the node, achieved TFLOPS, and an outcome. Not through `GoodputMeasurement`, whose per-Job scalar is the thing this entry exists to improve on.

A sample needs an identity, or the rows are an unordered bag. ADR-090's `index` is the GPU's device index within the node, which is what makes a row addressable to a component; it is not a sample counter, and the two must not be conflated. The entry emits a monotonic sample ordinal starting at zero, scoped to one Job attempt, carried on the row alongside a wall-clock timestamp. Every GPU on the node shares the ordinal for a given sweep, which is what makes "GPU 3 fell behind at sample 900 while the other seven held" a question the rows can answer. The field name belongs to the row schema ADR; what this record fixes is that the ordinal exists, that its scope is the attempt, and that it is distinct from `index`.

**4. No new threshold registry key, and the harness owns the verdict.** `Registry` is a closed five-key vocabulary and `ValidateKeysError` rejects anything outside it (`pkg/threshold/evaluator.go:30-36`, `pkg/controller/job_controller.go:1061`). Every key in it is a per-Job scalar, which is the shape this entry deliberately does not produce. The per-component verdict rule belongs to the ADR-090 follow-on that defines the declaration vocabulary, and inventing a scalar key here would be vocabulary that record then has to deprecate.

That leaves a gap this entry cannot ship with: a `compute` category whose verdict cannot reflect a slow GPU is not a certification. So the harness checks each GPU's sustained result against a per-architecture floor and exits non-zero when any GPU is below it. A non-zero exit fails the Job, and the existing failed-node path records the node with a reason, which is the same mechanism every other entry already relies on. The rows name the GPU; the exit code fails the node. The floor is a template parameter on the entry, defaulted per GPU architecture the way the rest of the catalog defaults, so an operator can set it without a rebuild. When the per-component verdict record lands, it replaces the harness's judgement with a declared rule and the floor moves into the spec. Until then the capability is real rather than advisory.

**5. Correct the "Supported" claim in ADR-001.** `docs/designs/001-adr-abridged.md:184` changes to mark GEMM as reachable through `WorkloadRun` and planned as a catalog entry, matching how the same table already marks RL, inference and finetuning. ADR-000:698 needs no change; it already says "can be added".

## Implementation

### Shard arithmetic

At `numNodes: 1`, Job count equals node count, so a 1000-node fleet produces 1000 Jobs and 1000 shards, the same object count as `diagnostics/dcgm-level4`. Every shard holds exactly one node's rows, which is the smallest shape ADR-090 covers.

ADR-090's measured sizing table includes this entry's corpus directly: one node and 8 GPUs sampled at 1 Hz for 30 minutes is 14,400 rows and 86.4 KiB of gzip output, against a 768 KiB roll bound. A 2 hour soak is 343.4 KiB and still one shard. GPU counts are from `pkg/catalog/entries/_lib/gpu-defaults.yaml:24-52`; GB200 and GB300 at 4 GPUs per node produce half the rows of H100 for the same duration, so H100 is the bounding case. This entry stays well under the bound at any soak length an operator would plausibly run, and exposes no case ADR-090 does not handle.

One thing it does expose is the limit of emission order. A single-node shard gets no compression benefit from sorting on node, measured at 1.00x in ADR-090's table, which is why that record re-anchors the normative order on multi-node shards and on bundle determinism rather than on a flat per-shard gain.

### Entry shape

The trainer runs a GEMM harness under `numProcPerNode: 1`, with `nvidia.com/gpu` requested at `{{ .GpusPerNode }}` so one process addresses every GPU on the node and can attribute each result to a UUID. This is the `dcgm-level4` shape (`dcgm-level4.yaml:31-35`).

**Every GPU on the node is loaded concurrently, for the full duration.** This is a requirement on the harness, not an implementation detail. A harness that walks the devices one at a time would report eight per-GPU numbers that each look fine, because it never puts the chassis under the power and thermal load that makes a marginal GPU fail. Sequential per-device benchmarking is a different test from a burn-in and would not find what this entry exists to find.

The image is not pinned in this record. It needs a container carrying a CUTLASS profiler build or an equivalent harness that reports per-device achieved TFLOPS on stdout, and the tag is chosen and verified at implementation the way ADR-057 pinned the DCGM tag.

`meta.yaml` carries `timeoutPerJob`, since the catalog-wide 1h default is sized for short runs and a sustained thermal soak is the point of this entry.

### Tests

A rendered-output case under `pkg/catalog/testdata/` through `testutil.TestCaseParser`, and a UAT golden under `test/uat/testdata/<csp>/<gpu>/` following the existing NCCL layout.

## Rationale

The domain is the right unit because ClusterMAX treats raw compute as its own category and the catalog already maps one domain to one capability class. Putting GEMM under `diagnostics` would group an active stress workload with a vendor diagnostic tool and make the `compute` claim in ADR-000 permanently false.

Per node rather than per fleet is what makes the result actionable. The burn-in exists to find the one GPU that is slow, and both the Job boundary and the result row have to be narrow enough to say which.

Declining to add a threshold key is the part most likely to be questioned, so to be direct about where the verdict lives in the meantime. The threshold registry evaluates per-Job scalars, and the only scalar available here is an average that hides exactly the GPU this entry is hunting. Putting the per-GPU floor in the harness instead gets a real pass or fail today through the existing failed-node path, without adding a key the follow-on record would have to deprecate. The cost is that the floor is a template parameter rather than a declared threshold, so it does not appear in the threshold surfaces an operator already knows, and that is the thing the follow-on fixes.

## Consequences

### Positive

- Raw compute becomes a certification category rather than a recipe an operator writes themselves, which is what ADR-001's table already claims.
- First catalog entry producing per-component rows, which exercises ADR-090 end to end on a real workload rather than a synthetic one.
- Per-node Jobs give per-node fault attribution for free through the existing failed-node recording.

### Negative

- A 1000-node run creates 1000 Jobs and 1000 shard ConfigMaps. Same object count as `dcgm-level4` at that scale, and ADR-090 rejects coalescing them for reasons that apply here identically.
- The per-GPU floor lives in the harness and in entry template parameters, not in the threshold surfaces. It is a real verdict, but an operator looking for it where thresholds normally live will not find it until the follow-on record moves it there.
- This entry cannot ship before ADR-090, since it has nowhere to put its rows.

## Alternatives Considered

### A variant under `diagnostics`, or tuning `dcgm-level4` instead

Rejected, and this is the alternative with the strongest case, since `dcgm-level4` already soaks each GPU for 1200s per plugin on one node per Job. Two reasons it is not the same test. Both DCGM plugins judge on the peak, as quoted in the Context, so a GPU that degrades through the soak passes; raising the durations extends the window without changing what is compared. And `diagnostics` is vendor tooling reporting its own verdict, where a GEMM burn-in is an active stress workload whose sustained per-GPU result NVCRE judges. Filing it under `diagnostics` would also keep ADR-000's `compute` claim false indefinitely.

### DCGM `--json` as the first producer of component rows

Not rejected, and arguably it should come first. `dcgm-level4` already passes `--json` (`dcgm-level4.yaml:60`), so its per-GPU field values are structured output NVCRE throws away today. Turning that into component rows needs no new image, no new domain and no invented verdicts, since DCGM states its own per-GPU results. It is a cheaper first exercise of ADR-090 than this entry is. It is a separate record because it still needs the ingestion path and an outcome for a skipped plugin, and because it does not close the sustained-performance gap: it reports DCGM's peak-based verdict faithfully, which is the verdict this entry exists to go past.

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
