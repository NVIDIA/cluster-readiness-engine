# ADR-079: Declarative Labels for Generated Workload Objects

> **Status:** Proposed

## Context

NVCRE's Job controller creates a framework workload object from
`Job.spec.workload`. The only current workload type is Kubeflow Trainer's
`TrainJob`, but the adapter boundary is intentionally framework-neutral. Users
can configure the workload spec and NVCRE can inject labels into the workload's
pod templates, but there is no declarative field for labels on the generated
workload object itself.

That distinction matters to admission and scheduling systems, but their paths
are not identical. Kueue selects a TrainJob's local queue from
`kueue.x-k8s.io/queue-name` on the TrainJob's `metadata.labels`, before workload
pods exist. Kubeflow Trainer's KAI guide also documents
`kai.scheduler/queue` on TrainJob metadata and describes Trainer propagating
the necessary metadata to the underlying JobSet; KAI's podgrouper then groups
pods that request `kai-scheduler`. ADR-076 already writes KAI's queue label to
the TrainingRuntime Job and pod templates, so this ADR does not claim that
pod-level KAI placement is ineffective. The remaining gap is that NVCRE cannot
produce KAI's documented TrainJob-level placement, Kueue's required placement,
or arbitrary labels for other workload-object integrations.

The gap exists on every user-facing path:

- `WorkloadRun` builds a Workflow and Job template, but exposes no workload
  object metadata.
- Certification resolves a catalog Workflow, but its category options expose no
  workload object metadata.
- A hand-authored Workflow can label the NVCRE Jobs created from
  `jobTemplate.metadata`, but those labels are deliberately metadata of the CRE
  Job, not of the external workload it creates.
- A direct Job can describe only `spec.workload`; it cannot describe metadata
  for the object constructed from that spec.

Issue #212 initially preferred adding `SetWorkloadLabels` to the Adapter
interface, following methods such as `InjectPodLabel`, `SetNodeSelector`, and
`SetTolerations`. That resemblance is superficial:

- those adapter methods locate and mutate framework-specific pod-template
  fields nested inside the workload spec;
- every generated workload already implements `client.Object`, whose labels
  are available uniformly through `GetLabels` and `SetLabels`; and
- the Job controller already sets `app.kubernetes.io/managed-by` and
  `nvcre.nvidia.com/job` on the object returned by `Adapter.Build`, outside the
  adapter.

Putting labels inside `WorkloadSpec` would also weaken its type model.
`WorkloadSpec` is a discriminated union with `MinProperties=1` and
`MaxProperties=1`; its properties select a workload framework. Metadata is not
another framework variant. Accommodating it there would require changing the
one-of validation or wrapping every framework spec in a new API shape.

ADR-076 added a configurable gang-scheduler queue label and writes it to the
Job and pod templates inside generated TrainingRuntime dependencies. It does
not write the label to the top-level TrainJob. ADR-077 identified issue #212 as
the likely third post-resolve Certification transform and set that point as the
trigger to replace duplicated call-site sequencing with a named transform
stage.

## Decision

1. **Add a narrow workload metadata API, with JobSpec as the canonical
   boundary.** Introduce:

   ```go
   type WorkloadMetadata struct {
       Labels map[string]string `json:"labels,omitempty"`
   }

   // On JobSpec, WorkloadRunSpec, and CategoryOptions respectively.
   WorkloadMetadata *WorkloadMetadata `json:"workloadMetadata,omitempty"`
   ```

   and an optional `workloadMetadata` field on `JobSpec`. `CertificationSpec`
   embeds `CategoryOptions` with `json:",inline"` as its global defaults, so the
   single `CategoryOptions.WorkloadMetadata` field surfaces both as the global
   `Certification.spec.workloadMetadata` and as the per-category
   `spec.categories[].options.workloadMetadata`; no separate top-level field is
   declared. The type intentionally
   contains only labels. It does not expose `metav1.ObjectMeta`, because users
   must not control the generated object's name, namespace, owner references,
   finalizers, or other lifecycle metadata. The nested shape leaves room for a
   separately reviewed annotations feature without committing to it now.

