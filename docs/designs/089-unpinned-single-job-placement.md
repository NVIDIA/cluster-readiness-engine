# ADR-089: Unpinned Single-Job Placement

**Status:** Accepted

**Date:** 2026-10-08

## Context

NVCRE has no input for "how many jobs to run". The number of jobs is derived from a chain
that was designed for a different purpose, and the result surprises users who ask for a
single job of a given size.

An 8-GPU Nemotron request against an 18-node cluster produces nine concurrent 2-node jobs:

1. `discoverTargetNodes` (`pkg/controller/workflow_controller.go:657`) returns every node
   matching the target. The default selector written by `nvcrectl` is
   `nvidia.com/gpu.present: "true"` (`pkg/certification/certification.go:1014-1019`), so by
   default that is the whole GPU fleet.
2. `resolveNodesPerJob` (`pkg/controller/certification_controller.go:940`) resolves a
   per-job node count from the entry's `minGPUs` and TP/PP constraints. 8 GPUs on 4-GPU
   nodes gives 2.
3. The catalog template writes it into the trainer: `numNodes: {{ .NodesPerJob }}`
   (`pkg/catalog/entries/training/nemotron5-8b.yaml:19`).
4. The Workflow controller reads it back with `adapter.NodesRequired`
   (`workflow_controller.go:538`) and passes it to `orchestration.PartitionNodes`
   (`:585`) as a **chunk size**.
5. `PartitionNodes` guarantees every target node lands in some group
   (`pkg/orchestration/partition.go:39-42`). 18 / 2 = 9 groups.
6. All nine launch at once, because `Execution.MaxConcurrent` defaults to 0, meaning
   unlimited (`workflow_controller.go:1020-1033`).

This is correct behavior for the model NVCRE was built around: a certification sweep that
must exercise every node in the fleet. It is wrong for the equally common case of running
one workload at a chosen scale.

No existing field expresses the second case:

- `maxConcurrent: 1` serializes the nine jobs rather than reducing them to one, and the
  Nemotron templates do not emit the field at all.
- `testScale: full-scale` is documented as "all nodes in a single group"
  (`api/v1alpha1/workloadrun_types.go:152`) but is a no-op in Go. `testScale` lives on
  `CategoryOptions` and is consumed only by `TemplateData`
  (`pkg/catalog/loader.go:481-490`); it never reaches `OrchestrationSpec`. The switch at
  `workloadrun_controller.go:785-796` has no `full-scale` arm.

Separately, every job is pinned to its group by a required `kubernetes.io/hostname` node
affinity (`workflow_controller.go:1166-1183`). On a shared or gang-scheduled cluster,
operators do not want NVCRE choosing hosts.

### The second defect, and why it is the same bug

`spec.orchestration.target.nodeSelector` is a discovery-only filter. It never reaches the
pod spec.

It is load-bearing where it is used. `discoverTargetNodes` (`workflow_controller.go:661-662`)
passes it as `client.MatchingLabels` to pick which nodes NVCRE considers, and
`GPUArchFromNodeSelector` (`pkg/catalog/catalog.go:208`) reads `nvidia.com/gpu.product` out
of it to select catalog overrides. But nothing propagates it onto the pods. Three
independent confirmations:

1. Rendering a cert carrying a distinctive `my-custom-pool: burnin` label puts it at
   exactly one place in the output, `spec.orchestration.target.nodeSelector` on the
   Workflow. No pod-level `nodeSelector` key appears anywhere.
2. The committed UAT goldens are real rendered pod specs.
   `test/uat/testdata/aws/h100/nccl/expected_pods.yaml:40-49` shows the only placement
   constraint is `nodeAffinity` on `kubernetes.io/hostname` with two literal hostnames.
   `grep '^\s*nodeSelector:'` across all seven `expected_pods.yaml` returns nothing.
3. No writer exists. `adapter.SetNodeSelector` (`pkg/workload/trainjob.go:79-91`,
   declared `pkg/workload/adapter.go:56`) is implemented but has zero production callers.
   No catalog entry, no `pkg/platform/` builder, and no WorkloadRun path emits a pod
   `nodeSelector`.

Today the hostname pin masks this: pods are pinned to named hosts that the discovery filter
already chose, so the target contract holds transitively. It is a correctness gap only in
the narrow window where a node's labels change after discovery but before scheduling, since
the affinity is `IgnoredDuringExecution` and the hostname list is already fixed.

But the two defects are the same bug. `target` is applied at the wrong layer: once, during
discovery, instead of being carried onto the pods. The pinning *is* the current substitute
for propagating the target. Removing pinning without fixing propagation would turn a latent
gap into an immediate one, with pods constrained by GPU resource requests alone and free to
land outside the target set entirely.

## Decision

Add `placement` to `OrchestrationSpec`, which is the Workflow tier:

```yaml
# Workflow
spec:
  orchestration:
    placement: Unpinned   # enum Pinned|Unpinned; empty means Pinned
```

The user-facing front door is `WorkloadRun`, which carries an `orchestration` block of its
own and lowers it through `buildWROrchestration`
(`pkg/controller/workloadrun_controller.go:780`):

```yaml
# WorkloadRun
spec:
  numNodes: 8
  orchestration:
    placement: Unpinned
```

**Certification deliberately has no placement field at all.** See "Why Certification is
excluded" below; it is the single most consequential decision in this ADR and the reason
`CategoryOptions` is untouched.

`OrchestrationSpec` is still where the field lives, because it is the only orchestration
input the Workflow controller reads and the tier that actually partitions. A hand-written
Workflow can therefore set it, and `pkg/render` previews it, but nothing in the catalog
path ever sets it.

### Why Certification is excluded

A Certification's output is a verdict about a fleet. `PartitionNodes` guarantees that every
node the target matched lands in some group (`pkg/orchestration/partition.go:39-42`), and
the only things that reduce coverage are the cordon, architecture and capacity filters,
every one of which forces the report to INCOMPLETE (`pkg/report/report.go:379-381`). That
invariant is what lets a PASSED certification mean "every node you pointed me at was
tested."

Unpinned on the Certification tier would be the first and only mechanism by which a
Certification could report PASSED while deliberately leaving targeted nodes untested. Not a
filter the operator can fix, not a configuration accident worth warning about: scope they
asked for, invisible in the verdict. Fixing that by routing intentional scope into
`ExcludedNodes` is worse, since it buries the real exclusions and downgrades a correct run.
Fixing it by keying the INCOMPLETE rule on placement means the certification verdict now
has two meanings depending on a flag.

So the certification verdict keeps its one meaning and the mode lives where the question is
genuinely different. A WorkloadRun makes no coverage claim: it runs a workload and reports
whether the workload passed. `buildWorkloadRunReport` (`pkg/workloadrun/workloadrun.go:1546`)
never populates `ExcludedNodes`, so the INCOMPLETE downgrade is structurally unreachable on
that path. Nothing has to be special-cased for it to be correct.

The cost is real and worth naming: WorkloadRun has no catalog access, so the TP/PP sizing
and NCCL tuning in `pkg/catalog/entries/` is reachable only through Certification. An
operator who wants a single unpinned Nemotron job at a tuned configuration cannot get the
catalog's tuning today. That is a separate feature, and it is a better one than weakening
the certification contract to reach it.

Under `Unpinned`:

