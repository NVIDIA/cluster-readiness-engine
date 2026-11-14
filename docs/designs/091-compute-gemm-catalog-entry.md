# ADR-091: CUTLASS GEMM Per-GPU Burn-In as a `compute` Catalog Entry

> **Status:** Accepted

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

**The harness is the CUTLASS profiler.** `cutlass_profiler` runs the GEMM kernels NVIDIA tunes for each architecture and reports achieved throughput per device, which is both the number this entry needs and a number the vendor stands behind, so a result the customer disputes is checkable against a published tool rather than against something we wrote. That is why the variant is named `cutlass-gemm` and not `gemm`: the harness is part of what the entry certifies, in the same way ADR-057 names DCGM rather than "a diagnostic".

The image tag is not pinned in this record. It needs a container carrying a CUTLASS profiler build that reports per-device achieved TFLOPS on stdout, and the tag is chosen and verified at implementation the way ADR-057 pinned the DCGM tag. An equivalent harness meeting the same per-device reporting contract would satisfy the design, but it would be a different entry under a different variant name rather than a substitution inside this one.

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

## Amendments

These record where the implementation departs from the decisions above, and why. Each was found by building the thing.

### The verdict is DCGM's, including the statistic, and that supersedes the Context

This amendment overrides the Context and the first *Alternatives Considered* entry. Read it before either.

The record above is built on one claim: that `dcgm-level4` judges on the peak, so "a GPU that hits its target in the first minute and degrades for the next nineteen passes", and that "sustained per-GPU performance over the length of a soak is the gap, and it is the gap a burn-in is for". The first implementation did exactly that, comparing each GPU's minimum sample against the floor.

That is no longer what this entry does. The verdict is DCGM's rule unchanged:

```
maxVal < discountMultiplier * target_perf_min_ratio * target_stress
```

every part of it, including `maxVal`: the **maximum** achieved throughput over the run. A GPU that peaks above the floor and decays through the soak passes, exactly as it does under DCGM.

`discountMultiplier` is 1, and that is the faithful value rather than an omission. DCGM's factor discounts the share of its measured interval spent copying over PCIe instead of doing GEMM work, because its own `maxVal` includes that copy time. `cutlass_profiler` reports throughput for the GEMM kernel alone, so there is no copy time in this `maxVal` to discount, and applying one anyway would put the bar below DCGM's.

**What this gives up, stated plainly.** The sustained-degradation case named in the Context is no longer caught by the verdict. It is still *reported*: the per-GPU rows carry every sample, the summary prints each GPU's maximum and minimum, and a GPU whose maximum clears the floor while its minimum does not gets an explicit note saying so and why it passed. So the evidence is complete and the gate is DCGM's. An operator who wants the stricter line reads the rows, or sets `gemmFloorTFLOPS` above the degraded level.

**The report names the GPUs the verdict passes.** The first version of this change left the degradation visible only in the result bundle and in pod logs, which meant a run could pass with a GPU at two thirds of its floor and the artifact a customer reads would say nothing. That is not an acceptable place to leave it, so `status.componentResults` now carries a bounded per-component summary and the report prints it under the category, PASSED or not: which node, which GPU UUID, what it reached, what it fell to, and how many samples were low.

It is derived from the rows alone. A producer marks a sample below its floor with `outcome: below`, so the controller needs no knowledge of the floor, a producer that enforces none yields nothing, and the derivation costs one pass over rows the Job tier is already holding. No shard is read, which is what lets it reach `--results-file` and not only `--results-bundle`.