2. **Surface the same intent at the two higher-level entry points.** Add
   `workloadMetadata` to `WorkloadRunSpec` and `CategoryOptions`.

   - WorkloadRun copies it into the generated
     `Workflow.spec.jobTemplate.spec.workloadMetadata`.
   - Certification accepts it globally at `spec.workloadMetadata` and per
     category at `spec.categories[].options.workloadMetadata`, then copies the
     resolved labels into the catalog Workflow's Job template.
   - A hand-authored Workflow uses
     `spec.jobTemplate.spec.workloadMetadata` directly.
   - A direct Job uses `spec.workloadMetadata` directly.

   Example WorkloadRun:

   ```yaml
   spec:
     workloadMetadata:
       labels:
         kueue.x-k8s.io/queue-name: gpu-team-a
   ```

   Example Certification with a global default and a category override:

   ```yaml
   spec:
     workloadMetadata:
       labels:
         environment: burn-in
     categories:
       - domain: communication
         variant: nccl-all-reduce
         options:
           workloadMetadata:
             labels:
               kueue.x-k8s.io/queue-name: nccl-queue
   ```

3. **Use additive, deterministic label composition.** Maps are cloned before
   they are changed; API objects and catalog templates are never mutated through
   a shared map reference. For Certification, the order is:

   1. labels already present in the resolved catalog JobSpec,
   2. global Certification `workloadMetadata.labels`, and
   3. per-category `options.workloadMetadata.labels`, and
   4. the resolved gang-scheduler queue-label invariant from Decision 4.

   Steps 2 and 3 replace earlier values by key. Step 4 does not overwrite: a
   missing key is inserted, the same value is accepted, and a different value
   fails. An empty map adds nothing, and an empty string remains a valid
   Kubernetes label value; there is no deletion syntax. WorkloadRun follows the
   same composition: labels already in its generated JobSpec, then explicit
   `workloadMetadata.labels`, then the gang-scheduler invariant with the same
   insert/same/conflict rule. This construction-time merge is provisional:
   WorkloadRun's structural overrides run later and may change either metadata
   level. Decision 4 defines the final check after those overrides.

4. **Derive the TrainJob queue label from `gangScheduler` as well as allowing
   arbitrary labels.** When `gangScheduler.schedulerName` is non-empty, its
   resolved `queueLabelKey` and `queue` are added to `workloadMetadata.labels`.
   This is in addition to ADR-076's Job-template and pod-template placement.
   Consequently, existing KAI and Run:ai configurations place the same queue on
   the submitted TrainJob and on the runtime templates.

   If the user or catalog supplies the resolved queue-label key with the same
   value, the merge is idempotent. If it supplies a different value,
   WorkloadRun or Certification construction or resolved Workflow validation
   reports a conflict naming the key, expected value, actual value, and field
   path. Kueue is not inferred from `gangScheduler`; users set its label
   explicitly.

   **Persist the scheduling intent through the Workflow handoff.** Add optional
   `WorkflowSpec.GangScheduler *GangSchedulerSpec`. WorkloadRun copies its
   configured intent into the generated Workflow; Certification copies it after
   its post-resolve transforms have configured the runtime dependencies. Clone
   the value and persist the resolved queue/key defaults. This field is outside
   the `jobTemplate` and dependency override surfaces, and its presence and
   value are immutable. It lets the Workflow controller and a separately
   submitted rendered Workflow enforce the same contract without fetching the
   original WorkloadRun or relying on mutable annotations.

   **Validate after all matching overrides and before creating dependencies or
   Jobs.** A shared `ValidateResolvedGangScheduling` helper uses that persisted
   intent to check the final Job template and the referenced TrainingRuntime
   dependency on the working copy used to create children; it does not patch
   the stored Workflow's immutable metadata field. The TrainJob queue label
   follows the insert/same/conflict rule, so an override removing that label
   results in the configured label being restored on the generated Job.
   Every effective runtime Job-template and pod-template queue label must equal
   the configured queue, and pod scheduler names must agree with the configured
   scheduler. Include TrainJob `runtimePatches` when determining the effective
   values, including launcher and worker templates. A missing runtime queue
   label, a different value, or an override that redirects `runtimeRef` to a
   runtime outside the supplied dependencies fails validation; do not silently
   repair a conflicting runtime override. An unsupported runtime/patch shape
   that prevents checking these fields also fails with a specific error.

   Run this helper after the final mutation in Workflow reconciliation,
   WorkloadRun CLI resolution, and Certification's named transform stage. The
   shared dry-run path checks again before issuing API requests. WorkloadRun
   controller construction persists the intent; the Workflow controller performs
   its authoritative post-override check. WorkloadRun offline output that retains
   unresolved conditional overrides retains `spec.gangScheduler` too: it is a
   template, not a claim that every possible override branch was validated.
   Resolution on a target performs the authoritative check. Conflict failures
   mark the Workflow failed before child creation, and CLI resolution returns
   an error.

   This guarantees consistency of the effective manifests NVCRE submits when
   `gangScheduler` is configured. Direct Job users and Workflows without that
   field receive label validation and passthrough, not inferred queue checks.
   The generic `BuildObject` helper has neither runtime dependencies nor
   scheduling intent and does not claim to enforce this contract. External
   admission mutations and later edits to external workloads are outside it.

   `Certification.spec.gangScheduler` is global, so every category receives the
   same resolved queue (including the `default-queue` default). A per-category
   workload label under that same KAI/Run:ai key must agree or that category
   fails. Per-category Kueue queues remain independent because
   `kueue.x-k8s.io/queue-name` is a different key.

