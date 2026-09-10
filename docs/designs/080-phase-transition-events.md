# ADR-080: Phase Transition Events Across the Lifecycle Tiers

> **Status:** Proposed

## Context

Issue #150 asked for Kubernetes Events so that `kubectl describe` explains a
resource's history without controller log access. It landed in two halves.
Commit `a331c85` wired an `EventRecorder` into the Job and Workflow
reconcilers, and #250 wired the remaining four (Certification,
GoodputMeasurement, BandwidthMeasurement, WorkloadRun) and gave each a nil-safe
`warnf` helper. Both changes emit only **Warning events tied to a failed action
on the current reconcile pass**: a child Create rejected by the API server, an
exec-framework build guard, an unresolvable LogProfile. Each site is safe
against the steady-state requeue because the action either does not recur or
recurs only when it fails again.

Issue #252 is the deliberately split-off other half: events on the phase
transitions themselves (InProgress, Succeeded, Failed). #250 excluded them
because they need dedup logic against the poll. Every reconciler requeues on a
configurable interval (15s in production), and a naive event at the status
write would fire once per pass, not once per transition.

Events RBAC and every recorder are already in place. The remaining problem is
placement and dedup. Two facts about the code shape the design.

**`changed` is not a phase transition.** The shared helper
`setExclusiveStatusCondition` ([status.go](../../pkg/controller/status.go))
returns `true` whenever any condition in the exclusive set differs after the
write: a reason or message update within the same phase (Certification
`WaitingForNodes` to `Running`, a progress message rewritten each pass), an
`ObservedGeneration` bump after a spec edit, or a caller-supplied `extra`
mutation. Emitting on `changed` would produce a Normal event on every message
refresh. Dedup has to key on which condition type flipped to `True`.

**The six reconcilers do not share one status path.** Three patterns exist:

| Tier | Status pattern |
|---|---|
| Job, Workflow, Certification | per-tier `setExclusiveCondition` wrapper over the shared `setExclusiveStatusCondition` |
| GoodputMeasurement, BandwidthMeasurement | bespoke `setComplete` / raw `meta.SetStatusCondition` with first-terminal-write-wins (ADR-072) |
| WorkloadRun | in-memory `setWorkloadRunCondition` followed by a direct `Status().Update` |

A single hook is therefore not available; the design has to say which tiers get
one and why.

Events are not `describe` output. `Recorder.Eventf` creates an `Event` API
object in the resource's namespace, stored in etcd and garbage-collected after
the API server's event TTL (one hour by default). It is read by
`kubectl describe`, by `kubectl get events --watch`, and by anything watching
the Events API: event exporters, log agents, alerting rules. The audience is
"someone watching the namespace", and the cost is write volume, not retention.
Both facts drive the scope decision below.

## Decision

1. **Emit a transition event at the four lifecycle tiers, for all three
   phases. Exclude the two measurement tiers.** Job, Workflow, Certification,
   and WorkloadRun each emit at most one event when their exclusive condition
   set flips to `InProgress`, `Succeeded`, or `Failed`. Twelve sites.

   GoodputMeasurement and BandwidthMeasurement emit no transition events. Their
   CRDs define only `Measuring` and `Complete`; there is no `Failed` phase to
   announce. Whether a measurement's result is acceptable is not the
   measurement's decision: threshold evaluation runs in the Job controller and
   records its verdict on the Job's `ValidationFailed` condition
   (`ThresholdViolation`, or `MeasurementTimeout` when measurements never
   arrive within the grace period). Every outcome a person cares about is
   therefore a Job condition write, and each has its own event below. The
   measurement tiers' real error paths (`LogProfileNotFound`) already emit a
   Warning from #250. The only excluded transition, `Measuring` to `Complete`,
   restates a status field on a namespaced child whose name the operator
   already knows. The trigger to revisit is a future `Failed` condition on a
   measurement CRD.

   **Plus the Job's two verdict conditions, `HardwareFailed` and
   `ValidationFailed`.** Both are deliberately outside the Job's exclusive
   set. `HardwareFailed` is not terminal; execution continues after it is
   set. `ValidationFailed` records the threshold verdict and is written
   `True` on a violation, a measurement timeout, or an unknown threshold key,
   and `False` with `ThresholdsMet` when thresholds pass. Neither is a phase
   transition, so the rule above would never emit for them, yet they are the
   two verdicts a burn-in exists to produce. Each gets its own event: a
   `Warning` emitted once, when the condition flips from absent or `False` to
   `True`, with the condition's reason (`HardwareFailureDetected`,
   `ThresholdViolation`, `MeasurementTimeout`, `UnknownThresholdKey`) and
   message.

   **Execution success and validation success are distinct facts, and the
   events keep them distinct.** The Job controller writes `Succeeded /
   WorkloadCompleted` when the workload finishes, *before* thresholds are
   evaluated; a violation then adds `ValidationFailed=True` without changing
   the execution phase. A Job therefore never transitions to `Failed` because
   of a threshold, and `Succeeded` announces only that the workload ran to
   completion. The tier that fails on a verdict is the Workflow, whose
   `Failed` reason becomes `JobValidationFailed` or `JobHardwareFailed`.
   Hardware failure likewise leaves the phase alone; execution continues.

   Because `Succeeded` cannot stand in for the passing verdict, the pass gets
   its own event too: a `Normal / ThresholdsMet` when `ValidationFailed` is
   first written `False`, so the Events section of a Job that ran and passed
   reads `Normal / WorkloadCompleted` then `Normal / ThresholdsMet`, and one
   that ran and failed validation reads `Normal / WorkloadCompleted` then
   `Warning / ThresholdViolation`. Without the pass event, a `Succeeded` Job
   with no verdict row would be indistinguishable from one whose validation
   has not run yet. Later passes that add nodes to `status.failedNodes` while
   `HardwareFailed` is already `True` emit nothing; the node list is read from
   status. The same three rules apply: flip detection inside the mutate
   callback of `setJobHardwareFailed` and `setJobValidationStatus`, emit after
   the write succeeds, never on a failed write. With the Workflow-driven
   timeout write from decision 3, sixteen sites in total.