1. Partitioning is skipped entirely. One group is emitted with no node list.
2. No `kubernetes.io/hostname` affinity is written.
3. The requested size is honored exactly, or the run fails. It is never silently adjusted.
4. `diagnose` and `topology.strictDomain` are rejected. `topology.topologyKey` is ignored.
5. The blanket `Operator: Exists` toleration narrows. An explicit `target.taintSelectors`
   still wins; the MPI fallback becomes the named GPU taints in
   `UnpinnedMPITolerations` rather than every taint on the cluster.

And independently of placement, in **both** modes:

6. `TargetSpec` is translated into a pod-level `nodeAffinity` on every job.

Default behavior is unchanged except for item 6, which strictly tightens Pinned mode
without changing which nodes are admissible.

### The contract

This is the guarantee the tests pin, stated for users:

> With `placement: Unpinned` and an explicit size, NVCRE creates exactly one job of exactly
> that many nodes, no matter how many nodes the target matches. The size the user wrote is
> the size that runs, or the run fails with a message saying why. It is never silently
> adjusted.

```yaml
# WorkloadRun
spec:
  numNodes: 8
  orchestration:
    placement: Unpinned
  target:
    nodeSelector: {nvidia.com/gpu.present: "true"}   # 18 nodes match
```

One job, 8 nodes, 10 nodes untouched. Today the same spec gives three jobs of 8: two full
groups and an overflow group that takes the remaining 2 nodes plus 6 borrowed from the
second, because `PartitionNodes` covers every node (`pkg/orchestration/partition.go:90-112`).

`numNodes` is already `Required` with `Minimum=1` (`workloadrun_types.go:207-210`), so the
"no size given" hole that the Certification path would have had cannot open here. What does
need closing is the CLI, where two flags rewrite `numNodes` after the file is read; see
"Honoring the size" below.

### Why `numNodes` does not, and should not, narrow `target`

A natural follow-up: if `target` defaults to every `nvidia.com/gpu.present=true` node but
`numNodes: 2` is specified, should `numNodes` override the default selector?

The two answer different questions, and keeping them separate is what makes this design
work:

- `target` answers **which nodes are eligible**. It is a *set* predicate: a label selector
  the API server and the scheduler can both evaluate.
- `numNodes` answers **how many of them to use**. It is a *count*.

A count cannot narrow a set, because it does not say *which* members to drop. Any attempt
to make it do so has to invent a choice the user never expressed, and that invented choice
is exactly the bug above: `nodesPerJob`/`numNodes` is currently consumed as a chunk size,
and the "narrowing" it performs is to slice the fleet into `ceil(M/N)` pieces and run all
of them. Giving the count more authority over the set would deepen the confusion rather
than resolve it.

What users want is for the count to be honored **as a count**: 2 means one job of 2 nodes,
not nine jobs of 2. That is the contract above, delivered by `placement: Unpinned` rather
than by changing what `target` means.

The resulting division of labor:

- `target` (possibly the permissive default) defines the eligible set and is carried onto
  the pods as a real node affinity. On a homogeneous fleet the default is the right answer:
  any GPU node will do.
- `numNodes` is the job size, honored exactly, never clamped or snapped or auto-expanded
  under Unpinned.
- The **scheduler** picks which 2 of the M eligible nodes to use. That is its job, it has
  live capacity information NVCRE does not, and not pinning nodes is the explicit request.

A user who wants a narrower set should tighten `target` itself: a more specific
`nodeSelector`, `matchExpressions`, or an explicit `nodeNames` list. Those are set
operations and they compose correctly with any `numNodes`.

`gpusPerNode` sits in a third category and is worth naming because it is often mentioned
alongside `numNodes`. It is a per-node resource request (`workloadrun_types.go:203-207`,
flowing to `NumProcPerNode` at `workloadrun_controller.go:674`, the MPI `-N` arg at `:639`,
and the container resources via `platform.RuntimeConfig.GpusPerNode` at `:507`), not a
selector. It does influence eligibility, but indirectly and through the right mechanism:
`filterNodesByGPUCapacity` (`workflow_controller.go:483`) drops nodes that cannot supply the
request, and the scheduler enforces the same thing again at bind time via the pod's resource
request. So `gpusPerNode` already narrows the effective set without teaching a count to
behave like a selector.

## Implementation

### API additions

Six fields across two files. All optional, so existing specs are unaffected.
`WorkloadRunSpec` is spec-immutable, which these inherit correctly. Only the last two are
status fields; the rest are spec.

| # | Location | Field |
|---|---|---|
| 1 | `api/v1alpha1/workflow_types.go` | constants `PlacementPinned = "Pinned"`, `PlacementUnpinned = "Unpinned"` |
| 2 | `OrchestrationSpec` | `Placement string`, enum `Pinned;Unpinned` |
| 3 | `OrchestrationOverrideSpec` | `Placement *string`, enum, no default |
| 4 | `WorkloadOrchestration` (`workloadrun_types.go`) | `Placement string` |
| 5 | `OrchestrationStatus` | `Placement string` |
| 6 | `OrchestrationStatus` | `GPUProducts []string` |

`certification_types.go` gains no field. `CategoryOptions.NodesPerJob`'s doc comment is
updated to say so, since the CRD description is generated from it and a reader looking for
the mode should be sent to WorkloadRun rather than left to conclude it was forgotten.

**No `+kubebuilder:default=Pinned`.** A string default materializes into every existing
object's serialized spec on the next write, moving golden files that otherwise would not
move. The existing defaults in `workflow_types.go` (`:108`, `:182`, `:199`, `:212`) are
numeric or boolean zero values that `omitempty` suppresses, so they are invisible. A
non-empty string default would not be. Empty means Pinned, enforced in code.

`GPUProducts` holds the raw distinct `nvidia.com/gpu.product` label values of the surviving
nodes. It exists because `gpu.ParseProduct` is lossy (`NVIDIA-H100-80GB-HBM3` becomes
`h100`, `pkg/gpu/product.go:35-50`), so `DetectedGPUArchitecture` cannot be reversed into a
label match. See "The GPU architecture term" below.

### Override merge

`mergeOrchestration` (`pkg/controller/workflow_detect.go:854-867`) gains a `Placement`
pointer branch after `Topology`. A nil pointer is a no-op, so no existing override behavior
moves. This is mandatory rather than optional: any new `OrchestrationSpec` field without a
pointer twin and a merge arm is silently unoverridable.

Ordering matters. `applyOverridesWithTracking` runs at `workflow_controller.go:552`, well
before the partition decision at `:568`. An override can therefore introduce orchestration
fields the user never wrote, and any placement check must assume the post-override spec.
The GB200 topology-key override is exactly that case, and is the reason for the asymmetry
in the next section.

### Reject diagnose and strictDomain; ignore topologyKey

**Diagnose: reject.** A terminal check in `discoverAndPartition`, after
`orch.DetectedGPUArchitecture = gpuArch` (`workflow_controller.go:449`). Diagnose reads
`g.Nodes[0]` (`:1818-1824`) and accumulates `g.Nodes` into suspects (`:1882`, `:2073`); with
an empty list it would silently report every node healthy. It is definitionally about
partitioning.

Both rejections are mirrored in `nvcrectl` so offline `render` rejects what a reconcile
would reject. The mirror sits in `render.ResolveWorkflowForPlatform`, immediately after
`ApplyOverridesWithTracking`, which is the same post-override boundary the controller uses
and is reached by every render path: `certification render` with and without `--dry-run`,
and `workflow render`. `workloadrun render` has its own copy at each of its two override
sites, because it does not route through that helper.

