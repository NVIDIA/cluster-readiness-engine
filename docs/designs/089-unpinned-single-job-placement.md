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
   `nvidia.com/gpu.present: "true"` (`pkg/certification/certification.go:905-913`), so by
   default that is the whole GPU fleet.
2. `resolveNodesPerJob` (`pkg/controller/certification_controller.go:897-920`) resolves a
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

Users do not write that path. `CertificationSpec` inlines `CategoryOptions`, so a
Certification sets `spec.placement` (or `categories[].options.placement`) and the catalog
lowers it into the Workflow's `orchestration` block. A WorkloadRun sets
`spec.orchestration.placement`, since `WorkloadRunSpec` does carry an `orchestration`
field of its own.

`OrchestrationSpec` is the one place both front doors converge: Certification via the
catalog template's `orchestration:` block, WorkloadRun via `buildWROrchestration`
(`pkg/controller/workloadrun_controller.go:776-806`). It is also the only orchestration
input the Workflow controller reads.

Under `Unpinned`:

1. Partitioning is skipped entirely. One group is emitted with no node list.
2. No `kubernetes.io/hostname` affinity is written.
3. The requested size is honored exactly, or the run fails. It is never silently adjusted.
4. `diagnose` and `topology.strictDomain` are rejected. `topology.topologyKey` is ignored.
5. Tolerations come only from explicit `target.taintSelectors`.

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
spec:
  target:
    nodeSelector: {nvidia.com/gpu.present: "true"}   # 18 nodes match
  nodesPerJob: 2                                     # required under Unpinned
  placement: Unpinned
  categories:
    - {domain: training, variant: nemotron5-8b}