2. **Dedup on the true-type flip, detected inside the write callback.** A
   transition is "the condition type that is `True` after the write differs
   from the one that was `True` before it". Reason-only and message-only
   changes within the same phase are not transitions and emit nothing. `extra`
   mutations and `ObservedGeneration` bumps emit nothing. Setting the same phase
   that is already `True` emits nothing.

   The previous true type is read from the object **inside** the mutate
   callback, on the same object state the write is computed from, so a
   conflict retry that refetches and recomputes reads the refetched state. If
   another writer already moved the object to the target phase, the retry sees
   no flip and emits nothing. Detection is not done from the caller's stale
   copy.

3. **Emit transition events after the write succeeds, never on a failed write.** The event is
   emitted by the tier wrapper after `setExclusiveStatusCondition` returns
   `nil`, using the transition it reports. A write that exhausts conflict
   retries or fails for any other reason emits no transition event: an event
   must never claim a phase the object does not have. This does not prohibit
   action-failure Warnings that describe an observed error rather than a
   persisted phase; decision 5 preserves those diagnostics on failed writes.

   This makes events **best-effort notification with transition-based
   duplicate suppression**, not an exactly-once record. A controller crash in
   the window between the successful status write and `Eventf` loses that
   event; the next reconcile sees the persisted phase, detects no flip, and
   correctly stays silent. The design accepts this: status is the source of
   truth and events are TTL-bounded diagnostics, so the failure mode is a
   missing hint, never a false one. The guarantee stated everywhere in this
   record is *at most one event per transition*.

   **One Job `Failed` write bypasses the shared helper and gets its own
   hook.** When a Job exceeds `timeoutPerJob`, the **Workflow** reconciler
   writes `JobFailed=True / JobTimedOut` on the Job directly with
   `meta.SetStatusCondition` and `Status().Update`
   ([workflow_controller.go](../../pkg/controller/workflow_controller.go),
   `updateStatusFromJobs`), not through the Job tier's wrapper. The wrapper
   hooks never see it. This write also does not clear `InProgress`, so a
   timed-out Job carries `InProgress=True` and `Failed=True` together; the
   "one previously-true type" assumption does not hold there. The rule for
   this site is therefore stated directly: emit `Warning / JobTimedOut`
   regarding the **Job**, from the Workflow reconciler's recorder, when
   `JobFailed` was not `True` before the write and the write succeeds. The
   `!ts.terminal` guard already ensures the write happens once; the pod-drain
   re-entry pass sees a terminal Job and does not rewrite. Fixing the
   exclusivity violation itself (routing this write through the shared helper
   so `InProgress` flips `False`) is out of scope: it would change every
   timeout golden's conditions and `ObservedGeneration`, and belongs in its own
   change. The wrapper's existing "status
   updated" log line and the Job tier's `recordJobStatus` metric are unchanged
   and remain keyed on `changed`, since a metric gauge and a log line are
   correctly refreshed on reason changes even though an event is not.

   `setExclusiveStatusCondition` gains a transition result alongside `changed`:
   the previously-true type (empty if none) and the newly-true type. Its four
   direct callers consume it: the Certification, Workflow, and Job
   `setExclusiveCondition` wrappers, and `JobReconciler.setJobFailed`, which
   calls the shared helper directly so that its failure-log capture and
   `FailedNodes` seeding run inside the retry closure. That direct call is
   the path every ordinary `Failed / WorkloadFailed` transition takes; it must
   emit like the wrappers, and its `extra` closure is left exactly as it is.
   WorkloadRun does not use the
   shared helper; its `setWorkloadRunCondition` is an in-memory mutation
   followed by `Status().Update`. Apply the same rule at that seam: capture the
   true type before the mutation, and emit after `Status().Update` returns
   `nil` when the type differs. Do not refactor WorkloadRun onto the shared
   helper in this change; that is an unrelated conflict-handling change.