Placing the mirror after overrides is not a stylistic choice. A conflicting field rarely
appears in the YAML the operator wrote: the catalog lowers `testScale: intra-rack` into
`topology.strictDomain` and `testScale: diagnose` into a `diagnose` block at `entry.Build()`
time (`pkg/catalog/entries/communication/nccl-all-reduce.yaml:167-175`), and an override can
merge `topology` in later still. A check on the spec as written sees neither.

`ValidateWRPlacement` keeps its separate, earlier call inside `BuildWorkflowSpec` for the
reason given there: it reads `testScale`, which has been lowered away by the time an
`OrchestrationSpec` exists. The two checks are complementary rather than redundant, and both
run on the WorkloadRun render path.

**`strictDomain: true`: reject.** It is set only by an explicit `testScale: intra-rack` and
means "one group per topology domain", which directly contradicts one job.

**`topologyKey`: ignore, do not reject.** The GB200/GB300 override at
`pkg/catalog/entries/training/nemotron5-8b.yaml:288-296` injects
`topology.topologyKey: nvidia.com/gpu.clique`:

```yaml
- when:
    gpuArchitecture:
      in: [gb200, gb300]
  dependencies:
    # ... ComputeDomain + DRA ...
  orchestration:
    topology:
      topologyKey: nvidia.com/gpu.clique
```

`nemotron5-56b.yaml:258-268` is identical, and `dcgm-level4.yaml:84` pulls the same through
`overrides/gb200-topology-key.yaml`. Overrides apply before the partition decision, so by
the time a placement check runs the topology key is already on the spec. Rejecting it would
mean a Nemotron run on GB200, the exact case that motivated this work, fails with a message
about a field the user never wrote and cannot see in their YAML.

The rule, stated once so it does not read as an inconsistency:

> Reject what the user asked for that contradicts Unpinned. Ignore what the catalog added
> that Unpinned simply does not use.

`TopologySpec` drives only partitioning: `TopologyKey` feeds the greedy domain packer and
`StrictDomain` forces one group per domain. Unpinned does not partition, so there is nothing
for either field to do. Ignoring is also the safe direction, since the override exists to
pair the topology key with the GB200 ComputeDomain dependency, and that dependency is
unaffected.

Two things downstream read the topology key outside partitioning, and both degrade cleanly:

- **Topology metrics** (`workflow_controller.go:2376-2414`). The gauges are labelled by
  domain, resolved per node via `getNodeDomain` (`:2428-2437`). That helper returns
  `groupDomains[0]` when the group has exactly one domain, else looks the node's label up
  directly. An Unpinned group has no `Domains`, so it falls to the live lookup and gets the
  right answer. Before the node backfill lands, the loop iterates zero nodes and records
  nothing, which is accurate rather than wrong.
- **`collectAllDomainNodes`** (`:2478-2489`), used only by `cleanupTopologyMetrics` at
  `:2947`. Groups with no `Domains` contribute nothing to either map, so recording and
  cleanup stay symmetric and no gauge leaks.

An informational log line records that a topology key was present and not used for
placement, so the behavior is visible rather than silent.

For a user who wants an unpinned job confined to one topology domain, the answer is
`target.matchExpressions` naming the domain label, which the target terms below carry onto
the pods.

#### What ignoring the key actually costs, on a multi-clique fleet

Ignoring `topologyKey` is not free, and the cost should be stated rather than left for a
reader to find. On GB200 the key is `nvidia.com/gpu.clique` and the greedy packer's purpose
is to minimize the number of NVLink domains a job spans. Pinned gets that packing. Unpinned
does not: one group, no domains recorded, and no clique constraint of any kind on the
created pods. On a fleet split across cliques the scheduler may put a 2-node job's pods in
different cliques, and a workload running `NCCL_MNNVL_ENABLE=1` then falls back to the
slower inter-clique path rather than multi-node NVLink.

`cmd/integration/testdata/reconcile/workflow-unpinned-multi-clique/` pins this. Four nodes,
two cliques of two, the same key, a 2-node job: the Workflow records one group with no
domains, and the Job's node affinity carries the target terms and nothing else. The
motivating GB200 fixture cannot show it, because every node there is in `clique-0`.

A required same-clique pod affinity is the obvious counter-proposal and is rejected here,
for two reasons rather than one. Applied unconditionally it makes any job larger than a
clique unschedulable, which turns a performance question into an availability one. And
without gang scheduling it strands partially placed pods: a required pod affinity resolves
against pods that are already bound, so the first pod picks the clique and the rest wait
indefinitely if that clique lacks room, with nothing naming the cause. Neither failure mode
is one a user opting into "let the scheduler place it" would expect.

The mechanism that fits is the same one the rest of this ADR uses for placement intent:
`target.matchExpressions` on the clique label, which is a set predicate the scheduler
re-evaluates at bind time and which the target terms carry onto every pod including the MPI
launcher. It confines the job to one clique by naming it, costs nothing when unset, and
composes with any `numNodes`. If a future change does want automatic clique confinement, a
*preferred* pod affinity is the shape to reach for, since it expresses the preference
without making the larger job unschedulable.

These are Go checks rather than CEL `XValidation` rules. CEL is a reasonable additional
admission-time guard but cannot be the only one: catalog-built Workflow specs are assembled
in-process, and an integration test that asserts the rejection must be able to create the
object first. CEL is in any case unable to express the topology rule, since the conflict
arises from an override applied after admission.

### Partition short-circuit

`workflow_controller.go:449-468` and the group construction in `discoverAndPartition` (`:474`):

- The `nodesPerJob < 1` fallback to `len(nodes)` at `:449-461` must not apply under
  Unpinned. That fallback *is* the span-the-cluster behavior. It becomes a terminal spec
  error naming `nodesPerJob` / `numNodes`.
- The `nodesPerJob > len(nodes)` terminal error at `:464-468` stays. Asking for 16 nodes on
  a 12-node fleet cannot schedule, and failing here beats pods pending forever. Under
  Unpinned it is the enforcement point for the size guarantee: `numNodes` is never clamped
  down to fit the fleet, it is reported as an error naming both numbers.
- A third arm is added **before** the diagnose/partition split at `:568-591`, emitting one
  `Group{Name: "group-0"}` with a nil node list.

`PartitionNodes` is not called and `pkg/orchestration/` needs no changes at all. Its
coverage invariant (`pkg/orchestration/partition.go:39-42`) is precisely what Unpinned
violates, so the short-circuit sits one level up rather than adding a flag to
`PartitionInput`.

Status: `TotalNodes` and `NodesPerJob` are unchanged, `TotalGroups` falls out as 1, and
`orch.Placement` records the resolved mode. `buildGroupStatuses` (`:2442-2473`) already
tolerates an empty `g.Nodes` and a nil topology. `getGroupJobName` (`:2686-2695`) already
collapses to `<workflow>-job` when `TotalGroups <= 1`, so job naming needs no change.

### Target propagation, in both modes

A shared helper translates `TargetSpec` into node selector requirements:

```go
// TargetNodeAffinityTerms converts a TargetSpec into node selector
// requirements for the pod spec. Discovery-side filters the scheduler
// already enforces (cordoned, GPU capacity) are deliberately omitted.
func TargetNodeAffinityTerms(t *nvcrev1alpha1.TargetSpec, gpuProducts []string) []corev1.NodeSelectorRequirement
```

