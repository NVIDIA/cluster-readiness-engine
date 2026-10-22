# ADR-086: Lifecycle Status Metrics Read from the Cache, and the Timeout Write Is Exclusive

> **Status:** Accepted

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

PR #414 (issue #410) adds `nvcre_certification_status` and
`nvcre_workflow_status`. As proposed, they use the same write-driven model as
the Job gauge, plus a refresh from the persisted condition at the top of
every reconcile and a cleanup in `handleDeletion`. Review of #414 found that
the refresh model has failures of its own:

- **Leak on a deletion that skips our finalizer.** The not-found branch of
  `Reconcile` returns without cleanup, and `handleDeletion` returns early
  once our finalizer is gone. A Workflow whose finalizers are stripped while
  it waits on Job drain or PV release keeps its series until restart.
- **Stale series after a parent-label change.** Each record writes only the
  current `certification` value. Changing or removing the Workflow's
  `nvcre.nvidia.com/certification` label leaves the old series at `1` beside
  the new ones.
- **Rollback from a lagging cache.** The refresh reads the cached object. A
  reconcile that starts before the informer has seen the previous pass's
  status write republishes the older status until the next event.
- **Untested wiring.** Removing the record and cleanup calls from both
  reconcilers leaves `go test ./pkg/controller/` green.

The first draft of this record applied the same refresh to the Job gauge, and
would have brought the same failures to the Job tier.

## Decision

This record makes two decisions. Each fixes one half of #401: Decision A makes
the metric report the Job's persisted state, and Decision B makes that state
well-formed. They are independent, and they can ship in either order.

### Decision A: lifecycle status metrics are read from the informer cache at scrape time

1. **One collector reports all three lifecycle families**:
   `nvcre_certification_status`, `nvcre_workflow_status` and
   `nvcre_job_status`. On each scrape it lists Certifications, Workflows and
   Jobs from the manager's informer cache and emits metrics from each
   object's persisted conditions. Reconcilers do not record or clean up these
   families.
2. **Names, labels and encoding do not change.** Each object has one series
   per status (`in_progress`, `succeeded`, `failed`), and exactly one of them
   is `1`. The label sets are `{namespace, certification, status}`,
   `{namespace, workflow, certification, status}` and
   `{namespace, job, workflow, status}`.
3. **An object with no phase condition `True` reports nothing.**
4. **When more than one phase condition is `True`, `Failed` wins, then
   `Succeeded`, then `InProgress`.** After Decision B, only Jobs that timed
   out before it can carry two. `Failed` first means an ambiguous object reads
   as its verdict and a failure is never hidden.
5. **The parent value comes from the parent label the controller sets**:
   `nvcre.nvidia.com/certification` on a Workflow and
   `nvcre.nvidia.com/workflow` on a Job, empty when absent. The controller
   uses the same labels to find children (the Workflow's `handleDeletion`
   lists its Jobs by `nvcre.nvidia.com/workflow`), and every other job-scoped
   metric already uses the Job's label. Because the value is read at scrape
   time, a changed label changes the reported value and leaves no old series.
6. **An object is reported until it leaves the cache**, including while it is
   being deleted. These families need no cleanup code.
7. **Only the elected leader reports.** The collector emits nothing until
   `mgr.Elected()` is closed. Without this, a `List` on a standby replica
   would start informers there and every series would be reported twice.

### Decision B: the timeout write is exclusive

The timeout write sets `InProgress=False / NotApplicable`,
`Succeeded=False / NotApplicable` and `Failed=True / JobTimedOut`, with
`ObservedGeneration` on each. That is the shape
`setExclusiveStatusConditionUnless` produces for every other Job transition.
The condition loop moves out of that helper into a pure function,
`applyExclusiveConditions`, and both callers use it. The loop has a single
definition, so the timeout's conditions cannot drift from the shared helper's.

The write itself does not change. It stays one direct `Status().Update` with
no conflict retry, so a conflict still returns an error and the next
reconcile retries from fresh state. ADR-080 decision 3's event rule stays as
written: emit `Warning / JobTimedOut` on the Job when `JobFailed` was not
`True` before the write and the write succeeds.

The timeout writer does not record any metric. Under Decision A, no writer
does.

## Implementation

### Decision A

- `pkg/controller/metrics.go` (or a new `metrics_status.go`)
  - Add `lifecycleStatusCollector`, a `prometheus.Collector` with one
    `*prometheus.Desc` per family. It reads from a source (a `client.Reader`
    and the elected channel) held in an atomic pointer. With no source set,
    it emits nothing.
  - Register the collector with `metrics.Registry` once, in `init()` beside
    the other metrics. Registering per manager would panic in the integration
    harness, which builds a manager per case.
  - Add `SetupStatusMetrics(mgr)`, which installs `mgr.GetCache()` and
    `mgr.Elected()` as the source.
  - In `Collect`, list each kind with `client.UnsafeDisableDeepCopy` (the
    collector only reads) under a bounded context, so a scrape during startup
    cannot hang on an unsynced informer. A failed `List` omits that kind from
    this scrape.
  - Pick the phase with the order in decision A4 and map it to the status
    label. If #414 lands first, reuse its condition-to-label helper.
  - Remove `jobStatusGauge` and `recordJobStatus`, and drop the gauge from
    `cleanupJobMetrics`. The other job-scoped metrics and their cleanup stay.