4. **Event type follows the phase; reason and message mirror the condition.**
   `InProgress` and `Succeeded` transitions are `Normal`. `Failed` transitions
   are `Warning`. A failed phase is the case an operator has to act on, and
   the Kubernetes batch Job controller sets the same precedent
   (`BackoffLimitExceeded` is a Warning). This deviates from the issue title's
   "Normal events" for the Failed phase only, on purpose.

   The event's reason is the condition's reason (`ReasonAllWorkflowsSucceeded`,
   `ReasonThresholdViolation`, `ReasonWorkflowValidationFailed`, and so on) and
   its message is the condition's message. No new reason constants are
   introduced for transitions. `kubectl describe` then shows the same words in
   the Events section as in the Conditions block, and the existing tier-prefixed
   reason vocabulary is reused rather than doubled.

5. **The two Certification pre-Workflow validation failures are covered by the
   Failed transition; no separate site is added.** #252 also asks for a Warning
   on the two build/validation failure paths before Workflow creation
   ([certification_controller.go](../../pkg/controller/certification_controller.go),
   the two `setCertificationFailed(..., ReasonWorkflowValidationFailed, ...)`
   calls). Both go terminal through `setCertificationFailed`, so under
   decision 4 they now emit `Warning / WorkflowValidationFailed` with the
   validation error as the message, at most once. Adding a hand-placed `warnf`
   there as well would produce a redundant pair with the same reason.

   The existing action-failure Warnings are kept **unless they share a
   reason with the Failed transition on the same object.** The recorder
   correlates on `(type, reason, regarding)`, not on message, so a
   hand-placed Warning immediately followed by a Failed transition with the
   same reason is not two rows but one `Event` with `count: 2`, which the
   single-failure cases in this design forbid. Where that happens, the transition owns the
   notification on success, and the unconditional pre-write emission becomes
   a fallback on status-write failure. Three sites match
   today:

   - WorkloadRun `BuildFailed`: `warnf` followed by
     `setWorkloadRunCondition(Failed, BuildFailed)` with the same message.
   - Workflow `HeterogeneousPlatform`: `eventf(Warning)` followed by
     `setWorkflowFailed("HeterogeneousPlatform")`.
   - Workflow `OverrideError`: `eventf(Warning)` followed by
     `setWorkflowFailed("OverrideError")`.

   At each of these three sites, first attempt the Failed status write. If it
   succeeds, only the transition hook emits; no fallback is emitted. If the
   status operation returns an error, emit one action Warning for that failed
   operation, after any internal retries have finished, using a distinct
   reason: `BuildFailedStatusUpdateFailed`,
   `HeterogeneousPlatformStatusUpdateFailed`, or
   `OverrideErrorStatusUpdateFailed`, respectively. Its message includes the
   original build/platform/override error and the status-update error, and
   states that recording the Failed condition was unsuccessful. It must not
   assert that the resource entered Failed. Preserve existing error returns
   and logging. The fallback uses the same object and nil-safe recorder.

   A later successful reconcile can emit the normal Failed-transition Warning
   with the original reason; the distinct fallback reason prevents those two
   facts from being aggregated together. Repeated failed status operations
   may repeat the fallback, just as retained action-failure Warnings may
   repeat. Fallbacks are outside the transition count and volume guarantees.

   Where the reasons differ, both stay, because they describe two facts:
   Certification `WorkflowCreationError` (the Create that was rejected)
   followed by `Failed / WorkflowFailed` (the outcome). Where an action
   Warning is followed by a returned error and no Failed write (Job and
   WorkloadRun `*CreationError`), or is informational with no phase change
   (Workflow `CordonedNodesExcluded`, `HeterogeneousGPU`,
   `InsufficientGPUCapacity`), nothing changes.