5. **Apply workload-object metadata generically after adapter construction.** A
   shared workload-object builder calls `Adapter.Build`, validates and merges
   `JobSpec.WorkloadMetadata.Labels` onto the returned `client.Object`, and
   returns that object. Both Job reconciliation and dry-run rendering use the
   helper. The controller then overlays its own identification labels and owner
   reference as it does today.

   Preserve adapter-produced labels with unrelated keys. For a requested key
   already present on the object, accept an identical value and reject a
   different value with a construction error; neither side silently wins.

   `Adapter.Build` remains responsible for constructing the framework-specific
   typed object from `WorkloadSpec`. The Adapter interface does not gain a
   `SetWorkloadLabels` method, and future adapters need no label-specific code.

6. **Reserve controller-owned labels and validate Kubernetes syntax.** User
   workload labels may use any valid Kubernetes label key except
   `app.kubernetes.io/managed-by` and keys in the `nvcre.nvidia.com/` prefix.
   Those labels identify controller ownership and Job association. Attempts to
   set them are rejected with a clear error rather than silently overwritten;
   the controller overlay remains as a defensive invariant.

   The `WorkloadMetadata` CRD schema uses OpenAPI/CEL validation for Kubernetes
   label-key and value syntax and for the reserved keys, so direct Job,
   Workflow, WorkloadRun, and Certification API requests fail at CRD admission.
   There is no conversion or validating webhook. Shared Go validation mirrors
   those rules for offline `nvcrectl` render and runs again in WorkloadRun and
   Certification construction and the workload-object builder as a defensive
   check.

   Bound each `labels` map to 32 entries (`maxProperties: 32`) and each value
   to 63 characters (`additionalProperties.maxLength: 63`). Label keys must
   satisfy the Kubernetes qualified-name grammar: an optional DNS-subdomain
   prefix of at most 253 characters, followed by `/`, and a non-empty name of
   at most 63 characters. Values may be empty and otherwise follow Kubernetes
   label-value syntax. These bounds are chosen to keep CEL map iteration within
   the API server's static cost budget, including the repeated metadata schema under
   Certification's up-to-64 categories. The 32-entry cap is motivated by that
   budget, but is an enforced API limit and must be documented as such.
   Validate the composed workload metadata after merging global/category
   labels and inserting any gang-scheduler queue label; exceeding 32 entries
   fails rather than truncating labels. Adapter-produced and controller-owned
   object labels are outside this metadata-map cap.

   Workload metadata is immutable in both presence and value. On `JobSpec`,
   combine a parent-level transition rule
   `has(self.workloadMetadata) == has(oldSelf.workloadMetadata)` with the
   field-level `self == oldSelf` rule on `workloadMetadata`. Field-level equality
   alone does not run when an optional field is added or removed. The parent
   rule forbids both transitions, including removing the field and re-adding a
   different queue. Equality compares the entire metadata object, so changing
   the presence or contents of its nested `labels` map is also rejected.
   These rules cover direct Jobs and typed Workflow Job templates; whole-spec
   immutability already covers WorkloadRun and Certification. Objects created
   without the field must remain without it on updates, including objects
   created before this schema change. Creation with or without it is allowed.

   Retrofitting presence immutability for the existing `nodeHealthMonitor`,
   `goodputMeasurement`, and `bandwidthMeasurement` fields is outside this ADR's
   scope and should be tracked separately.