- `docs/operations/metrics.md`
  - In the Job status section, replace "set for all three status values on
    each update" with "on each scrape", and replace "Metrics are cleaned up
    when a Job is deleted" for `nvcre_job_status` with the scrape-time rule:
    the series come from the cache and disappear once the object does.
- `pkg/controller/job_controller.go`
  - Remove the `recordJobStatus` calls in `setExclusiveCondition` and
    `setJobFailed`.
- `cmd/manager/main.go` and `cmd/integration/integration_test.go`
  (`startManager`)
  - Call `SetupStatusMetrics(mgr)` once the manager is built.
- Certification and Workflow
  - These two families are added with #414, built on the collector instead
    of the reconcile-entry refresh, the record calls in
    `setExclusiveCondition` and the cleanup calls in `handleDeletion`. If
    #414 merges with the refresh model, the PR that adds Jobs to the
    collector removes that code for all three families.

### Decision B

- `pkg/controller/status.go`
  - Extract the condition loop of `setExclusiveStatusConditionUnless` into
    `applyExclusiveConditions(conditions *[]metav1.Condition, allTypes []string, conditionType, reason, message string, generation int64) bool`.
    It returns whether any condition changed. `setExclusiveStatusConditionUnless`
    calls it in place of the loop and is otherwise unchanged.
- `pkg/controller/workflow_controller.go`, `updateStatusFromJobs`
  - Replace the timeout branch's `meta.SetStatusCondition` with
    `applyExclusiveConditions` over the Job's InProgress/Succeeded/Failed set.
    The `conditionFlip`-based event emission is unchanged.

### Testing plan

`pkg/controller/` is a required-golden package.

Decision A:

- `TestLifecycleStatusCollector`, a `testutil.TestCaseParser` test. Each case
  loads objects into a fake client used as the source and compares the
  collector's text exposition (`promtest.CollectAndFormat`) with a `.txt`
  golden. Cases:
  - each kind in each phase;
  - no phase condition;
  - the legacy dual-true Job (`InProgress=True` and `Failed=True`, expected
    `failed=1, in_progress=0`);
  - a Workflow with and without the certification label, and a Job with and
    without the workflow label;
  - an object with a deletion timestamp (still reported);
  - not elected, and no source set (no output).
- Remove `TestRecordJobStatus` and its testdata, and the `nvcre_job_status`
  entry in `TestCleanupJobMetricsRemovesEveryJobScopedSeries`.
- `TestJobPhaseWritePreservesConcurrentTerminalDecision` drops its gauge
  assertions. Writers no longer touch the metric, so a discarded transition
  cannot reach it.
- Integration: `startManager` installs the source, so `collectJobMetrics`
  reads through the collector. `waitForCondition` already reads through the
  manager's cached client, so the collector sees the state the step waited
  for. The `job` case's `jobMetrics` block does not change.
  `workflow-job-timeout` gains a `jobMetrics` block with `failed=1`.

Decision B:

- `TestTimeoutEventSurvivesPodDrainReentryWithoutDuplication` also asserts
  that the persisted Job has `InProgress=False` once the timeout lands.
- `TestJobPhaseWritePreservesConcurrentTerminalDecision` simulates the
  timeout as the concurrent terminal winner. Its winner now writes the
  exclusive shape, and the assertion that `InProgress` stays `True` is
  inverted.
- Integration `workflow-job-timeout`: its golden changes `InProgress` to
  `False / NotApplicable` with `observedGeneration` and adds
  `observedGeneration` to `Failed`. Its events are unchanged.

## Rationale

### Decision A

A gauge that follows writes is correct only if every writer remembers to
record, and every pass that skips the write leaves it stale. #401 is that
failure. Refreshing at reconcile entry narrows the gap but keeps it: the
metric is right only after a reconcile runs, and it brings the leak, stale
series and rollback described in Context, each needing its own patch in
controller code that must be remembered and tested.

A collector derives the metric from the state each time it is read. Any
writer, a restart, any deletion path and a label change all show on the next
scrape, with no code in the reconcilers. The informer cache only moves
forward, so a scrape never reports an older status than the one before it.
Kubernetes' own controllers (attach-detach, persistent volume), cert-manager
and kube-state-metrics derive state metrics from their informer caches the
same way.

One collector for all three families gives them the same semantics: the same
precedence, the same parent-label rule, the same deletion behavior and the
same leader rule. The Job gauge stops being a special case, and so do the two
families #414 adds.