- `target.nodeSelector` entries become `key In [value]`, emitted in sorted key order so the
  output is deterministic and goldens are stable.
- `target.matchExpressions` passes through verbatim; the field is already
  `[]corev1.NodeSelectorRequirement`.
- `gpuProducts` becomes `nvidia.com/gpu.product In [values]`.
- `target.nodeNames` is deliberately excluded. It is an explicit host list and is already
  expressed by the hostname term in Pinned mode; under Unpinned it is carried separately.
- `target.taintSelectors` is deliberately excluded. It selects nodes *by taint* and is
  answered by tolerations, not affinity.

All requirements go in a **single** `NodeSelectorTerm`, because terms OR together while
expressions within a term AND. This matches the AND semantics already documented at
`workflow_types.go:121`.

`SetNodeAffinity` is used rather than `SetNodeSelector`. `SetNodeAffinity` applies to all
replicatedJobs including the MPI launcher (`trainjob.go:99-107`); `SetNodeSelector` writes
only the worker (`trainjob.go:84-85`), which would let launcher pods escape the target.
`SetNodeSelector` remains part of the adapter interface but stays unused; see Notes.

The scheduler natively covers two of the three discovery-side exclusion filters: cordoned
nodes are unschedulable, and under-capacity nodes fail the GPU resource request. Only the
GPU architecture filter needs explicit carry-over.

**The GPU architecture term.** `detectGPUArchConsistent` (`workflow_detect.go:302-318`)
filters the discovered set to one architecture via `gpu.ParseProduct`, which is lossy. So
`orch.DetectedGPUArchitecture` cannot be reversed into a label match. `GPUProducts` records
the raw values, populated in `discoverAndPartition` where `detectGPUArchConsistent` already
runs and the surviving node objects are in hand. `createJobForGroup` reads it from status.
Persisting is preferred to re-deriving: `createJobForGroup` runs on a later reconcile, so
re-deriving means a second discovery pass whose result could disagree with the partition the
groups were built from. When the list is empty (no `nvidia.com/gpu.product` labels anywhere,
as in much existing testdata), the term is omitted rather than emitting an empty `In []`,
which matches nothing and would deadlock scheduling.

**Branching on placement** at `workflow_controller.go:1373`:

- Pinned: hostname term `In group.Nodes`, plus the target terms, in one `NodeSelectorTerm`.
  `SetNumNodes(spec, len(group.Nodes))` is kept exactly as today.
- Unpinned: the target terms, plus `kubernetes.io/hostname In target.NodeNames` when
  `nodeNames` is set, and `SetNumNodes` is **skipped entirely**.

**Why Pinned mode cannot regress.** The hostname term is `In [n1, n2]` where those names
came from the discovery filter, so every one of them already satisfies every target term by
construction. Adding the target terms to the same `NodeSelectorTerm` ANDs a predicate that
is already true for every value in the hostname list. The admissible node set is unchanged.
What changes is that the constraint is now re-evaluated by the scheduler at bind time
instead of being trusted from discovery time, which is the latent gap closing.

The same branch is mirrored in `pkg/render/render.go:393-420` through the shared helper, so
the preview and the controller cannot drift. `render.go` has no `OrchestrationStatus` to
read `GPUProducts` from; it already discovers nodes at `:306`, so it derives the distinct
products from that node set inline.

### ComputeDomain sizing, and why SetNumNodes is skipped rather than zeroed

The GB200/GB300 override that carries the topology key also carries a ComputeDomain whose
size is templated from the same number:

```yaml
# pkg/catalog/entries/_lib/deps/gb200-compute-domain-and-dra-torch.yaml:15
    numNodes: {{ .NodesPerJob }}
```

A WorkloadRun on GB200 or GB300 pulls that same fragment through
`pkg/platform/overrides/workloadrun.yaml:9-13`, rendered with
`platform.OverrideConfig.NodesPerJob` (`workloadrun_controller.go:554`). The TrainingRuntime
the same path builds takes its `mlPolicy.numNodes` from the identical field
(`pkg/platform/runtime.go:245`). Both read `NodesPerJobForScale(spec.Orchestration,
spec.NumNodes)` (`:504`, `:404-409`), which returns `spec.NumNodes` unchanged for every
`testScale` except `intra-node`, and `intra-node` is rejected under Unpinned
(`pkg/controller/placement.go:66-70`). So the ComputeDomain and the trainer are sized from
one value, and that value is the size the user wrote.

This is why skipping `SetNumNodes` is not interchangeable with clamping or zeroing it.
`SetNumNodes` rewrites the trainer's `numNodes`, but nothing rewrites the ComputeDomain's.
With `group.Nodes == nil`, `len(group.Nodes)` is 0, so `SetNumNodes(spec, 0)` would write
`numNodes: 0` (`pkg/workload/trainjob.go:134-140`) and the job would never run. Any other
adjusted value would desynchronize the two: a ComputeDomain sized for N channels against a
TrainJob asking for a different count, which on GB200 means pods that never get their DRA
allocation and hang Pending with no error naming the cause. Skipping keeps a single source
of truth, `spec.numNodes`, for both.

It is also why rejecting rather than adjusting the size is load-bearing beyond user-facing
honesty. Under Pinned, a clamp flows into both the trainer and the ComputeDomain together,
because both are templated from the same resolved number, so they stay consistent even when
adjusted. Under Unpinned the clamps are rejections, which is what guarantees the two stay
equal to the number the user wrote.

### Honoring the size: three doors

`numNodes` is `Required` with `Minimum=1`, so a WorkloadRun always arrives carrying a size.
Three mechanisms can still change it before a job is created, all silent, and all must
become errors under Unpinned.

Two are CLI flags on `nvcrectl workloadrun run`. Both are a sharp version of the same
problem, because the operator never sees the object that was submitted and so never learns
the job got smaller:

1. **The `--node-list` clamp.** `applyRunOverrides` lowers `numNodes` to the length of the
   list when the list is shorter. Correct under Pinned, where naming fewer nodes than a
   chunk size asks for can never be satisfied, so shrinking beats waiting.
2. **The `--topology-domain` replacement.** This one does not clamp, it overwrites:
   `numNodes` becomes the number of nodes the domain turned out to contain, whatever the
   file asked for. Like the clamp it has a second, discovery-time half that fires on the
   real count.

The third is the Workflow tier, which is a backstop rather than a user-facing door:

3. **The `nodesPerJob < 1` fallback** at `workflow_controller.go:449-461` substitutes
   `len(nodes)`, which *is* the span-the-cluster behavior. A WorkloadRun cannot reach it,
   since `numNodes` is required, so this guards hand-written Workflows.

| Door | Today | Under Unpinned |
|---|---|---|
| `workloadrun.go:1630` (`applyRunOverrides`) | `--node-list` clamps, `--topology-domain` overwrites | reject both flags, naming the alternative |
| `workloadrun.go:988` (post-discovery) | same two flags, on the live node count | skip the adjustment, do not fail late |
| `workflow_controller.go:449-461` | falls back to `len(nodes)` | terminal spec error (backstop only) |

Under Pinned all three are unchanged: both CLI adjustments and the fallback remain exactly
right for a chunk size.