6. **Integration coverage asserts real Event objects, opt-in per case.** The
   integration harness runs envtest, a real API server, so emitted events are
   real `Event` objects; no fake recorder is needed. Wire
   `mgr.GetEventRecorder("<kind>-controller")` into all six reconcilers in the
   harness, matching `main.go` (today only Workflow has one). Add an `events`
   list to the case config that names an involved object; the collector lists
   events in the test namespace for that object and serializes a
   projection for deterministic test fixtures: `type`, `reason`,
   `message`, `involvedObject.kind`, `involvedObject.name`, and `count`,
   sorted by the first five. Event names, UIDs, timestamps, source, and
   reporting instance are omitted; they are not stable across runs.

   `count` is load-bearing. The `events/v1` recorder correlates events on
   `(type, action, reason, reportingController, reportingInstance,
   regarding)`; the message is **not** part of that key. Repeated emissions
   for the same object and reason therefore do not appear as extra rows: they
   collapse into one `Event` whose `series.count` increments. A projection
   without `count` cannot distinguish one emission from fifty. The collector
   emits `count` as `series.count` when a series exists and `1` otherwise.
   For each new phase or verdict event whose case drives one qualifying
   transition, the expected count is `1`; a higher count is a dedup defect,
   not a regeneration candidate. This rule does not apply to retained action
   or informational events: repeated failed actions or multiple applied
   overrides may legitimately produce a count above `1`.

   Delivery is asynchronous: the recorder hands events to a broadcaster
   goroutine, so observing the status transition does not mean the `Event`
   object exists yet. The expected set is therefore declared in the case's
   `input_config.yaml`, not inferred:

   ```yaml
   events:
     - involvedKind: Job
       involvedName: test-timeout
       namespace: default
       expect:
         - { type: Normal,  reason: WorkloadCreated }
         - { type: Warning, reason: JobTimedOut }
   ```

   Readiness is "every `expect` row is present", checked with the harness's
   existing `require.Eventually` idiom. Once ready, the collector serializes
   **all** rows for that involved object, not only the expected ones, so an
   unexpected extra row fails the golden as loudly as a missing one. Row
   presence establishes that emission happened; it does not by itself prove
   that further reconciles did not re-emit, because a late duplicate would
   only bump `count` on a row that already passed the readiness check. The
   golden's `count: 1` is a consistency check, not the proof. The decisive
   evidence for duplicate suppression is the recorder-level tests in the
   testing plan, which drive repeated reconciles against a `FakeRecorder`
   and count calls synchronously.

   Collection is opt-in through `input_config.yaml`, so existing goldens are
   untouched: a case gains an events block only when its config carries an
   `events` list. The Workflow tier's existing `OverrideApplied` and
   `NoOverridesMatched` Normal events appear in any Workflow case that opts in.
   They are not inherently deterministic: multiple `OverrideApplied`
   emissions share a correlation key despite carrying different messages,
   so aggregation loses the individual messages and asynchronous delivery can
   affect the observed message and count. Integration fixtures in this design
   therefore arrange at most one retained emission per correlation key, and
   include each retained event in `expect` before taking the golden snapshot.
   Cases exercising multiple overrides or repeated action failures assert
   their individual messages and emission counts with `events.FakeRecorder`
   instead of golden-testing the aggregated API message or count. No
   production event is suppressed or rewritten to make a fixture stable.

## Implementation

- `pkg/controller/status.go`
  - Extend `setExclusiveStatusCondition` to report the transition: the type
    that was `True` before the mutation and the type that is `True` after, read
    inside the mutate callback on the object state the write is computed from.
    Keep `changed` and its semantics. A callback that performs no write reports
    no transition.
  - Add a small shared `transitionEventType(conditionType, failedType) string`
    or equivalent that returns `Warning` for the tier's Failed type and
    `Normal` otherwise, so the four tiers cannot drift on decision 4.