7. **Do not infer propagation between metadata levels.** Ordinary
   `JobTemplate.metadata.labels` continue to label the CRE Job only. They are
   not copied to the generated workload, because that could unexpectedly opt a
   workload into an admission controller or queue. Conversely,
   `workloadMetadata.labels` label only the generated workload object; they are
   not copied to pod templates. Existing internal pod-label injection and
   ADR-076's explicit queue-label placement remain unchanged.

8. **Introduce the named post-resolve Certification transform stage anticipated
   by ADR-077.** Certification controller and CLI render paths invoke one
   `ApplyResolvedWorkflowTransforms` operation after catalog and platform
   overrides. Its mutation order is pinned as:

   1. `ApplyGangSchedulerToDependencies`,
   2. `ApplyImageToJobTemplate`,
   3. `ApplyImageToDependencies`, and
   4. resolve and write `JobTemplate.spec.workloadMetadata` from the already
      resolved catalog JobSpec, global and per-category labels, and the
      gang-scheduler invariant.

   Gang scheduling and image replacement still touch disjoint fields. Writing
   workload metadata last makes the resolved catalog/override JobSpec the base
   map and makes its precedence unambiguous. The named stage removes three
   separate call-site sequences and gives future post-resolve behavior one
   parity seam. After these mutations, persist the resolved gang-scheduler
   intent on the Workflow and run `ValidateResolvedGangScheduling`. Certification
   keeps ADR-076's post-override runtime rewrite; WorkloadRun instead validates
   its effective runtime after overrides, because its runtime was configured
   during construction. Both paths check any remaining runtime-patch effects.

## Implementation

- `api/v1alpha1/job_types.go`
  - Add `WorkloadMetadata` and `JobSpec.WorkloadMetadata`.
  - Put Kubernetes label syntax and reserved-key OpenAPI/CEL validation on the
    shared metadata type so every containing CRD receives the same admission
    rules.
  - Set the labels-map `maxProperties` to 32 and string-value `maxLength` to
    63 for CEL cost budgeting. Use an OpenAPI pattern for value syntax and CEL
    for map-key syntax; OpenAPI `pattern` does not constrain map keys. On the
    labels map, sketch the reserved-key rule as
    `self.all(k, k != 'app.kubernetes.io/managed-by' && !k.startsWith('nvcre.nvidia.com/'))`.
    Add key-grammar rules with the 253-character prefix and 63-character name
    bounds from Decision 6. Admission must enforce the complete syntax and
    reserved-key contract; Go validation mirrors it, not a weaker subset.
    Verify both per-rule and whole-schema cost budgets by installing all four
    generated CRDs in envtest, especially the nested Certification path.
  - Add both parent-level presence invariance and field-level equality from
    Decision 6. Reject addition, removal, and value changes on updates so a
    checkpoint restart uses the original Job's labels.
- `api/v1alpha1/workflow_types.go`
  - Add optional `WorkflowSpec.GangScheduler` using the existing shared type.
    Protect its presence at the WorkflowSpec level and its complete value with
    field-level equality, using the same two-rule pattern as workload metadata.
- `api/v1alpha1/workloadrun_types.go`
  - Add optional `WorkloadRunSpec.WorkloadMetadata`. The existing whole-spec
    immutability rule covers it.
- `api/v1alpha1/certification_types.go`
  - Add optional `CategoryOptions.WorkloadMetadata`. Because `CertificationSpec`
    inlines `CategoryOptions`, this one field is deserialized at both the global
    `spec.workloadMetadata` path and the per-category
    `spec.categories[].options.workloadMetadata` path; do not add a second
    top-level field. `ResolveOptions` merges the two per key with per-category
    values winning, per Decision 3. The existing whole-spec `self == oldSelf`
    immutability rule covers both forms.
  - Regenerate CRDs and deepcopy code with `make manifests generate` after
    implementation; generated files are not edited directly.