`resolveNodesPerJob` (`certification_controller.go:940`) is **not** on this list and takes no
placement argument. Its clamp, its `MaxValidNodes` snap and its auto-select are all correct
for what they are: a group size for a sweep that covers the whole target either way. They
change how the sweep is cut into jobs, not which nodes it reaches. The reason they would
have needed rejecting arms in an earlier draft is that the draft put Unpinned on the
Certification tier; with the mode confined to WorkloadRun, the Certification path keeps
its behavior. Two edits to it remain from the branch and neither changes a result: the
`resolveNodesPerJob` doc comment now says why the adjustments are safe on that tier and
why Unpinned lives elsewhere, and `ResolveOptions` spells its string options with `cmp.Or`
to stay under the cyclomatic limit.

The CLI door is two halves, and they are closed differently. `applyRunOverrides`
runs on the spec read from the file and **rejects**: `--node-list` with fewer names than
`numNodes`, and `--topology-domain` at all. The second half runs after node discovery
(`workloadrun.go:988`), where the flags' effect depends on numbers only known then, and it
**skips** rather than rejects. The asymmetry is deliberate: reaching the discovery clamp
under Unpinned means the flags were already accepted, so the file's `numNodes` was within
the list, and the right answer there is to leave it alone rather than fail late on a spec
that was fine. Rejecting in both places would turn a legal `--node-list` of three names for
a 2-node job into an error the moment discovery returned two live nodes.

`--node-list` itself stays usable under Unpinned. Only the shortfall is refused; a list
longer than `numNodes` is a narrower target, which is exactly what Unpinned asks the
operator to use instead of a count. `--topology-domain` has no such reading, because it
replaces the count unconditionally, so the error names the substitute:
`target.matchExpressions` on the topology label, which confines placement to the domain and
leaves `numNodes` alone.

**Error classification.** The CLI rejections surface as a flag error before any object is
created, which is the whole point of putting them there: a server-side rejection would mean
the operator sees a Failed WorkloadRun rather than a usage message. The Workflow-tier
backstop is a terminal spec error, deliberately not a requeue: a missing size does not fix
itself on the next reconcile, and requeuing would spin silently with no Failed condition.

### Tolerations

ADR-023 justifies the blanket `Operator: Exists` toleration on the grounds that "workloads
are already pinned to specific nodes via NodeAffinity"
(`docs/designs/023-catalog-configurability.md:113`). Under Unpinned they are not, so blanket
Exists plus no host pin would let an MPI job land on any tainted node with free GPUs.

So under Unpinned the blanket narrows rather than disappearing. An explicit
`target.taintSelectors` still wins outright, which is branch 1 of the existing precedence.
What changes is the fallback branch: instead of `Operator: Exists`, an MPI workload gets the
named pair in `UnpinnedMPITolerations` (`pkg/controller/placement.go`), the two taints the
catalog's own runtime patches already list for the platforms that declare any.

Dropping the fallback entirely was the first shape of this amendment and it is wrong in the
other direction. GPU fleets are routinely tainted to keep non-GPU work off them, so a
launcher that tolerates nothing simply never schedules, and it fails silently: the pods sit
Pending with no event naming a toleration. That is the harder of the two failures to
diagnose. The named pair is strictly narrower than `Exists`, so it cannot admit a node the
current behavior would have refused, while still covering the taints a real GPU fleet
carries. A fleet that taints some other way names it in `target.taintSelectors`, which takes
precedence.

This is a scoped amendment to ADR-023, linked from its Notes; the original decision stands
for Pinned mode, where its premise still holds.

### Node attribution from actual placement

With no pin, `GroupStatus.Nodes` is empty at job creation, so it is backfilled from where
pods actually landed. Two independent write-backs, each in the controller that owns the data.

**`GroupStatus.Nodes`** (Workflow controller). `backfillGroupNodes`, called from the
per-group loop in `updateStatusFromJobs`, gated on Unpinned and `len(g.Nodes) == 0`.
Setting `statusChanged = true` routes it through the existing `applyGroups` retry closure,
so there is no new write path. This requires adding a `NodeDiscoverer` to
`WorkflowReconciler` and wiring it in `SetupWithManager` the way `job_controller.go:1674`
does. The Workflow controller already has pod read RBAC (`workflow_controller.go:77`).

While the job is still running the write is gated on a complete placement,
`len(names) == nodesPerJob`, so a prefix of bound pods is not persisted as if it were the
whole group: the `len(g.Nodes) == 0` guard would stop re-firing and every later read would
treat the short list as complete. Waiting costs one reconcile.

That gate comes off once the job is terminal. The loop only visits groups that are still
`GroupRunning`, so a terminal reconcile is the last one that can record anything, and a job
that failed with half its pods bound is exactly the case attribution exists for. Holding out
for a complete list there would attribute the failure to no nodes at all. A partial list
beats an empty one, and nothing re-reads it expecting `NodesPerJob` entries.

Two other branches of the same loop reach `GroupFailed` without passing the status switch:
a Job confirmed deleted, and a Job carrying a `DeletionTimestamp`. Both used to go straight
to the failure without the backfill, so an Unpinned job deleted mid-run was attributed to
nothing at all. Running the backfill at the point of failure is not enough on its own,
because of where that point sits: the `DeletionTimestamp` branch fails the group only after
the pod-drain barrier passes, and the barrier passes once the job's Running and Pending pods
are gone, which is exactly when there is nothing left to read placement from. The Job
controller's finalizer waits on the same drain, so the confirmed-deletion branch arrives
later still.

So the `DeletionTimestamp` branch records placement first, with `terminal` set, and only then
waits on the drain. That is the one reconcile that still sees the pods, and the write goes
out even though the group stays Running behind the barrier. Both branches run the backfill
again as a backstop before failing the group, which is a no-op once a list is recorded.
`workflow-unpinned-deleted-job-attributed/` pins the order with the controller: a 2-node job
with one pod bound, the Job deleted while that pod is Running, and the record checked before
the pod is detached. A backfill placed after the drain never satisfies that wait.

This is why `backfillGroupNodes` takes a namespace and name rather than the Job. The
confirmed-deletion branch has only the `JobRef` left to go on, and neither branch can wait
for a live read.

**`groupNodeNames`** (Job controller). `createJobForGroup` sets the
`nvcre.nvidia.com/group-nodes` annotation only when `len(group.Nodes) > 0`, so under
Unpinned the key is absent rather than present-and-empty and `groupNodeNames`
(`job_controller.go:1444-1467`) stays on its existing nil path. It then falls back to live
discovery, so failed jobs get a populated `FailedNodes` list at `:935` and `:1069`.

**The phase filter must not be reused for attribution.** `DiscoverNodesForJob`
(`pkg/nodemonitor/nodes.go:57-59`) keeps only pods in `Running` or `Pending`. A job that
fails fast, or whose pods have already terminated when the reconcile lands, yields zero
discovered nodes, so attribution would be empty in exactly the case attribution exists for.
A sibling in `pkg/nodemonitor/` applies the same index lookup, label-selector fallback,
`Spec.NodeName != ""` check and dedupe, but no phase filter. A terminated pod still records
where it ran. The Workflow controller's success path keeps the phase-filtered helper, since
it only runs while the group is `GroupRunning`.