- `pkg/controller/job_controller.go`, `workflow_controller.go`,
  `certification_controller.go`
  - In each `setExclusiveCondition` wrapper **and in `setJobFailed`**, after
    the shared helper returns `nil`, emit one event when a transition is
    reported, with type from decision 4 and the condition's reason and
    message. `setJobFailed` keeps its `extra` closure (failure-log capture and
    `FailedNodes` seeding) unchanged; only the post-write emission is added
    beside its existing `recordJobStatus` call. Leave the existing log
    line and `recordJobStatus` on `changed`.
  - In `setJobHardwareFailed` and `setJobValidationStatus`, read whether the
    condition was already `True` inside the mutate callback (in the former,
    alongside the existing `isFirstFailure` computation, which is keyed on
    `failedNodes` emptiness rather than on the condition and is left for the
    metrics it drives). Emit the Warning after a successful write when the
    condition flipped to `True`, and the `Normal / ThresholdsMet` when
    `ValidationFailed` flipped from absent to `False`. A write that leaves the
    condition's status unchanged emits nothing.
  - Job and Certification currently expose only `warnf`/`normalf`; add or
    generalize to an `eventf(obj, eventType, reason, fmt, args...)` matching
    the Workflow tier so the wrapper can pass the type. Keep all helpers
    nil-safe.
- `pkg/controller/workflow_controller.go`, `updateStatusFromJobs`
  - At the `timeoutPerJob` write, read whether `JobFailed` is already `True`
    before `meta.SetStatusCondition`, and after `Status().Update` returns
    `nil` emit `Warning / JobTimedOut` regarding the Job when it was not.
    Leave the write itself, including its effect on `InProgress`, unchanged.
  - Replace the unconditional `eventf(Warning)` at the `HeterogeneousPlatform`
    and `OverrideError` sites with decision 5's fallback in the status-error
    branch. Add their distinct fallback reason constants. Successful writes
    notify through the Failed-transition hook only.
- `pkg/controller/workloadrun_controller.go`
  - Replace the unconditional `warnf(BuildFailed)` with decision 5's fallback
    when the build guard's status update fails, using the new
    `BuildFailedStatusUpdateFailed` reason constant. On success, the Failed
    transition emits with the original reason and message.
  - At each call site that runs `setWorkloadRunCondition` then
    `Status().Update`, capture the previously-true execution type before the
    mutation and emit after a successful update when it differs. Prefer one
    small helper that wraps the pair so the rule lives in one place.
- `cmd/integration/integration_test.go`
  - Wire recorders for all six reconcilers.
  - Add an `events` list to `waitConfig` with `involvedKind`, `involvedName`,
    `namespace`, and `expect: [{type, reason}]`. Readiness waits for every
    `expect` row; serialization then emits all rows for the object in the
    decision 6 projection, including `count`.
  - Keep opted-in fixtures within decision 6's single-emission constraint for
    retained events and declare those events in `expect`. Preserve their
    messages and counts in the projection; do not normalize them to `1`.
- Documentation
  - `docs/operations/troubleshooting.md` (or the closest existing
    operations page): a short section on reading the Events section of
    `kubectl describe` per tier, the Normal/Warning split, the one-hour TTL,
    and that measurement tiers report through their Job.
  - Add the new page or section to `docs/index.yml` if a new page is created.

### Testing plan

- `pkg/controller/status_test.go` (table tests are the accepted idiom in this
  file): the transition result for absent to InProgress, InProgress to
  Succeeded, InProgress to Failed, same phase with a new reason (no
  transition), same phase with a new message (no transition), `extra`-only
  mutation (no transition), and a conflict retry where the refetched object
  already holds the target phase (no transition).
- Emission tests at the recorder level, using client-go's
  `events.FakeRecorder` injected as the wrapper's `Recorder`, since the golden
  projection observes only the aggregated `Event` and the fake sees every
  call: calling the wrapper with the same phase across five consecutive
  reconciles produces one recorded event; a reason-then-message change within
  the phase produces zero further events; and a flip to a new phase produces
  exactly one more.
- Failed-write tests of the transition hooks using the existing `interceptor.Funcs{SubResourceUpdate}`
  pattern in `status_test.go`: a status update that returns a non-conflict
  error emits nothing; a conflict sequence that exhausts `retry.DefaultRetry`
  emits nothing; a conflict followed by success emits exactly once. These pin
  the "no transition event on failed write" rule from decision 3.
- Recorder-level tests for each of decision 5's three fallback sites:
  successful status persistence emits one transition and no fallback; a
  non-conflict status error emits one fallback and no transition; Workflow
  conflict exhaustion emits one fallback after retries, not one per attempt.
  A failed reconcile followed by successful persistence emits the fallback
  and then one transition under distinct reasons. Assert that fallback
  messages contain both errors and do not claim a persisted Failed phase.
- Nil-recorder pins for any new or generalized event helper, mirroring
  `TestJobWarnfNilRecorder`.
