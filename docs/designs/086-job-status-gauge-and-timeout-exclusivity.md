# ADR-086: Job Status Gauge Mirrors Conditions, and the Timeout Write Is Exclusive

> **Status:** Proposed

## Context

Issue #401 reports that `nvcre_job_status` never leaves `in_progress` for a
Job that failed on `timeoutPerJob`. Every CR records the failure (the Job is
`Failed / JobTimedOut`, the Workflow `Failed / IterationsFailed`, the
Certification `Failed / WorkflowFailed`), but the gauge reads
`in_progress=1, failed=0, succeeded=0` for the rest of the run and afterwards.
The documented `NVCREJobStuck` alert does not catch it either, because the
controller keeps reconciling.

Two defects on one code path cause it.

**The gauge follows writes, not state.** `recordJobStatus` is called only
from the Job tier's own status writers, `JobReconciler.setExclusiveCondition`
and `JobReconciler.setJobFailed`
([job_controller.go](../../pkg/controller/job_controller.go)), and only when
that write changed something. Any status the Job tier did not write itself is
invisible to the gauge. Two such cases exist today:

- The `timeoutPerJob` write. When a Job exceeds its timeout, the **Workflow**
  reconciler sets `JobFailed=True / JobTimedOut` on the Job directly with
  `meta.SetStatusCondition` and `Status().Update`
  ([workflow_controller.go](../../pkg/controller/workflow_controller.go),
  `updateStatusFromJobs`). It never calls `recordJobStatus`. The Job
  reconciler that runs next sees a terminal Job and returns before any status
  write, so nothing refreshes the gauge. It keeps the last value the Job tier
  wrote, `in_progress`.
- A controller restart. The gauge lives in process memory. After a restart,
  every terminal Job takes the same early return, so its series never
  reappear.

**The timeout write is not exclusive.** `meta.SetStatusCondition` touches only
`Failed`, so a timed-out Job carries `InProgress=True` and `Failed=True`
together. Every other Job transition goes through
`setExclusiveStatusConditionUnless`
([status.go](../../pkg/controller/status.go)), which keeps exactly one of
InProgress/Succeeded/Failed `True`. The `workflow-job-timeout` integration
golden records the dual-true shape as expected output.

ADR-080 found both defects and deferred them. Decision 3 calls the
exclusivity repair "a status-correctness change [that] deserves its own
record and dedicated test and golden review", and its Consequences note that
the timeout writer "does not update the Job gauge … Metric synchronization is
a separate change." This record is that change.

PR #414 added `nvcre_certification_status` and `nvcre_workflow_status`. Their
gauges are republished from the object's persisted condition at the top of
every reconcile, through `exclusiveTrueCondition` (terminal types first) and
`metricStatusFromCondition` ([metrics.go](../../pkg/controller/metrics.go)).
It left `nvcre_job_status` alone on purpose and pointed at #401.

## Decision

1. **`nvcre_job_status` mirrors the Job's persisted conditions.** The Job
   reconciler republishes the gauge from the Job's conditions on every
   reconcile, through the same `exclusiveTrueCondition` and
   `metricStatusFromCondition` helpers the Certification and Workflow gauges
   use. A terminal condition outranks `InProgress`, so a Job that still
   carries both reports its verdict. A Job with no true phase condition
   records nothing. A Job that is being deleted is not refreshed: deletion
   removes its series through `cleanupJobMetrics`, and a refresh must not
   bring them back.

   The existing records at the Job tier's own writers stay. They update the
   gauge on the pass that makes the change, without waiting for the next
   reconcile.

2. **The timeout write sets the full exclusive set.** It sets
   `InProgress=False / NotApplicable`, `Succeeded=False / NotApplicable` and
   `Failed=True / JobTimedOut`, with `ObservedGeneration` on each. That is
   the shape `setExclusiveStatusConditionUnless` produces for every other Job
   transition. The condition loop moves out of that helper into a pure
   function, `applyExclusiveConditions`, and both callers use it. The loop
   has a single definition, so the timeout's conditions cannot drift from the
   shared helper's.

   The write itself does not change. It stays one direct `Status().Update`
   with no conflict retry, so a conflict still returns an error and the next
   reconcile retries from fresh state. ADR-080 decision 3's event rule stays
   as written: emit `Warning / JobTimedOut` on the Job when `JobFailed` was
   not `True` before the write and the write succeeds.

3. **The timeout writer records the gauge.** After its write succeeds, the
   Workflow reconciler calls `recordJobStatus` with `failed`. With decision 1
   alone the gauge would still be corrected on the next Job reconcile; this
   makes the change visible on the same pass, and it is the record every
   other Job-tier status writer already makes.

## Implementation

- `pkg/controller/status.go`
  - Extract the condition loop of `setExclusiveStatusConditionUnless` into
    `applyExclusiveConditions(conditions *[]metav1.Condition, allTypes []string, conditionType, reason, message string, generation int64) bool`.
    It returns whether any condition changed. `setExclusiveStatusConditionUnless`
    calls it in place of the loop and is otherwise unchanged.
- `pkg/controller/workflow_controller.go`, `updateStatusFromJobs`
  - Replace the timeout branch's `meta.SetStatusCondition` with
    `applyExclusiveConditions` over the Job's InProgress/Succeeded/Failed set.
  - After `Status().Update` returns `nil`, call `recordJobStatus` for
    `failed`. The `conditionFlip`-based event emission is unchanged.