- `pkg/workload/metadata.go`
  - Add shared validation and map-cloning/merge helpers.
  - Add `BuildObject(adapter, name, namespace, workloadSpec, metadata)`, which
    preserves any labels produced by an adapter and merges requested labels on
    the returned `client.Object`: identical values are idempotent, conflicting
    values fail, and unrelated adapter labels survive.
- `pkg/controller/job_controller.go` and `pkg/render/render.go`
  - Replace direct `adapter.Build` calls with `workload.BuildObject`.
  - Keep controller-owned labels and the owner reference applied by the Job
    controller after the shared build.
- `pkg/controller/certification_controller.go`
  - Extend `ResolveOptions` with a cloned, per-key workload-label merge. This is
    deliberately different from the existing replacement behavior for maps,
    pointers, and slices such as `Thresholds`, `Resources`, and
    `ImagePullSecrets`; `resolved := *global` must not leave the result aliased
    to the Certification's global label map.
- `pkg/platform/transform.go`
  - Add `ApplyResolvedWorkflowTransforms` over a resolved `WorkflowSpec` and a
    small options value containing gang scheduler, image, and workload metadata.
  - Compose `ApplyGangSchedulerToDependencies`,
    `ApplyImageToJobTemplate`, `ApplyImageToDependencies`, and the final
    workload-label composition in the order fixed by Decision 8; retain those
    focused helpers and their tests.
  - Add `ValidateResolvedGangScheduling` with the persisted intent, resolved Job
    template, and runtime dependencies as inputs. Check the effective queue and
    scheduler fields after runtime patches; keep this framework-specific
    inspection separate from generic workload-object label application.
    NVCRE owns insertion of the TrainJob queue label, while runtime labels are
    assertions over dependency payloads: restore a missing TrainJob key before
    checking it, but reject missing or conflicting runtime keys without repair.
- `pkg/controller/certification_controller.go` and
  `pkg/certification/certification.go`
  - Use the named transform stage after override resolution in reconciliation,
    cluster dry-run, and offline render. Remove the duplicated image-only CLI
    wrapper.
- `pkg/controller/workloadrun_controller.go` and
  `pkg/workloadrun/workloadrun.go`
  - Merge WorkloadRun labels and the resolved gang-scheduler queue label into
    the generated JobSpec in both controller and CLI construction paths.
  - Carry the cloned, resolved gang-scheduler intent into WorkflowSpec. In CLI
    paths, validate after `ApplyOverridesWithTracking`, not only inside
    `BuildWorkflowSpec`. Preserve the intent in unresolved offline output.
- `pkg/controller/workflow_controller.go` and `pkg/render/render.go`
  - Validate the effective Workflow after overrides and before dependency/Job
    creation or dry-run API requests. Cover both initial discovery and override
    reapplication on subsequent reconciles before launching more groups. No
    subsequent override or runtime-patch mutation may bypass this check;
    revalidate if a later step changes those scheduling fields. Use the existing
    Workflow failure/status path to expose the conflict, and return an
    actionable error from CLI resolution.
- Documentation
  - Update `docs/api-reference/job.md`, `workflow.md`, `workloadrun.md`, and
    `certification.md` with field placement, precedence, reserved keys, and
    examples for Kueue and KAI/Run:ai.
  - State explicitly that workload labels are not general pod-label injection.
  - Document Workflow `gangScheduler` as the persisted consistency contract,
    including its immutable defaults and the requirement for an inspectable
    runtime dependency. Add examples of accepted and conflicting overrides.

### Testing plan

- Install all four generated CRDs in envtest to prove CEL cost-budget
  acceptance, including Certification's nested category schema. Exercise
  admission and Go validation at 32/33 labels, 63/64-character values,
  253/254-character key prefixes, and 63/64-character key names. Cover merged
  metadata and queue insertion exceeding the map cap without truncation.