- A recorder-level case applying multiple overrides asserts one retained
  `OverrideApplied` emission per applied override, including each distinct
  message. These legitimate repeated reasons are not transition duplicates;
  aggregated API counts and messages are not golden assertions for this case.
- Integration cases with `Event` collection opted in, using
  `testutil.TestCaseParser` goldens:
  - one per lifecycle tier covering the full InProgress, then Succeeded path,
    asserting one row per phase with `count: 1`;
  - one Failed path per tier, asserting the Warning type and the condition's
    reason, using an actual execution failure for the Job (`Failed /
    WorkloadFailed` from a failed TrainJob), and Certification
    `WorkflowValidationFailed` (decision 5);
  - a Workflow-driven timeout case (decision 3): the Workflow's timeout write
    yields one `Warning / JobTimedOut` row on the Job with `count: 1`, and the
    pod-drain re-entry pass that follows adds no second row;
  - a dedup case that stays InProgress across several requeues with a changing
    reason or message (Certification `WaitingForNodes` is the natural one) and
    asserts a single InProgress event;
  - a Job hardware-failure case: the first detection yields one
    `Warning / HardwareFailureDetected` row with `count: 1`; a second pass that adds another
    failed node while the condition is already `True` emits nothing more; the
    Job's execution phase is unchanged by the verdict, and if the workload
    itself later fails, that `Failed` transition is a separate row with its
    own reason;
  - a Job threshold case in each direction, both asserting the Job stays
    `Succeeded`: a violation shows `Normal / WorkloadCompleted` then
    `Warning / ThresholdViolation` and no Job `Failed` row, with the owning
    Workflow's `Failed / JobValidationFailed` row in the same case; a pass
    shows `Normal / WorkloadCompleted` then `Normal / ThresholdsMet`;
  - a checkpoint-restart Job case asserting the restart does not emit a second
    InProgress event for the same Job when the phase does not flip;
  - a Certification `WorkflowCreationError` case asserting both the action
    Warning and the outcome Warning appear with their distinct reasons;
  - the three same-reason sites from decision 5 (WorkloadRun `BuildFailed`,
    Workflow `HeterogeneousPlatform`, Workflow `OverrideError`), each
    asserting a single transition row with `count: 1` and no fallback after
    successful status persistence. A `count: 2` here is the regression these
    tests exist to catch.
- Existing goldens must show zero diffs, since event collection is opt-in
  through the `events` list.
  Any diff in a case that did not opt in is a defect, not a regeneration
  candidate.

Golden files are regenerated only after field-by-field review and explicit
maintainer approval.

## Rationale

- **Scope follows information, not tier depth.** An event earns its etcd
  write when it is the highest-tier place a fact becomes visible. Certification
  InProgress marks the start of reconciliation on the operator's own object
  (its first reason is often `WaitingForNodes`, before any node has matched);
  the Job's verdict conditions carry the hardware and threshold findings;
  measurement Complete restates a status field whose consequence the Job's
  verdict conditions announce. Excluding the measurement tiers
  also avoids building two bespoke hooks with an ADR-072 first-terminal-write
  interaction, for events nobody would watch.
- **Flip detection, not `changed`.** The wrapper already knows the exclusive
  set; comparing which member is `True` before and after is the only signal
  that means "phase changed". Reading it inside the callback is what makes the
  conflict-retry path correct without a second read.
- **Emit-after-write is the transition safety invariant.** A transition event
  must describe a persisted phase. An action fallback instead describes the
  observed failure and unsuccessful status update, preserving diagnostics
  without claiming that the phase changed.
- **Failed as Warning matches how operators filter.** `kubectl get events
  --field-selector type=Warning` is the standard first cut. Burying a terminal
  failure under Normal would make the events feature miss the case #150 was
  filed for.
- **Reusing condition reasons keeps one vocabulary.** The tier-prefixed reason
  constants already distinguish tiers; a parallel set of event reasons would
  drift.
- **Real events over a fake recorder.** envtest already runs the API server.
  Asserting on listed `Event` objects tests the path production uses,
  including the recorder's own aggregation, and needs no test-only seam in the
  reconcilers.

## Consequences

- Events are best-effort with duplicate suppression. A crash between a status
  write and its emission drops that one event and it is not replayed. Nothing
  may treat the absence of an event as evidence that a transition did not
  happen; status remains the record.