**Results are sorted.** `DiscoverNodesForJob` dedupes through a map and returns
map-iteration order, which Go randomizes per run. Writing that straight into
`GroupStatus.Nodes` would produce a status field that reshuffles between reconciles and
golden files that fail intermittently.

**Every reset to Pending clears the node list.** Two sites return a group to Pending and
neither cleared `Nodes`: the iteration reset in `handleIterationComplete` and the retry
reset in `completeTerminalGroup`. Both now call `resetUnpinnedGroupNodes`, which is a no-op
under Pinned, where the controller assigned the nodes and the job is recreated on the same
hosts.

Under Unpinned the next job is placed afresh by the scheduler, so the previous attempt's
nodes are a guess about it, and `backfillGroupNodes` only fires on an empty list: a stale
list is never corrected. The retry case is the worse of the two, because the list also
reaches the Job through the `nvcre.nvidia.com/group-nodes` annotation, which `groupNodeNames`
prefers over live pod discovery. A failure after a retry would be attributed to the nodes
that ran the attempt before it, and a success would report the nodes that just failed. The
terminal-backfill above makes this reachable on a first attempt too, since a partially placed
failure now records a list.

The reset writes an empty slice rather than nil: `nodes` is a required CRD field and nil
marshals to `null`, which the API server rejects.

**The retry fixture found a race in the retry path itself.** The end-to-end check for the
reset, `cmd/integration/testdata/reconcile/workflow-unpinned-retry-reattributes/`, drives a
Job through a retry with the second attempt's pods bound to different nodes. Its first run
failed with the Workflow `Failed/IterationsFailed` while the retried Job ran to completion
unobserved. The log showed why: `completeTerminalGroup` deletes the failed Job and resets
the group to Pending in one reconcile, but the Job's deletion is a watch event of its own,
and the reconcile it triggered read the Workflow from the informer cache before that status
write had reached it. It saw a Running group whose Job was being deleted, which is what an
external deletion looks like, and failed the group. Nothing about this is specific to
Unpinned, and no integration fixture had exercised `retryFailedGroups` before, which is how
it went unnoticed.

Three changes close it, all in `workflow_controller.go`.

The two branches in `updateStatusFromJobs` that fail a group because its Job is gone or
going now call `groupViewIsStale` first, which reads the Workflow through the uncached
reader and checks that the group is still Running on that Job there, in the same iteration
and at the same retry count, since a relaunched Job carries the name of the one it
replaced. A group whose live state has moved on is left alone for the pass and the
reconcile requeues immediately. This is the same shape as the existing confirmation of the
Job's absence through `jobReader` (issue #385): both branches are irreversible, so both
confirm before they act.

The tail write in `updateStatusFromJobs` carries over only the groups the pass changed,
matched by name, instead of replacing the whole slice. After a conflict refetch the slice
came from a cached view, and writing it back wholesale would put an untouched group where
the stale view had it, Running on a Job that no longer exists, which the next pass would
then fail. The per-group merge is also what lets a deferred group coexist with a sibling
whose retry reset in the same pass must still be written, because that sibling's Job is
already gone.

`createOrAdoptJob` no longer adopts this Workflow's own Job when it is terminating. The
retry relaunches under the same name while the Job controller still holds the old Job's
finalizer for the pod drain, so a relaunch that collided with it adopted a Job about to
vanish, and the live view then agreed that the group was Running on it. A terminating own
Job is now the same retry-with-backoff as a terminating foreign one.

`pkg/controller/testdata/stale-group-view/` pins the first two with a fake client standing
in for the API server, including the two agreeing-view controls where the group still
fails and a two-group case where one group's reset is written while its stale sibling is
not. `workflow-create-adopt-job/own-terminating-holder/` pins the third.
`workflow-unpinned-iteration-reattributes/` covers the iteration reset the same way as the
retry fixture. It cannot hit the race: handleIterationComplete deletes the Jobs of groups
whose terminal phase was already in the cache it read, so no later view shows them Running
and neither failing branch is reachable for them. The retry is the one path that deletes a
Running group's Job and changes its phase in the same reconcile.

### Coverage reporting

Untested nodes are not put into `ExcludedNodes`. `exclusionSummary`
(`workflow_controller.go:242-277`) is about nodes dropped by a filter, all of which are
configuration accidents; untested nodes in a single-job run are intentional scope.
Conflating them would make the cordon and architecture warnings unreadable and would turn a
correct run INCOMPLETE (`pkg/report/report.go:355-362`).

Instead `orch.Placement` surfaces on the report and the scope is printed near the existing
`Nodes/Job` line (`report.go:1315-1318`), for example
`Placement: Unpinned (2 of 18 target nodes exercised)`. The INCOMPLETE rule itself is
unchanged, so a cordoned node in an Unpinned run still reports INCOMPLETE.

Three existing lines in the same box read orchestration counts and are printed
unconditionally:

- `Nodes/Job: %d` from `cat.NodesPerJob = orch.NodesPerJob` (`:642`, printed `:1315`).
  Correct and now more meaningful: under Unpinned it is the job size the user asked for.
- `Jobs: %d` from `cat.Jobs = orch.TotalGroups` (`:643`, printed `:1319`). Prints `Jobs: 1`,
  which is the headline outcome.
- `Nodes: %d` from `report.TotalNodes = orch.TotalNodes` (`:347`, printed `:1085`). This is
  a genuine ambiguity: it is the *target* count (18), while `Nodes/Job` is the *tested*
  count (2), and the report never says the two differ. Under Pinned they are reconciled by
  `Jobs x Nodes/Job == Nodes`; under Unpinned that identity breaks. The Placement line
  carries both numbers explicitly for this reason.

`detectTestScale` (`report.go:1016-1036`) gains an Unpinned arm before the
`NodesPerJob == 1` inference at `:1031`. Without it, an Unpinned 1-node job would report as
`intra-node`, which means "every node tested independently", the exact opposite. It returns
`""`, which the function already documents as "no explicit test scale"; the Placement line
carries the real information.

`notEnoughNodesMessage` (`workflow_controller.go:306-340`) takes a placement parameter. Its
remedy, "Set nodesPerJob to %d", tells the user to shrink the job to fit the fleet, which is
the wrong framing when they asked for that size deliberately: under Unpinned it becomes
"make N qualifying nodes available rather than lowering nodesPerJob". The `archExcluded`
and `capacityExcluded` cause clauses are unchanged, and the Pinned message with no causes
stays byte-identical.

### Catalog plumbing: none

`pkg/catalog/` is untouched. `BuildConfig` gains no `Placement` field, `loader.go` never
writes `spec.Orchestration.Placement`, no entry YAML mentions it, `categoryOptionsSuffix`
needs no new term, and `nvcrectl certification run` grows no `--placement` flag. Every one
of those appeared in an earlier draft that put the mode on the Certification tier; all of
them are absent by design now, and their absence is what makes the certification invariant
hold without a guard.

The practical consequence is stated once, under "Why Certification is excluded": catalog
tuning is reachable only through Certification, so an unpinned run cannot have it.

### WorkloadRun plumbing

- `orch.Placement = spec.Orchestration.Placement` in `buildWROrchestration`
  (`workloadrun_controller.go:780`). The whole `spec.Orchestration` block is optional
  (`:781`), so `placement` is readable only inside that branch. A WorkloadRun with no
  orchestration block stays Pinned, which is the correct default.