- API and helper tests:
  - valid arbitrary keys and empty values;
  - invalid label keys and values;
  - rejection of `app.kubernetes.io/managed-by` and the
    `nvcre.nvidia.com/` prefix;
  - clone semantics proving source maps are not mutated;
  - global/per-category precedence and no deletion-by-empty-string behavior;
  - matching and conflicting gang-scheduler queue labels.
- Envtest admission update cases for direct Jobs and Workflow Job templates:
  absent-to-present metadata, present-to-absent, changed label values, removed
  keys, and nested labels-map presence changes are rejected; unchanged absent
  and unchanged present metadata allow unrelated updates. A removal followed
  by re-addition must fail at the removal. Cover an existing object whose stored
  spec omits the new field. Exercise the corresponding presence/value rules for
  Workflow `gangScheduler`, and the higher-level whole-spec rules. These tests
  must use generated CRDs and the API server, not just Go validators.
- Workload-object construction tests assert both `kueue.x-k8s.io/queue-name` and
  `kai.scheduler/queue` on the built TrainJob's top-level `metadata.labels`.
  Cover adapter-produced labels: unrelated keys survive, identical values
  succeed, and conflicting values fail without mutating source maps.
- Job controller integration covers a Workflow-created Job whose child
  TrainJob has user labels plus the controller-owned labels. A checkpoint
  restart case verifies the replacement TrainJob receives the same labels.
- WorkloadRun tests cover controller and CLI-render construction, including the
  queue label derived from `gangScheduler`.
- For both Certification and WorkloadRun without `workloadMetadata`, cover
  KAI and Run:ai with an explicit queue and an omitted queue. Assert the
  TrainJob receives the configured queue label with the explicit value or
  `default-queue`, respectively; controller and CLI output must agree and
  runtime queue labels must remain consistent.
- WorkloadRun override cases start with queue A, then override only the TrainJob
  label to B, only a runtime Job/pod queue label to B, or both to B. All fail
  against the persisted queue A intent. A JSON patch removing the TrainJob
  queue label restores A; removing a runtime queue label fails. Assert that a
  missing runtime label remains absent after failed validation. Include runtime
  patches changing launcher/worker labels or scheduler names,
  and replacement runtime references that cannot be validated. Matching A and
  unrelated overrides succeed. Check controller, resolved offline render, and
  `--dry-run` parity, and assert failure occurs before any dependency/Job create
  or dry-run API request on initial resolution, and before new Job creation on
  later reconciles. Submit unresolved rendered output separately to prove
  its retained intent enforces the same checks when its overrides resolve.
- Certification testutil cases cover global labels, per-category key override,
  coexistence with catalog labels, and a queue conflict. Controller, offline
  render, and `--dry-run` must produce the same resolved workload labels.
- Existing pod-template label tests remain unchanged except where the same
  gang-scheduler queue is now additionally asserted at the TrainJob level.
- Structured outputs and integration goldens follow the repository's testutil
  conventions. Golden files are regenerated only after field-by-field review
  and explicit maintainer approval.

### Validation

| Area | Required proof |
|---|---|
| CRD admission | All generated CRDs install within CEL budgets; invalid labels, exceeded bounds, and forbidden updates are rejected |
| Workflow template immutability | Adding, removing, or changing typed template workload metadata is rejected by the propagated JobSpec rules |
| Workload builder | Unrelated adapter labels survive; identical collisions succeed; conflicting values fail |
| Gang scheduling | Post-override validation restores missing TrainJob queue labels and rejects invalid runtime labels without repair |
| Existing users | Configured gang scheduling emits explicit/default queue labels without workloadMetadata |
| Controller/CLI parity | Both produce consistent resolved workload labels |
| Label placement | Workload metadata does not automatically propagate to pod labels |

## Rationale

Option B is not selected merely because it offers a nicer YAML surface. It
matches the ownership boundaries already present in the code:

| Dimension | Option A: Adapter mutator | Option B: Job-level workload metadata |
|---|---|---|
| Meaning | Treats metadata as framework-spec mutation | Treats metadata as execution policy for the object the Job owns |
| Type model | Must place metadata inside or wrap the one-of `WorkloadSpec` | Adds an orthogonal field beside the one-of union |
| Framework knowledge | Every adapter implements the same generic map copy | One implementation uses `client.Object` for every framework |
| Existing precedent | Adapter mutators navigate nested pod templates | Job controller already applies workload-object labels after `Build` |
| Entry-point plumbing | Still required separately for Job, Workflow, Certification, and WorkloadRun | The canonical JobSpec field is the propagation destination for all paths |
| Reconcile/render parity | A mutator alone does not ensure dry-run uses it | One shared builder is used by both paths |
| Future adapters | Interface and implementation change per adapter | No label-specific adapter change |

The Adapter pattern remains necessary for facts that differ by workload kind:
constructing the typed object, finding pod templates, setting replica counts,
and normalizing status. Kubernetes object metadata is the counterexample noted
by ADR-003's duck-typing alternative: metadata *can* be handled uniformly even
though status cannot. Keeping generic metadata outside the adapter makes the
adapter boundary narrower and more faithful to its purpose.

The dedicated field also avoids a dangerous shortcut: propagating every label
from the CRE Job. Labels are executable policy in many clusters. Copying a
team's organizational Job labels to TrainJob could activate Kueue, policy
webhooks, cost attribution, or custom automation unintentionally. An explicit
`workloadMetadata` field makes that effect reviewable in the submitted spec.

Finally, the Job tier is the stable convergence point. WorkloadRun and
Certification are conveniences that produce Workflows; Workflows produce Jobs;
Jobs alone own, recreate, and record the external workload. Persisting the
intent in JobSpec means checkpoint restarts use the same labels without
re-running high-level inference and direct Job/Workflow users receive the same
behavior as the convenience APIs.

## Consequences

- The Job, Workflow, WorkloadRun, and Certification CRD schemas change. All new
  fields are optional. Manifests without workload metadata or gang scheduling
  retain current behavior; existing gang-scheduler manifests intentionally gain
  TrainJob queue labels and validation of their effective scheduling fields.
- Typed workload metadata cannot be added, removed, or changed after creation.
  Changing it requires a new Job, Workflow, WorkloadRun, or Certification, as
  applicable. A Job's labels therefore remain fixed across checkpoint restarts.
  This does not make all raw Workflow overrides immutable: edits to overrides
  may affect future Jobs, whose own metadata becomes immutable at creation.
  The separately immutable Workflow gang-scheduler intent still constrains the
  queue of every generated Job after overrides.
- WorkloadRun overrides that previously changed the configured runtime queue
  or scheduler now fail the consistency check. Configure a different queue via
  `gangScheduler` on a new owning resource. Direct Jobs and Workflows without
  that intent retain responsibility for their own scheduling configuration.
- A configured gang scheduler now places its queue label on the TrainJob in
  addition to the existing runtime Job and pod templates. This is an intended
  behavior change that aligns the submitted object with Kubeflow's documented
  KAI placement.
- Labels can intentionally cause external systems to suspend, mutate, reject,
  prioritize, or account for a TrainJob. Documentation must present this as the
  purpose of the field, not as inert decoration.
- Certification map merging is additive. A per-category option can replace a
  global value, but cannot remove a global key. A user who needs a completely
  different map must avoid setting those keys globally and specify them per
  category, or use a hand-authored Workflow.
- Existing CRE Job labels, pod-template labels, controller owner references,
  and garbage-collection behavior do not change.
- The transform-stage refactor touches already-tested gang-scheduler and image
  sequencing. Focused parity tests are required even though their individual
  transformations are unchanged.

## Alternatives Considered

### Option A: add `SetWorkloadLabels` to Adapter

Rejected. It would force generic `client.Object` metadata through a
framework-specific interface and require somewhere inside the discriminated
`WorkloadSpec` to store non-discriminator data. The method would be identical
for every adapter, while all four user entry points and the dry-run path would
still need the same propagation work. The existing pod mutators are not a
precedent for this because they encode framework-specific nesting that object
metadata does not have.

### Propagate `JobTemplate.metadata.labels` automatically

Rejected. That metadata is documented and currently used for the CRE Job.
Automatic transitive propagation would also copy controller-added workflow and
group labels and could opt the external workload into policy unexpectedly. A
separate field makes the boundary explicit.