- The new event emissions are bounded by qualifying phase and verdict changes,
  not by polls. For a Certification with N category Workflows and J actual
  Job objects, a run in which each object enters InProgress and one terminal
  phase attempts roughly `2 * (1 + N + J) + V` new emissions, where V is the
  number of qualifying Job verdict writes. With I iterations and G groups
  per category per iteration, J is approximately `N * I * G` before retries;
  additional retry Jobs must be counted too. Early failures can skip
  InProgress, and any additional actual phase flips add emissions. The
  timeout hook supplies the Job's failure event, not an extra event on top
  of it. This estimate excludes retained action and informational events,
  including status-error fallbacks, which can repeat, and is not an estimate of API writes or aggregated Event
  objects. Events expire according to the API server's TTL.
- `kubectl describe` on a Job, Workflow, Certification, or WorkloadRun now
  shows its lifecycle in the Events section. Measurement objects continue to
  show only action-failure Warnings; their outcomes are read on the Job.
- Failed transitions are `Warning`. Alerting that already filters on Warning
  events will begin to see NVCRE terminal failures, which is the intended
  effect. Anything that treated "any Warning from NVCRE" as an action failure
  needs to read the reason.
- Where an action Warning precedes a Failed transition with a different
  reason, two Warning events result. This is documented, not deduplicated.
  Where the reasons were identical, the unconditional emission is replaced at
  three sites: the transition emits on success, and a distinct action
  fallback emits if status persistence fails. On success, at WorkloadRun `BuildFailed`
  and Workflow `HeterogeneousPlatform` the message is unchanged. At Workflow
  `OverrideError` the message changes from `Override failed: …` to the
  condition's `Failed to apply overrides: …`; the reason is the same, and the
  new wording is the one already shown in the Conditions block.
- A Job that times out shows `Warning / JobTimedOut`, emitted by the
  Workflow reconciler against the Job. That Job continues to carry
  `InProgress=True` beside `Failed=True`, as it does today; this record does
  not fix that pre-existing exclusivity gap.
- The integration harness wires six recorders instead of one. Existing goldens
  are unchanged because event collection is opt-in via the `events` list; new cases carry an
  events block.
- No RBAC or chart changes. Recorders and the `events` permission already
  exist.
- `setExclusiveStatusCondition`'s signature changes. Its four direct callers
  (three tier wrappers and `setJobFailed`) are updated in the same change; no
  external callers exist.

## Alternatives Considered

### Emit whenever `setExclusiveCondition` reports `changed`

Rejected. `changed` is true on reason and message updates within a phase, on
`ObservedGeneration` bumps, and on `extra` mutations. Certification
`WaitingForNodes` alone would emit on every pass until nodes match.

### Detect the flip from the caller's copy of the object

Rejected. The caller's copy is stale after a conflict retry. If another writer
moved the object to the target phase, a stale-copy comparison reports a flip
and emits a duplicate. Reading inside the mutate callback is correct on the
retry path by construction.

### Include the measurement tiers for completeness

Rejected for now. There is no Failed phase to announce, every outcome is
already a Job phase or verdict-condition event, and real errors already have
Warnings. It would require two
bespoke hooks and reasoning about ADR-072's skipped-write path (a replay that
skips the write must also skip the event). Revisit if a measurement `Failed`
condition is ever added.

### Make every transition `Normal`, as the issue title says

Rejected. A terminal failure is the event operators filter for; classifying it
as Normal defeats `--field-selector type=Warning` and event-based alerting.
The Kubernetes batch Job controller already treats job failure as Warning.

### Emit `HardwareFailureDetected` on every pass that changes `failedNodes`

Rejected. Each newly failed node is already recorded in `status.failedNodes`
and in the `recordHardwareFailure` metric. An event per node addition would
re-fire on every detection pass during a long run; one Warning at the flip
tells the operator to look, and status carries the list.

### Let `Succeeded` stand for the passing verdict and emit nothing on `ThresholdsMet`

Rejected. An earlier draft of this record did exactly that, on the belief
that a pass was followed by `Succeeded`. The controller does the reverse:
`Succeeded / WorkloadCompleted` is written when the workload completes and
thresholds are evaluated afterwards, so `Succeeded` carries no information
about validation. Suppressing the pass event would make a Job that passed
indistinguishable, in its Events section, from one whose validation has not
run. The pass event is the only row keyed on a condition being written
`False`, and that asymmetry is stated in decision 1 rather than hidden.

### Keep every existing action Warning alongside the new transition events