- `pkg/controller/metrics.go`
  - Add `refreshJobStatusMetrics(job)`, shaped like
    `refreshWorkflowStatusMetrics`. It is a no-op when no phase condition is
    `True`.
- `pkg/controller/job_controller.go`, `Reconcile`
  - Call `refreshJobStatusMetrics` right after the deletion branch, before
    the finalizer step.

### Testing plan

`pkg/controller/` is a required-golden package.

- `TestRefreshJobStatusMetrics`, golden cases: in-progress, succeeded,
  failed, no condition, and the legacy dual-true timeout shape
  (`InProgress=True` and `Failed=True`, expected `failed=1, in_progress=0`).
- A Job reconcile case that republishes the gauge for a terminal Job
  without writing status. This is the restart case.
- `TestTimeoutEventSurvivesPodDrainReentryWithoutDuplication` also asserts
  that the persisted Job has `InProgress=False` and that the gauge reads
  `failed=1, in_progress=0` once the timeout lands.
- `TestJobPhaseWritePreservesConcurrentTerminalDecision` simulates the
  timeout as the concurrent terminal winner. Its winner now writes the
  exclusive shape, and the assertion that `InProgress` stays `True` is
  inverted. Its assertion that a discarded transition does not touch the
  gauge stays, because the refresh runs at reconcile entry, not inside the
  setters.
- Integration `workflow-job-timeout` collects the Job's gauge. Its golden
  changes `InProgress` to `False / NotApplicable` with `observedGeneration`,
  adds `observedGeneration` to `Failed`, and gains a `jobMetrics` block with
  `failed=1`. Its events are unchanged.

## Rationale

The Job tier's gauge was the only lifecycle gauge still driven by writes. A
gauge that follows writes is correct only if every writer remembers to
record, and every pass that skips the write leaves it stale. A gauge that
follows the persisted state is correct after any writer and after a
restart. Certification and Workflow moved to that model in #414. The Job
gauge now matches, using the same helpers and the same precedence, so the
three `nvcre_*_status` families behave alike.

Fixing exclusivity at the writer rather than reading around it in each
consumer keeps the invariant the rest of the code assumes. `trueConditionType`
returns the first true type in `InProgress, Succeeded, Failed` order, so a
dual-true Job reads as `InProgress` to anything that relies on it. The
existing consumers happen to check `Failed` first. The next one might not.

Keeping the timeout write as a single direct update, with no retry,
preserves the two behaviors ADR-080 and its tests depend on. A conflict
surfaces as a reconcile error, so a concurrent terminal decision by the Job
reconciler is never overwritten. The event is emitted at most once.

## Consequences

- A timed-out Job reports `failed=1, in_progress=0, succeeded=0` on
  `nvcre_job_status`, both on the pass that times it out and on every
  reconcile afterwards.
- After a controller restart, `nvcre_job_status` reappears for every
  existing Job on its first reconcile. Before, terminal Jobs had no series
  until deletion.
- New timed-out Jobs carry `InProgress=False / NotApplicable` and
  `ObservedGeneration` on all three phase conditions. Anything that read
  `InProgress=True` on a failed Job as "still running" now sees the
  verdict.
- Jobs that timed out before the upgrade keep their dual-true conditions;
  nothing rewrites a terminal Job. The gauge still reports `failed` for
  them, because terminal types outrank `InProgress`.
- `NVCREJobStuck` fires on real stalls again, provided its label matching
  is correct. The `namespace` / `job` label clash is a separate defect
  (#407).
- Every Job reconcile writes three gauge values in memory. This is
  negligible next to the reconcile's API traffic.

## Alternatives Considered

### Route the timeout write through `setExclusiveStatusConditionUnless`

Rejected. That helper retries conflicts in place against a refetched object.
The timeout path relies on a conflict failing the reconcile, and
`TestTimeoutEventSurvivesPodDrainReentryWithoutDuplication` pins that. A
retrying write would also need a terminal guard to avoid overwriting a
concurrent Job-tier verdict, and the event rule would move to the helper's
transition result. Extracting only the condition loop gets the exclusive
shape without changing any of that.

### Record the gauge only at the timeout write

Rejected. It fixes the reported symptom but not the restart case, and it
leaves the gauge correct only as long as every future writer outside the
Job tier remembers to record.

### Rely on `nvcre_workflow_status` from #414

Rejected. It gives a correct verdict per Workflow, but `nvcre_job_status`
remains documented, and the `NVCREJobStuck` alert and existing dashboards
read it. A gauge that reports a failed Job as running is a defect in its own
right.

### Repair existing dual-true Jobs on reconcile

Rejected. Rewriting the conditions of a terminal Job would also change its
`lastTransitionTime` and, through the ADR-080 transition hooks, could emit
events for a transition that happened long ago. The gauge's precedence rule
already gives these Jobs the right metric. They age out as their
Certifications are deleted.

## Notes

- This stacks on #414, which introduces `exclusiveTrueCondition`,
  `metricStatusFromCondition` and the reconcile-entry refresh for the other
  two tiers.
- ADR-080's deferred alternative "Route the Workflow timeout write through
  the shared exclusive helper" is resolved here in a narrower form: the
  shared condition loop, not the shared write.

## References

- Issue #401: `nvcre_job_status` never leaves `in_progress`
- PR #414: Certification and Workflow status gauges
- [ADR-080](080-phase-transition-events.md): Phase Transition Events Across the Lifecycle Tiers, decision 3
- Issue #407: ServiceMonitor overwrites metric labels `namespace` / `job`