### Wrap each framework spec with its own metadata

Rejected. A shape such as `workload.trainJob.metadata` plus
`workload.trainJob.spec` is technically type-safe, but it breaks the existing
TrainJobSpec-shaped API and repeats identical metadata for every future
framework. Metadata is owned by the Job-to-workload relationship, not by a
framework adapter.

### Add only scheduler-specific fields

Rejected. A Kueue queue, KAI queue, priority class, Volcano/YuniKorn policy, or
site-specific admission label would each require another API field and mapping.
Kubernetes label keys already provide the extensibility boundary. The existing
`gangScheduler` field remains a convenience for the scheduling behavior NVCRE
configures itself.

### Expose full `metav1.ObjectMeta`

Rejected. Names, namespaces, owner references, finalizers, generation fields,
and other metadata are controller-owned or server-owned. Supporting only labels
is the minimum capability required by issue #212 and avoids implying unsafe or
unimplementable passthrough semantics.

## Notes

- Issue #212 predates Certification's reuse of `GangSchedulerSpec`. The current
  API can configure gang scheduling from Certification, but Certification still
  has no route for arbitrary workload-object labels and the queue label still
  does not reach TrainJob metadata.
- This ADR does not make arbitrary workload labels appear on JobSets or pods.
  Any system requiring another metadata level needs an explicit, separately
  tested transform for that level. ADR-076 remains the explicit queue-label
  exception.
- **ADR-078 is held by [PR #336](https://github.com/NVIDIA/cluster-readiness-engine/pull/336)**
  (`078-jobset-ownership.md`, JobSet ownership). The design index on this
  branch therefore runs `077` to `079` on purpose; ADR-080 is likewise held by
  open PR #338. The gap is merge ordering, not a free number. Whichever of
  these records merges later rebases onto the others so the index reads
  `077 / 078 / 079 / 080` without a duplicate or a silent drop.
- Retrofitting presence immutability onto the three existing optional
  `JobSpec` pointer fields (Decision 6) is a named follow-on, to be filed as
  its own issue before this record is accepted, so the CRD does not ship two
  meanings of "immutable" without a tracked owner.
- Kueue may mutate `TrainJob.spec.suspend` during admission. The workload
  adapter's Pending phase already models an admission-controlled TrainJob that
  has not started; this ADR does not add Kueue lifecycle management.
- Although TrainJob is the only current adapter, the decision stays generic so
  restoring or adding framework adapters does not require another label API.

## References

- [GitHub issue #212: Allow labels on the TrainJob created by the workload adapter](https://github.com/NVIDIA/cluster-readiness-engine/issues/212)
- [Kueue: Run a TrainJob](https://kueue.sigs.k8s.io/docs/tasks/run/trainjobs/)
- [Kubeflow Trainer: KAI Scheduler](https://trainer.kubeflow.org/en/latest/operator-guides/job-scheduling/kai.html)
- [Kubernetes: CRD validation transition rules](https://kubernetes.io/blog/2022/09/23/crd-validation-rules-beta/)
- [ADR-003: Strongly-Typed Workload Adapter Pattern](003-workload-adapter-pattern.md)
- [ADR-076: Configurable Gang Scheduler Queue Label Key](076-gang-scheduler-queue-label-key.md)
- [ADR-077: Certification Workload Image Override](077-workload-image-override.md)
- `api/v1alpha1/job_types.go` — `WorkloadSpec` and `JobSpec`
- `api/v1alpha1/workflow_types.go` — `JobTemplateSpec` and `WorkflowSpec`
- `api/v1alpha1/workloadrun_types.go` — `WorkloadRunSpec` and `GangSchedulerSpec`
- `api/v1alpha1/certification_types.go` — `CategoryOptions` and Certification spec immutability
- `pkg/workload/adapter.go` and `pkg/workload/trainjob.go` — adapter contract, object construction, and pod-template injection
- `pkg/controller/job_controller.go` — current post-build workload labels and owner reference
- `pkg/controller/workflow_controller.go` — CRE Job template metadata propagation
- `pkg/render/render.go` — independent dry-run workload construction
- `pkg/platform/gang_scheduler.go` and `pkg/platform/image.go` — existing post-resolve transforms