Rejected. At three sites the action Warning and the Failed transition share
reason and object (and at two of them the message as well). The recorder
correlates on reason, not message, so each pair collapses into one `Event`
with `count: 2`, which would fail the single-failure case's expected `count: 1`
and would show operators an inflated count for a single failure. Ownership
moves to the transition on successful status persistence, while a distinct
fallback Warning preserves the observed error on failed persistence. On
success the reason an operator filters on is unchanged at all three sites;
the message text changes only at `OverrideError`, to the wording the
condition already carries. Removing these action diagnostics entirely would
lose visibility when the status write fails.

### Route the Workflow timeout write through the shared exclusive helper

Deferred. It would restore the invariant that only one execution phase is
`True` and let the timeout use the same hook as every other Job transition.
It would also flip `InProgress` to `False` and add `ObservedGeneration` on
every timed-out Job, changing every existing timeout golden for a reason
unrelated to events. That is a status-correctness fix and deserves its own
record and golden review.

### Treat the events as an exactly-once record of transitions

Rejected. The emit-after-write ordering guarantees an event never asserts a
phase the object lacks, but a crash between the write and the emission loses
that event, and the next reconcile correctly suppresses it. Closing that
window would require persisting an "event emitted" marker in status and
emitting on the following reconcile, trading a lost hint for an extra status
write on every transition and a stale-marker failure mode. Status is the
record; events are a best-effort pointer to it.

### Introduce dedicated event reason constants per transition

Rejected. The condition reasons already carry tier and cause. A second
vocabulary would have to be kept in sync and would show different words in
`describe`'s Events and Conditions sections for the same fact.

### Hand-place a `warnf` at the two Certification validation paths

Rejected. Under decision 4 the Failed transition already emits
`Warning / WorkflowValidationFailed` there. A second site would be a
same-reason duplicate.

### Assert events with a fake recorder in the harness

Rejected. envtest is a real API server; listing `Event` objects tests the
production path, exercises the recorder's aggregation, and requires no
test-only interface on the reconcilers. Nondeterministic fields are handled by
projection rather than by faking the sink.

### Rely on the recorder's built-in aggregation for dedup

Rejected. The events library aggregates identical events into a `count` bump
within a window; it does not suppress them, and the aggregation window is
shorter than a long-running InProgress phase. Dedup must be a property of the
emit decision.

## Notes

- `HardwareFailed` and `ValidationFailed` are written by `setJobHardwareFailed`
  and `setJobValidationStatus` through their own `updateStatusWithRetry`
  calls, not through the exclusive-set wrapper. That is why decision 1 gives
  them an explicit rule rather than assuming the phase hook covers them.
  Neither verdict changes the Job's execution phase: a threshold violation
  lands on a Job that is already `Succeeded`, and a hardware failure lands on
  one that is still running. The lifecycle consequence appears one tier up,
  as the Workflow's `Failed / JobValidationFailed` or `Failed /
  JobHardwareFailed`. Reading a Job's events top to bottom therefore gives
  "what happened" (`WorkloadCompleted`) and then "what was found"
  (`ThresholdsMet` or a verdict Warning), as two rows.
- Workflow already emits `OverrideApplied` and `NoOverridesMatched` Normal
  events outside the transition path. They are unaffected and will appear in
  Workflow cases that opt into event collection.
- The `Superseded` reason WorkloadRun writes on the non-true members of its
  exclusive set is a status detail, not an event reason; only the true type's
  reason is emitted.
- The event TTL is an API server setting (`--event-ttl`), not something NVCRE
  controls. Documentation should say so rather than promise retention.

## References

- [Issue #252: Emit Normal events on phase transitions across all six reconcilers](https://github.com/NVIDIA/cluster-readiness-engine/issues/252)
- [Issue #150: Reconcilers do not emit Kubernetes Events](https://github.com/NVIDIA/cluster-readiness-engine/issues/150)
- [PR #250: wire event recorders into the four recorder-less reconcilers](https://github.com/NVIDIA/cluster-readiness-engine/pull/250)
- Commit `a331c85`: Job and Workflow tier events
- [ADR-072: Freeze GoodputMeasurement Status at Job Terminal State](072-goodput-terminal-freeze.md)
- [ADR-000: CRD hierarchy](000-adr.md)
- [Kubernetes Events API and `--event-ttl`](https://kubernetes.io/docs/reference/kubernetes-api/cluster-resources/event-v1/)
- [`pkg/controller/status.go`](../../pkg/controller/status.go): `setExclusiveStatusCondition`, `updateStatusWithRetry`
- [`pkg/controller/workloadrun_controller.go`](../../pkg/controller/workloadrun_controller.go): `setWorkloadRunCondition`
- [`cmd/integration/integration_test.go`](../../cmd/integration/integration_test.go): recorder wiring and `collectSpec`