- Unpinned is rejected together with `testScale: intra-node` or `intra-rack`. Both are
  explicit user requests for a grouping strategy, so this is the `strictDomain` rule applied
  at the WorkloadRun tier. `intra-rack` in fact lowers to `Topology{StrictDomain: true}` at
  `:792-795`, which is literally the field rejected above, so the two checks agree rather
  than merely resembling each other.
- `full-scale` needs no rejection and no handling: the switch at `:785-796` has no
  `full-scale` arm, so it already falls through to default behavior. Under Unpinned it
  becomes true for the first time, since it has always claimed "all nodes in a single group"
  (`workloadrun_types.go:152`) and never done it.
- `NodesPerJobForScale` (`:404-409`) needs no change: it already returns `spec.NumNodes` for
  anything but intra-node, which is the correct total under Unpinned.

## Rationale

**Why `OrchestrationSpec` and not a fifth `testScale` value.** `testScale` lives on
`CategoryOptions` and is consumed only by `TemplateData` (`pkg/catalog/loader.go:481-490`);
it never reaches the controller. That is exactly why `detectTestScale`
(`pkg/report/report.go:1016-1036`) has to infer it from node counts and why the
`nvcre.nvidia.com/requested-test-scale` annotation exists as a workaround. A new value would
need a second annotation round trip and would have to be added to two enums whose value sets
already disagree. `OrchestrationSpec` is the one struct both front doors converge on and the
only orchestration input the Workflow controller reads.

**Why opt-in rather than changing the default.** The sweep model is correct for
certification, which is NVCRE's primary purpose: a certification that silently skipped most
of the fleet would be worse than useless. Unpinned is a different question being asked of
the same machinery, so it gets a different answer only when requested.

**Why fix target propagation in both modes.** The propagation gap is a real defect on its
own. It is invisible in Pinned mode only because pinning masks it, and fixing it in one mode
would leave two different placement contracts to reason about. Doing it once, in both modes,
means there is a single answer to "what constrains where this pod can land".

**Why the scheduler picks the nodes.** It has live capacity, taint and affinity information
NVCRE does not, it re-evaluates at bind time rather than at discovery time, and "do not pin
any nodes" is the request being answered.

## Consequences

### Positive

- An explicit job size runs as one job of that size. The number the user wrote is the number
  that runs.
- `target` is enforced by the scheduler at bind time, in both modes, closing a latent
  correctness gap that the hostname pin was masking.
- `target.nodeSelector` now behaves the way its name and documentation imply.
- Unpinned jobs cooperate with gang schedulers and other tenants instead of claiming named
  hosts.
- `full-scale` in `workloadrun_types.go:152` becomes implementable for the first time, and
  its documentation becomes true.
- Size adjustments that were silent become explicit errors naming the field and the
  constraint.

### Negative

- Target propagation moves 33 golden files (29 integration, 4 unit) plus 7 UAT
  `expected_pods.yaml`. Every diff must be an addition of target terms to an existing
  hostname term, never a change to hostname values.
- Unpinned has no gang semantics of its own. On a contended cluster a multi-node job can get
  some pods bound and the rest Pending indefinitely unless `spec.gangScheduler` (KAI or
  Run:ai) is set. The `len(names) == nodesPerJob` attribution guard means partial placement
  is never recorded as complete, so nothing reports a false pass, but the job itself can sit
  half-scheduled. Documented as a recommendation rather than enforced as a warning.
- Two safety properties are implicit. See Notes.
- One more placement contract for contributors to hold in their head, in a controller that
  already has several grouping modes.

## Alternatives Considered

### Offer Unpinned on Certification as well

**Rejected**, and this is the decision that shaped everything else. An earlier draft of this
ADR put `placement` on `CategoryOptions` and threaded it through the catalog, the CLI and
`resolveNodesPerJob`. It was implemented, and then removed.

The reason is the one in "Why Certification is excluded": a Certification's PASSED verdict
means "every node you pointed me at was tested", an invariant held jointly by
`PartitionNodes` covering every target node and by every coverage-reducing filter forcing
INCOMPLETE. Unpinned on that tier would have been the only way to report PASSED with
targeted nodes deliberately untested.

Three repairs were considered and all are worse than exclusion:

- **Route untested nodes into `ExcludedNodes`.** Buries the cordon and architecture warnings
  the field exists for, and downgrades a correct run to INCOMPLETE. Rejected separately
  below.
- **Key the INCOMPLETE rule on placement.** The verdict then means two different things
  depending on a flag, and every consumer of a report has to know which.
- **Add a coverage line to the report and leave the verdict alone.** Honest, but it puts the
  scope caveat in prose beside a verdict that still says PASSED, which is exactly the
  reading failure the invariant exists to prevent.

Removing the tier also deleted a surprising amount of surface: a `CategoryOptions` field, a
`BuildConfig` field, a `--placement` flag, a `categoryOptionsSuffix` term, the offline-render
rejection, and the three-way clamp/snap/auto-select rework of `resolveNodesPerJob`. None of
it is needed, because `numNodes` on a WorkloadRun is already `Required` with `Minimum=1`.
The honest reading is that the Certification tier was carrying the feature's entire
complexity budget and none of its value.

The cost is stated in the Decision: catalog tuning is unreachable from WorkloadRun. That is
a separate feature and a better one than weakening the certification contract to reach it.

### Make `numNodes` narrow the default `target`

**Rejected** because a count cannot narrow a set: it does not say which members to drop, so
any implementation has to invent a selection the user never expressed. That invented
selection is the current bug. See "Why `numNodes` does not, and should not, narrow `target`"
above. Users who want a narrower set should tighten `target`, which is a set operation and
composes with any `numNodes`.

### A fifth `testScale` enum value

**Rejected** because `testScale` is a catalog template input that never reaches the
controller. See Rationale.

### Hostname `In` over the full candidate set under Unpinned

**Rejected.** The proposal was to emit `kubernetes.io/hostname In [all M filtered
candidates]` rather than label terms, on the grounds that label terms cannot reproduce the
cordon, architecture and capacity filters, whereas the candidate list *is* that filter
chain's output. The fidelity reasoning is correct and is why the architecture term is
carried explicitly. But the mechanism cannot work, for a structural reason.

The candidate list is not available when jobs are created. `discoverAndPartition` runs only
on the first reconcile, guarded by `if orch.TotalGroups == 0`
(`workflow_controller.go:182-183`). Every later reconcile skips it and falls through to
`launchPendingGroups` (`:231`). The filtered node set exists only inside that one call and
is gone by the time `createJobForGroup` runs on a subsequent pass. A non-serialized
`OrchestrationStatus.CandidateNodes []string` with `json:"-"` would be nil on every reconcile
after the first, because `orch` comes from `ensureOrchestrationStatus` (`:147`) which reads
the persisted status subresource. Jobs would get `hostname In []`, matching no node, and
every pod would hang Pending forever. The failure is silent and total.

Making it serialize replaces the nil with a worse problem: writing up to M hostnames into
status on every Workflow and into every job's pod spec.

The semantics are also wrong. `hostname In [M candidates]` says "the operator approved
exactly these M machines, by name, at this instant." What the operator said is "any node
matching this target." Those diverge the moment the fleet changes, and it is a required
`IgnoredDuringExecution` term, so a node added five minutes later is ineligible until
someone re-runs discovery. Label terms re-evaluate at bind time and are the honest encoding
of a label-based intent.

### Reject `topology` outright under Unpinned