```

One job, 2 nodes, 16 nodes untouched. Today the same spec gives nine jobs.

For WorkloadRun, `numNodes` plays the same role and is already `Required` with `Minimum=1`
(`workloadrun_types.go:192-195`), so nothing extra is needed there.

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

Seven fields across three files. All optional, so existing specs are unaffected. Both
`CertificationSpec` and `WorkloadRunSpec` are spec-immutable, which these inherit correctly.
Only the last two are status fields; the rest are spec.

| # | Location | Field |
|---|---|---|
| 1 | `api/v1alpha1/workflow_types.go` | constants `PlacementPinned = "Pinned"`, `PlacementUnpinned = "Unpinned"` |
| 2 | `OrchestrationSpec` | `Placement string`, enum `Pinned;Unpinned` |
| 3 | `OrchestrationOverrideSpec` | `Placement *string`, enum, no default |
| 4 | `CategoryOptions` (`certification_types.go`) | `Placement string` |
| 5 | `WorkloadOrchestration` (`workloadrun_types.go`) | `Placement string` |
| 6 | `OrchestrationStatus` | `Placement string` |
| 7 | `OrchestrationStatus` | `GPUProducts []string` |

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

Ordering matters. `applyOverridesWithTracking` runs at `workflow_controller.go:460`, before
`resolveNodesPerJob`'s output is consumed and well before the partition decision at `:568`.
An override can therefore introduce orchestration fields the user never wrote, and any
placement check must assume the post-override spec. The GB200 topology-key override is
exactly that case, and is the reason for the asymmetry in the next section.

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

These are Go checks rather than CEL `XValidation` rules. CEL is a reasonable additional
admission-time guard but cannot be the only one: catalog-built Workflow specs are assembled
in-process, and an integration test that asserts the rejection must be able to create the
object first. CEL is in any case unable to express the topology rule, since the conflict
arises from an override applied after admission.

### Partition short-circuit

`workflow_controller.go:538-606`:

- The `nodesPerJob < 1` fallback to `len(nodes)` at `:547-549` must not apply under
  Unpinned. That fallback *is* the span-the-cluster behavior. It becomes a terminal spec
  error naming `nodesPerJob` / `numNodes`.
- The `nodesPerJob > len(nodes)` terminal error at `:550-556` stays. Asking for 16 nodes on
  a 12-node fleet cannot schedule, and failing here beats pods pending forever. Note it is
  unreachable on the Certification path, because the clamp described below fires first; it
  guards WorkloadRun and hand-written Workflows.
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

**Branching on placement** at `workflow_controller.go:1166-1187`:

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

`aws-h100-efa-resources-training.yaml:10` and `gb200-compute-domain-and-dra-comm.yaml:15`
do the same. These are rendered by the catalog at Certification time from
`BuildConfig.NodesPerJob`, which under Unpinned is the user's exact requested size. So the
ComputeDomain is sized correctly for free.

This is why skipping `SetNumNodes` is not interchangeable with clamping or zeroing it.
`SetNumNodes` rewrites the trainer's `numNodes`, but nothing rewrites the ComputeDomain's.
With `group.Nodes == nil`, `len(group.Nodes)` is 0, so `SetNumNodes(spec, 0)` would write
`numNodes: 0` (`pkg/workload/trainjob.go:134-140`) and the job would never run. Any other
adjusted value would desynchronize the two: a ComputeDomain sized for N channels against a
TrainJob asking for a different count, which on GB200 means pods that never get their DRA
allocation and hang Pending with no error naming the cause. Skipping keeps a single source
of truth, the templated `NodesPerJob`, for both.

It is also why rejecting rather than adjusting the size is load-bearing beyond user-facing
honesty. Under Pinned, a clamp or a snap flows into both the trainer and the ComputeDomain
together, so they stay consistent even when adjusted. Under Unpinned, rejecting is what
guarantees the two stay equal to the number the user wrote.

### Honoring the size: four doors

Three mechanisms currently change the requested size, all silent, and all must become errors
under Unpinned:

1. **The clamp.** `resolveNodesPerJob` (`certification_controller.go:904-906`) does
   `ceiling = min(*opts.NodesPerJob, n)`. Ask for 8 on a 4-node fleet and silently get 4.
   This also means the Workflow-tier `nodesPerJob > len(nodes)` check never fires on the
   Certification path.
2. **The constraint snap.** `entry.MaxValidNodes(ceiling, ...)` (`:909-916`) returns the
   largest *valid* count at or below the ceiling, enforcing minGPUs and TP/PP divisibility.
   Ask for 6 and silently get 4. Reasonable under the sweep model, wrong when the user named
   a size.
3. **The auto-select.** With `nodesPerJob` unset, `ceiling = n` (`:903`) and the job gets
   **every matching node**. This is the worst one: it is exactly the span-the-cluster
   behavior being escaped, and it would survive `placement: Unpinned` untouched, since 18 is
   a perfectly valid `NodesRequired` and nothing downstream objects.

The size reaches a job through four doors, and all four need the check:

| Door | Today | Under Unpinned |
|---|---|---|
| `certification_controller.go:543` | clamp, snap, or auto-select to all nodes | require explicit `nodesPerJob`, reject rather than adjust |
| `certification.go:453-460` (offline render) | defaults to 1 | reject, same message |
| `certification.go:922-924` (CLI `run`) | omits the field when the flag is 0 | reject before the object is created |
| `workflow_controller.go:547-549` | falls back to `len(nodes)` | terminal spec error (backstop only) |

`resolveNodesPerJob` takes a placement argument. Under Pinned all four are unchanged:
clamping to `min(requested, available)` and `MaxValidNodes` validation remain exactly right
for a chunk size.

The check belongs in `createWorkflowForCategory`, after
`opts := ResolveOptions(&certification.Spec.CategoryOptions, category.Options)`
(`certification_controller.go:509`), not at the Certification level. Both `nodesPerJob` and
`placement` live on `CategoryOptions`, so both arrive already merged global-over-per-category
and the check is naturally per-category. A cert with `placement: Unpinned` globally and
`nodesPerJob` set on only one of three categories must fail for the other two, and a
Certification-level check cannot see that.

The CLI door matters on its own: `--nodes-per-job` defaults to 0 (`certification.go:769`)
and `buildConfigFromFlags` only sets `cert.Spec.NodesPerJob` when `> 0` (`:922-924`), so
`nvcrectl certification run --placement Unpinned` with no `--nodes-per-job` would build a
Certification with a nil field and submit it. The controller rejects it, but only after the
object exists and a reconcile has run, so the user sees a Failed Certification instead of a
flag error.

**Error classification.** The new rejections return a plain error, so they land in the
default branch at `certification_controller.go:225-230`:
`setCertificationFailed(ReasonWorkflowValidationFailed)` plus `return ctrl.Result{}, err`.
That is how the existing `no valid node count <= %d satisfies model constraints` error at
`:911-913` is already handled, so the new ones inherit proven behavior. They are
deliberately **not** `retryableCreateError` (`:707`): a missing field does not fix itself on
the next reconcile, and wrapping it that way would requeue silently forever with no Failed
condition.

### Tolerations

ADR-023 justifies the blanket `Operator: Exists` toleration on the grounds that "workloads
are already pinned to specific nodes via NodeAffinity"
(`docs/designs/023-catalog-configurability.md:113`). Under Unpinned they are not, so blanket
Exists plus no host pin would let an MPI job land on any tainted node with free GPUs.

Under Unpinned, only explicit `target.taintSelectors` tolerations are applied, which is
branch 1 of the existing precedence at `workflow_controller.go:1189-1202`. The MPI fallback
is dropped. This is a scoped amendment to ADR-023, linked from its Notes; the original
decision stands for Pinned mode, where its premise still holds.

### Node attribution from actual placement

With no pin, `GroupStatus.Nodes` is empty at job creation, so it is backfilled from where
pods actually landed. Two independent write-backs, each in the controller that owns the data.

**`GroupStatus.Nodes`** (Workflow controller). In `updateStatusFromJobs`
(`workflow_controller.go:1342`), inside the per-group loop, gated on Unpinned and
`len(g.Nodes) == 0`. A `len(names) == nodesPerJob` guard ensures partial placement does not
persist an incomplete list that later reads treat as complete. Setting
`statusChanged = true` routes it through the existing `applyGroups` retry closure, so there
is no new write path. This requires adding a `NodeDiscoverer` to `WorkflowReconciler` and
wiring it in `SetupWithManager` the way `job_controller.go:1674` does. The Workflow
controller already has pod read RBAC (`workflow_controller.go:77`).

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

**Iteration reset clears the node list.** `handleIterationComplete`
(`workflow_controller.go:1671-1679`) resets Phase, JobRef, StartTime, CompletionTime and
Retries, but not `Nodes`. Under Unpinned, iteration 2 would inherit iteration 1's list and
the `len(g.Nodes) == 0` guard would never re-fire, so iteration 2's real placement would
never be recorded. `Nodes = nil` is added to that loop for Unpinned.

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

`notEnoughNodesMessage` (`workflow_controller.go:288-317`) takes a placement parameter. Its
remedy, "Set nodesPerJob to %d", tells the user to shrink the job to fit the fleet, which is
the wrong framing when they asked for that size deliberately. The `archExcluded` and
`capacityExcluded` cause clauses are unchanged.

### Catalog and CLI plumbing

- `Placement string` on `BuildConfig` (`pkg/catalog/catalog.go:98-100`).
- Set in Go, not in templates. In `loader.go`, next to the existing
  `spec.Orchestration.Target = &target` at `:411`:

  ```go
  if config.Placement != "" {
      spec.Orchestration.Placement = config.Placement
  }
  ```

  This is one line instead of a `{{- if .Placement }}` conditional in each of eight entry
  YAMLs. It works uniformly whether an entry's orchestration block is a bare `iterations: 1`
  or a richer NCCL block, and carries no risk of whitespace or conditional-nesting mistakes
  perturbing rendered output. `TemplateData` needs no new field.
- `Placement: opts.Placement` in the `BuildConfig` literal
  (`certification_controller.go:554-583`), the per-category merge in `ResolveOptions`
  alongside `SourceRepo`, and the offline render path (`pkg/certification/certification.go:466`).
- `Placement` added to `categoryOptionsSuffix` (`certification_controller.go:1098-1128`), so
  two categories differing only in placement hash to distinct Workflow names instead of
  colliding.
- `--placement` flag on `nvcrectl certification run`.

### WorkloadRun plumbing

- `orch.Placement = spec.Orchestration.Placement` in `buildWROrchestration`
  (`workloadrun_controller.go:776-806`). The whole `spec.Orchestration` block is optional
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

2. **Exclusions still force INCOMPLETE.** Cordon, architecture and capacity exclusions
   populate `orch.ExcludedNodes` regardless of placement, so an Unpinned run on a
   heterogeneous or partly-cordoned fleet still reports INCOMPLETE even though the operator
   made no coverage claim. This is deliberate: the operator should learn the fleet is
   heterogeneous.

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
- `pkg/controller/workflow_controller.go:538-606` (partition decision)
- `pkg/controller/workflow_controller.go:1166-1187` (the pinning seam)
- `pkg/controller/certification_controller.go:892-920` (`resolveNodesPerJob`)
- `pkg/orchestration/partition.go:39-42` (the coverage invariant Unpinned opts out of)
- `pkg/catalog/entries/training/nemotron5-8b.yaml:288-296` (the GB200 topology override)
- `pkg/catalog/entries/_lib/deps/gb200-compute-domain-and-dra-torch.yaml:15` (ComputeDomain
  sizing from `NodesPerJob`)