The count is exact and the list is capped at 16 worst-first, so a cap can hide which components are named but never how many were found. The complete set stays in the bundle. An earlier unbounded per-group field measured 25.9 MB of Workflow status against a 1 MiB object limit (ADR-090's amendments), which is the mistake the cap exists not to repeat.

**What the entry is still for, since the Context's reason no longer applies.** Three things survive, and they are the honest justification:

- **Per-GPU result rows.** `dcgm-level4` already passes `--json`, and NVCRE discards the per-GPU values in it. This entry reports one row per GPU per sample through ADR-090, which is the capability the customer asked for and which no existing entry provides at any granularity.
- **The `compute` domain claim.** ADR-000 and ADR-001 claim raw compute as a capability. Filing this under `diagnostics` would, as the *Alternatives* section says, "keep ADR-000's `compute` claim false indefinitely".
- **An active stress workload NVCRE runs and judges**, on CUTLASS, rather than vendor tooling reporting its own verdict. That is the second of the two reasons the *Alternatives* section gives, and it is untouched.

The first reason that section gives, that peak-judging makes `dcgm-level4` "not the same test", no longer distinguishes them. What distinguishes them now is the reporting granularity and the harness, not the comparison.

### The floor is DCGM's own table and DCGM's own ratio

Decision 4 says the harness "checks each GPU's sustained result against a per-architecture floor" and leaves the floor's provenance open, describing it only as "a template parameter on the entry, defaulted per GPU architecture". Inventing those numbers was the obvious reading and it is the wrong one: a floor set too high fails healthy nodes, and nobody implementing this has the hardware to calibrate ten architectures.

DCGM already solved it. Its Targeted Stress plugin fails a GPU when achieved performance is below `target_perf_min_ratio * target_stress`, where `target_stress` is a per-SKU reference target from a built-in table and `target_perf_min_ratio` defaults to 0.95. Both knobs are taken over unchanged: `gemmTargetGFLOPS` in the entry's `meta.yaml` is that table keyed by this catalog's architecture names, `gemmMinRatioPercent` is the ratio as whole percent, and the harness computes the same product. The figures are not datasheet peaks; DCGM derived each from a measurement on real hardware discounted by 0.75, recorded in its source as comments of the form "dcgmproftester -t 1007 measures ~39000, multiply by .75 to get ~29250". Using a vendor's calibrated numbers also satisfies this record's own stated reason for naming CUTLASS rather than writing a benchmark: a result the customer disputes is checkable against a published tool.

There is no departure. The statistic, the ratio and the target are all DCGM's; see the amendment above for `maxVal` and for why `discountMultiplier` is 1.

### Precision is part of the calibration, and getting it wrong is silent

This is the amendment most worth reading, because the failure it describes looks exactly like success.

DCGM's targets are calibrated on the **FP32 pipe**: the field id its own provenance comments cite, 1007, is `DCGM_FI_PROF_PIPE_FP32_ACTIVE`, and every target in the table lands at 44% to 62% of its SKU's FP32 non-tensor peak. The harness therefore runs an FP32 GEMM, and the image builds only FP32 kernels.

The first implementation defaulted to FP16, which is the natural choice for a GEMM burn-in and would have been a silent disaster. An H100 sustains roughly 700 TFLOPS of FP16 tensor throughput against a 29.25 TFLOPS target, so the floor would have sat at about 4% of the achieved value: the gate would have passed every GPU, including a badly degraded one, while reporting itself as armed. A certification that cannot fail is worse than one that does not exist, because it produces a clean bill of health.

### The floor an operator sets is whole TFLOPS

`gemmFloorTFLOPS` on `CategoryOptions` is an `int32`, not a quantity or a float, and the reason is naming rather than precision. The value is tokenised into the generated Workflow name so two categories differing only in their floor do not collide (issue #430), that name becomes the Job name and then the TrainJob and pod names, and those must be DNS-1035 labels. A fractional value puts a dot in all of them. A floor is a coarse accept/reject line, so the precision a fraction would buy has no use, and `*resource.Quantity` would additionally forbid any CEL rule on the field under ADR-105.

Setting it replaces the target-times-ratio product outright rather than scaling it: someone who states a number in TFLOPS means that number.

### GB200 gets rows and no verdict, GB300 is armed

This amendment replaces an earlier one that said the whole Grace-Blackwell family goes without a verdict. That was written against a constraint the implementation then removed, and it is narrower than it looked: GB300 is armed from its own DCGM rows, and GB200 alone is the gap.

The trap is still worth naming. `ConfigArch` collapses `gb300` to `gb200`, so any `meta.yaml` lookup that goes through it sees one key for both architectures and a `gb300:` entry would be dead: never read, while looking armed. That collapse is right where it came from, parallelism, because the two share a GPU count and therefore a `tp/pp/ep` shape. It is wrong for a performance target, because GB300 is materially faster and DCGM gives them different numbers.

So the target lookup is the one consumer that keys on the **raw** architecture instead (`loader.go`, `getGemmTarget(config.GPUArchitecture)`). The collapse stays where it belongs rather than being widened to a place it would silently disarm a gate. `gb300` is therefore live, set from the lowest of DCGM's three GB300 rows (MaxQ at 46631) so a MaxQ node is not failed by a target taken from a Bianca one.

What remains is the number for GB200, and that is a sourcing problem rather than a keying one. DCGM's table has no row named GB200. Its multi-node diagnostic module maps device id `2941` to "GB200 NVL" in a comment, and `2941`'s row in the target table is named "B200 DevId 10DE" with a target of 15225, less than half the 33843 this entry uses for B200 on what should be comparable silicon. Identifying `2941` as GB200 takes a cross-reference between two source files rather than one explicit label, and the value disagrees with its own siblings.

So there is no figure for GB200 that is both sourced and credible. A GB200 fleet gets per-GPU rows without a performance verdict until someone measures one and sets `gemmFloorTFLOPS` from it. Arming the gate at half the right level reads as armed and catches nothing.

### Decision 5 is superseded by this entry shipping

Decision 5 corrects ADR-001's capability table to mark GEMM as "reachable through `WorkloadRun` and planned as a catalog entry", because the unqualified **Supported** was false. Implementing the entry makes it true, so downgrading the row would replace one wrong claim with another. The row instead names `compute/cutlass-gemm` and what it reports, which is the specific claim the table was missing.

### The harness owns three failure modes the record does not mention

Building it surfaced three ways a burn-in reports the wrong thing, each fixed:

A scratch directory that cannot be created made every path absolute, every sweep fail, and every GPU report `outcome: error`, so the run exited claiming no GPU sustained the floor. A tooling failure presented as a hardware failure on every GPU in the node is the worst answer available: it fails a healthy node and sends an operator hunting a fault that does not exist. The harness now exits 2 for "could not run the test", distinct from exit 1 for "the test ran and a GPU was below the floor".

A profiler that exits immediately, which is what a missing binary or an unsupported architecture looks like, made the sweep loop spin at full speed and emit a row per GPU per iteration. Rows feed ADR-090's bounded row budget, so a spinning harness does not merely fill the log, it truncates the result set it exists to produce. The sweep cadence is now held even when the profiler returns early, which bounds the row count at duration/sample per GPU by construction.

And the Mokka stub's own fault injection failed silently when its sweep counter was unwritable, so the degraded device never degraded and the lane reported a healthy node for a run asked to inject a fault. A fault injector that quietly stops injecting is worse than none, because the lane goes green for a case that never ran.

### The harness image tag is derived from the build version, not pinned

The entry has to name the harness image, and the first implementation wrote the version in: two literal `cutlass-gemm:vX.Y.Z` strings, with RELEASE.md told to bump them before tagging.

That is a step with nothing behind it. `release.yml` builds the binary and the harness image from the same tag, so the correct value is always the release being cut, and there is no reason a human should have to restate it. Getting it wrong is also uniquely quiet: the tag is only wrong once the release exists, nothing in the repository fails, and the first symptom is `ImagePullBackOff` on a cluster that has already installed the release. The repository had the same exposure on the two admission-policy sample pins, where nothing read a pinned version back either.

So the tag is derived. `catalog.SetBuildVersion` records the running binary's version once from each `main`, `HarnessImageTag` reduces it to a tag, and the entry renders `{{ .HarnessImageTag }}`. A release build renders its own version, which is exactly the image that shipped beside it.

Package state rather than a `BuildConfig` field, which is the one deliberate compromise. A field would be explicit, but it would have to be set at every construction site and would fall back to `dev` wherever someone forgot, reintroducing the silent-wrong-tag failure in a new place; it would also have to thread through `renderCertification` and its eighteen test callers. The build version describes the process rather than the render, so it is read-only after `main` sets it.

A build that is not a release has no release to point at, and renders `dev`. That includes a build past a tag: `git describe` output such as `v0.11.0-15-gabc1234` does NOT resolve to `v0.11.0`. Resolving it that way would hand a build of this tree the harness image from a release built from a different one, and once that release shipped every main build would pull its harness against main's entry with nothing reporting the mismatch. `publish.yml` offers no alternative to point at either: it tags the harness `main-<sha>` and the build is gated on the harness sources having changed, so that tag exists for only some commits. `dev` names no published image on purpose, and the lanes that run this entry without a release already set `spec.image`.

Recovering the tag means removing exactly what `git describe` appended, which is `-dirty` and `-<commits>-g<sha>`, and nothing else. The first implementation cut at the first `-` instead, and that is wrong for every prerelease: this repository has released `v0.2.0-rc.2` and its siblings, `release.yml` accepts the shape, and on such a release the harness image carries the prerelease in its tag too. Cutting at the `-` turned `v0.2.0-rc.3` into `v0.2.0`, a different release and quite possibly one that never existed, which is the same `ImagePullBackOff` the derive exists to prevent. `verify-pins` had the same blind spot from the other direction: matching a bare `vMAJOR.MINOR.PATCH` anywhere in the policy samples found three values, because both samples also name a tested cosign version and a tested policy-controller version in their comments, so it would have failed a correctly prepared release. It now compares the pinned `attest.yml@refs/tags/<TAG>` identity alone.

`verify-pins` in `release.yml` closes the loop from the other side: it fails the release if a sample pin disagrees with the tag, and if a catalog entry ever carries a literal version again.


## References

- [ADR-090: Sharded Component Results and the Report Bundle](090-component-result-sharding.md)
- [ADR-000: NVCRE Architecture for GPU Cluster Certification](000-adr.md), ClusterMAX alignment table
- [ADR-010: Certification Catalog with init() Registration](010-certification-catalog.md)
- [ADR-016: NCCL All-Reduce Certification Catalog Entry](016-nccl-all-reduce-catalog.md)
- [ADR-057: DCGM Level-3 Diagnostics, A100 Configuration](057-dcgm-level3-a100.md)
- [ADR-021: Performance Threshold Enforcement](021-performance-threshold-enforcement.md)