**Rejected** because the GB200/GB300 catalog override injects
`topology.topologyKey: nvidia.com/gpu.clique` before any placement check runs, so the
motivating use case (Nemotron on GB200) would be rejected with a message about a field the
user never wrote. Only `strictDomain` is rejected. See "Reject diagnose and strictDomain;
ignore topologyKey".

### Have the Workflow controller patch the group-nodes annotation onto the Job

**Rejected** in favor of a live-discovery fallback inside the Job controller. Patching makes
the Workflow controller write to a resource the Job controller owns and reconciles, so the
two race on the same object, and it persists a point-in-time snapshot that is stale the
moment a pod reschedules. The fallback reads live state at the instant it is needed, inside
the controller that owns the question. The RBAC is not the deciding factor: the manager role
(`helm/cluster-readiness-engine/templates/manager-role.yaml:74-80`) already grants `patch`
and `update` on `nvcre.nvidia.com` jobs, and only the kubebuilder marker at
`workflow_controller.go:71` is narrower.

### CEL `XValidation` instead of Go checks

**Rejected as the sole mechanism.** Catalog-built Workflow specs are assembled in-process,
and an integration test that asserts a rejection must be able to create the object first.
CEL also cannot express the topology rule, since the conflict arises from an override
applied after admission. CEL remains available as an additional admission-time guard.

### Put untested nodes in `ExcludedNodes`

**Rejected** because `exclusionSummary` is about configuration accidents (cordoned,
wrong-architecture, under-capacity nodes), and untested nodes in a single-job run are
intentional scope. Conflating them would bury real warnings and turn a correct run
INCOMPLETE.

## Notes

### Known gaps

1. **Attribution before binding.** A job that fails before any pod is bound to a node has no
   placement to report, so `FailedNodes` is empty and `Status.FailureLog.NodeName` is the
   only signal. The broader version of this gap, terminated pods being invisible, is fixed by
   the unfiltered discoverer. This residue is not fixable, because the information does not
   exist.

2. **The INCOMPLETE rule is untouched, and unreachable from Unpinned.** `report.go:379-381`
   downgrades a PASSED run to INCOMPLETE whenever `ExcludedNodes` is non-empty, with no
   placement term. That rule is correct as written and stays that way.

   It cannot fire on an unpinned run, by construction rather than by a guard:
   `buildWorkloadRunReport` (`pkg/workloadrun/workloadrun.go:1546`) builds its `CertReport`
   from `Title`, `Name`, `Platform`, `GPU`, `FailedNodes`, `Result` and `TotalNodes`, and
   never sets `ExcludedNodes` at all. Only `Build` populates it, from the first Workflow with
   non-nil orchestration status, and `Build` runs on Certifications. Confining the mode to
   WorkloadRun is what resolves this; no placement-keyed rule exists anywhere in the report.

   An earlier draft did put Unpinned on the Certification tier, and there this was a genuine
   defect rather than a gap: an operator who asked for one 2-node job against an 18-node
   target got INCOMPLETE if any of the other 16 happened to be cordoned, for a coverage
   claim they never made. Two secondary problems came with it, and both are gone for the same
   reason: the `break` at `report.go:369` makes the exclusion set come from whichever
   category's Workflow is read first, so a mixed cert's verdict would have depended on
   category order.

3. **Render divergence is inherited, not introduced.** `pkg/render/render.go` discovers its
   own nodes and never runs the architecture, override or GPU-capacity filters, so its
   preview can differ from what the controller computes. Pre-existing, and not fixed here.

4. **`hasNodeOverlap` is a no-op for Unpinned groups.** With `candidate.Nodes == nil` the
   final loop at `workflow_controller.go:1010-1014` iterates zero times and returns false,
   which is the right answer for a single-group mode. If multiple Unpinned groups were ever
   allowed, overlap detection would be silently disabled.

5. **ADR-068 is unimplemented.** `GroupStatus.Nodes` is still serialized inline; no
   `GroupNodesRef` or ConfigMap offload exists. The node backfill uses the ordinary status
   path. If ADR-068 lands later, the backfill becomes one more site that must route through
   its hydrate/rewrite path.

6. **Two safety properties are implicit, not enforced.** Most readers of `GroupStatus.Nodes`
   are safe only because diagnose is rejected under Unpinned and because `TotalGroups` is
   always 1. Neither is asserted near the code that depends on it.
   `buildGroupBandwidthRows` (`report.go:744-790`) is the clearest example: it is gated on
   `orch.TotalGroups > 1` at `:707`, a count rather than a placement check, and its
   `nodeCount <= 4` label branch would render `group-0 ()` from an empty node list if that
   ever became reachable. The same single-group assumption is what makes gap 4 safe. If
   multi-group Unpinned is ever added, both need revisiting.

7. **Topology metrics are empty until the backfill lands.** Between job creation and the
   first backfill reconcile, an Unpinned group has no nodes, so the per-domain gauges record
   nothing. This is accurate rather than wrong, and self-corrects, but a dashboard scraped in
   that window shows a gap.

### Implementation notes

- **`pkg/orchestration/` is unchanged.** `PartitionNodes` is not called in this mode. No new
  test cases belong there.
- **`SetNodeSelector` stays unused.** It is part of the `workload.Adapter` interface
  (`pkg/workload/adapter.go:56`) and implemented (`pkg/workload/trainjob.go:79-91`), but it
  writes only the worker replicatedJob, so using it would let MPI launcher pods escape the
  target. `SetNodeAffinity` covers all replicatedJobs and is used instead.
- **The `nvcre.nvidia.com/job` label is injected into the worker replicatedJob only**, not
  the MPI launcher (`pkg/controller/cache.go:38-42`). That is correct for node attribution,
  since workers are the GPU-consuming pods.
- **ADR-023 amendment.** The blanket `Operator: Exists` toleration rationale
  (`docs/designs/023-catalog-configurability.md:113`) depends on NodeAffinity pinning and
  does not hold under Unpinned. The original decision stands for Pinned mode.

## References

- ADR-023: Catalog Configurability (`023-catalog-configurability.md`), whose toleration
  rationale is amended here
- ADR-042: Default `gpu.present` target
- ADR-063: Toleration precedence
- ADR-068: `GroupNodesRef` offload (unimplemented)
- ADR-081: Support for Cordoned Node Selection (`081-cordoned-node-selection.md`)
- `pkg/controller/workflow_controller.go:449-468`, `:474` (size checks and partition decision)
- `pkg/controller/workflow_controller.go:1373` (the pinning seam)
- `pkg/controller/placement.go` (placement helpers: validation, affinity, tolerations)
- `pkg/controller/workloadrun_controller.go:404-409` (`NodesPerJobForScale`, the one size
  both the trainer and the ComputeDomain are built from)
- `pkg/orchestration/partition.go:39-42` (the coverage invariant Unpinned opts out of, and
  the reason the mode is not offered on Certification)
- `pkg/workloadrun/workloadrun.go:1546` (`buildWorkloadRunReport`, which never populates
  `ExcludedNodes`)
- `pkg/platform/overrides/workloadrun.yaml:9-13` (the GB200/GB300 ComputeDomain override on
  the WorkloadRun path)
- `pkg/catalog/entries/_lib/deps/gb200-compute-domain-and-dra-torch.yaml:15` (ComputeDomain
  sizing from `NodesPerJob`)