The parent value comes from labels rather than owner references because the
labels are what the controller itself uses to group children and what every
other job-scoped metric reports. Reading at scrape time removes the
stale-series problem that made a mutable label risky.

### Decision B

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

### Decision A

- A timed-out Job reports `failed=1, in_progress=0, succeeded=0` on
  `nvcre_job_status` from the first scrape after its status write reaches
  the cache, whichever reconciler wrote it.
- After a controller restart, all three families are correct from the first
  scrape after the caches sync. A scrape before then waits up to the bounded
  timeout and omits the unsynced kinds.
- An object that leaves the cache by any path, including stripped
  finalizers, drops out on the next scrape. An object being deleted keeps
  reporting its last phase until it is gone. Today the Job's series are
  removed when its finalizer starts.
- Changing a parent label changes the reported value. No old series remain.
- Status metrics lag the API server by the informer's delay, where a
  write-time record showed the change as soon as the write returned. This is
  negligible against a scrape interval.
- Each scrape lists three kinds from the cache without copying them. Jobs are
  the largest set (one per group per iteration), in the hundreds per cluster.
- Standby replicas expose no lifecycle status series.
- The other gauges (`nvcre_job_failed_nodes`, topology, goodput, bandwidth)
  stay write-driven. Most are backed by status fields and could move under the
  same rule later. That is out of scope here.
- `NVCREJobStuck` fires on real stalls again, provided its label matching is
  correct. The `namespace` / `job` label clash is a separate defect (#407).

### Decision B

- New timed-out Jobs carry `InProgress=False / NotApplicable` and
  `ObservedGeneration` on all three phase conditions. Anything that read
  `InProgress=True` on a failed Job as "still running" now sees the verdict.
- Jobs that timed out before the upgrade keep their dual-true conditions;
  nothing rewrites a terminal Job. Their metric still reports `failed`,
  because of the precedence in decision A4.

## Alternatives Considered

### Republish the gauges from persisted conditions at reconcile entry

Rejected. This was the first draft of this record, and it is the model #414
proposes. It fixes the restart case and the timeout case on the next
reconcile, but it leaks series when an object goes away without passing
through our finalizer, leaves old series when a parent label changes, and can
republish an older status from a lagging cache. Each failure needs its own
fix in controller code, and the writers still have to record for a change to
show on the same pass.

### Record the gauge only at the timeout write

Rejected. It fixes the reported symptom but not the restart case, and it
leaves the gauge correct only as long as every future writer outside the Job
tier remembers to record.

### Rely on `nvcre_workflow_status` from #414

Rejected. It gives a correct verdict per Workflow, but `nvcre_job_status`
remains documented, and the `NVCREJobStuck` alert and existing dashboards
read it. A gauge that reports a failed Job as running is a defect in its own
right.

### Export status through kube-state-metrics custom resource state

Rejected. It adds a cluster-wide dependency the chart does not control, it is
usually not configured for `nvcre.nvidia.com` resources (#410), and its
configuration cannot express the precedence rule for legacy dual-true Jobs.

### Take the parent value from the controlling owner reference

Rejected. It would change `nvcre_job_status` for Jobs that carry the workflow
label without an owner (the `job` integration case is one), and it would give
`workflow` a different meaning on `nvcre_job_status` than on the other
job-scoped metrics, which read the label. Its main benefit, immunity to label
edits, matters only when values are recorded at write time.

### Route the timeout write through `setExclusiveStatusConditionUnless`

Rejected. That helper retries conflicts in place against a refetched object.
The timeout path relies on a conflict failing the reconcile, and
`TestTimeoutEventSurvivesPodDrainReentryWithoutDuplication` pins that. A
retrying write would also need a terminal guard to avoid overwriting a
concurrent Job-tier verdict, and the event rule would move to the helper's
transition result. Extracting only the condition loop gets the exclusive
shape without changing any of that.

### Repair existing dual-true Jobs on reconcile

Rejected. Rewriting the conditions of a terminal Job would also change its
`lastTransitionTime` and, through the ADR-080 transition hooks, could emit
events for a transition that happened long ago. The precedence rule in
decision A4 already gives these Jobs the right metric. They age out as their
Certifications are deleted.

## Notes

- Decisions A and B are independent and can land in either order. #401
  closes when both have landed.
- Decision A for Certification and Workflow is implemented with #414. Jobs
  follow in a separate PR.
- ADR-080's deferred alternative "Route the Workflow timeout write through
  the shared exclusive helper" is resolved here in a narrower form: the
  shared condition loop, not the shared write.

## References

- Issue #401: `nvcre_job_status` never leaves `in_progress`
- Issue #410 and PR #414: Certification and Workflow status gauges
- [ADR-013](013-prometheus-observability.md): Prometheus Metrics and Observability
- [ADR-080](080-phase-transition-events.md): Phase Transition Events Across the Lifecycle Tiers, decision 3
- Issue #407: ServiceMonitor overwrites metric labels `namespace` / `job`
