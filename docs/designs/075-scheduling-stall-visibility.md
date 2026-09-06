# ADR-075: Surface Runtime Scheduling Stalls as a First-Class Job Condition

> **Status:** Proposed

## Context

When a workload's pods cannot schedule, NVCRE gives the operator no signal that
distinguishes "waiting for GPUs" from "running". The condition hierarchy
reports `InProgress=True(WorkloadRunning)` regardless.

A real incident on an on-prem 3-node HGX H200 cluster: the `nccl-loopback`
category's per-node job was hard-pinned to one node (nodeAffinity) and
requested all 8 GPUs. Two tenant pods held 3 of those GPUs. The workload pod
sat `Pending` for **7 hours** — `FailedScheduling: 1 Insufficient nvidia.com/gpu,
2 node(s) didn't match Pod's node affinity/selector` fired 114 times — while:

- the Job reported `InProgress=True(WorkloadRunning)` the whole time,
- the Workflow and Certification showed a healthy-looking run,
- `timeoutPerJob` and the startup-stall detector counted against a workload
  that never started a single process.

The operator's only discovery path was `kubectl describe pod` on a pod whose
name had to be guessed from the Job name. Nothing in `nvcrectl certification
report`, `--wait` streaming, or the status conditions said "this is not running,
it cannot even be placed".

### Why the current phases hide this

`TrainJobAdapter.GetStatus` ([trainjob.go:142-188](../../pkg/workload/trainjob.go#L142))
maps the TrainJob to exactly four phases:

- `Complete` condition → `WorkloadSucceeded`
- `Failed` condition → `WorkloadFailed`
- `spec.suspend=true` → `WorkloadPending` (deliberate: Kueue-queued time must
  not burn `timeoutPerJob` or the stall budget — issue #213)
- **everything else falls through to `WorkloadRunning`** (line 187)

A non-suspended TrainJob whose pods are unschedulable lands in the last bucket.
The Job controller's `default` branch
([job_controller.go:505-540](../../pkg/controller/job_controller.go#L505)) then
stamps `WorkloadRunning` and `WorkloadStartTime`, which starts the stall and
timeout clocks against hardware that is doing nothing.

The genuinely-pending path (suspended TrainJob) is handled correctly — but
there is no phase for "unsuspended, admitted, and still not schedulable".

Pre-flight blockers (cordoned nodes, GPU-architecture mismatch, insufficient
GPU capacity) *are* surfaced before any Job is created, via exclusion records
and events like `InsufficientGPUCapacity`
([workflow_controller.go:442-505](../../pkg/controller/workflow_controller.go#L442)).
The gap is exclusively the runtime case: pods exist, cannot be placed, and the
reason lives in pod events that nothing reads.

## Decision

Introduce a **scheduling-stall detector on the Job tier** that inspects the
workload's pods directly and reports the finding as a first-class, non-terminal
condition — without changing phase semantics, timeout accounting, or retry
behavior.

1. **New Job-tier reason: `WorkloadSchedulingBlocked`.** A new condition/reason
   in the existing `InProgress` family
   ([helpers.go:103-114](../../pkg/controller/helpers.go#L103)). It follows the
   tier-prefixed reason convention (`ReasonWorkload*`).

2. **Detection lives in the Job controller, not the adapter.** The adapter
   interface (`Adapter.GetStatus`) stays phase-only — adapters are typed to
   their workload resource and have no pod-listing capability, and adding one
   would push Kubernetes-pod knowledge into every adapter (torch, mpi, exec,
   custom) for information the Job controller already has. The Job controller
   lists pods by the existing `nvcre.nvidia.com/job: <name>` label (same
   mechanism as `NodeDiscoverer.DiscoverNodesForJob`
   [nodes.go:36-51](../../pkg/nodemonitor/nodes.go#L36) and
   `captureTimeoutLog` [workflow_controller.go:2510](../../pkg/controller/workflow_controller.go#L2510)).

3. **Classification — `WorkloadSchedulingBlocked` fires when ALL of:**
   - the workload is unsuspended and in the running path (phase would be
     `WorkloadRunning`), i.e. pods exist but the workload has never started;
   - at least one workload pod has `PodScheduled=False`
     (`status.conditions[type=PodScheduled]`, status `False`, reason
     `Unschedulable`);
   - the blocked state has persisted for a grace window (default **5 minutes**,
     overridable via `Job.spec.schedulingStallGraceSeconds`, following the
     `StartupStallTimeoutSeconds` pattern) — so a slow-but-healthy bin-packing
     event does not flap the condition.

4. **Non-terminal, clock-neutral.** Like `WorkloadPending`, a scheduling-blocked
   workload is not running: `WorkloadStartTime` stays unset, so queued/stalled
   time never counts against `timeoutPerJob` or the stall budget. The Job stays
   `InProgress`. No retry, no restart, no node-failure attribution — NVCRE does
   not modify the cluster (ADR-061); it reports.

5. **Message carries the scheduler's reason.** The condition message embeds the
   most recent `FailedScheduling` event message for the blocked pod (e.g.
   `0/3 nodes are available: 1 Insufficient nvidia.com/gpu, 2 node(s) didn't
   match Pod's node affinity/selector`). The scheduler already wrote the
   diagnosis; NVCRE's job is to relay it, not re-derive it.

6. **Recovery is automatic and silent.** If the pod schedules on a later
   reconcile, the normal `WorkloadRunning` path proceeds and the condition
   simply stops being re-asserted (`setExclusiveCondition` semantics mean the
   InProgress condition updates to `WorkloadRunning` on the next observation).
   No terminal state is ever set by this detector.

7. **Visibility paths.** The blocked reason lives on the Job CR and propagates
   upward through the existing aggregation: the Workflow's InProgress condition
   reflects the Job's reason, and `nvcrectl certification report` renders
   Job/Workflow condition chains. The `--wait` stream today prints only
   category-status transitions
   ([certification.go:1303-1312](../../pkg/certification/certification.go#L1303)),
   so a stalled category appears as an enduring `InProgress` line; surfacing
   the reason in the watch stream itself is a small waiter enhancement,
   included in scope below.

## Implementation

- **`pkg/controller/helpers.go`** — add `ReasonWorkloadSchedulingBlocked =
  "WorkloadSchedulingBlocked"` to the Job-tier reason block.
- **`pkg/controller/job_controller.go`**
  - New `checkSchedulingBlocked(ctx, job) (blocked bool, message string)`:
    lists pods by `labelJobKey`, filters to pods with `PodScheduled=False`
    and no `nodeName`, extracts the newest `FailedScheduling` event message
    per pod, returns the aggregate message.
  - Called from the `default` (running) branch **before** the
    `WorkloadStartTime`/stall logic: if blocked past the grace window, set
    `InProgress` with `ReasonWorkloadSchedulingBlocked` and requeue — skipping
    the clock-start write entirely.
  - The grace window is tracked via a first-observed-blocked timestamp on the
    Job status (`SchedulingBlockedSince *metav1.Time`, cleared on recovery) —
    persisted like `WorkloadStartTime` so controller restarts do not reset it.
- **`api/v1alpha1/job_types.go`** — add
  `SchedulingStallGraceSeconds *int32` (optional, default 300) and
  `SchedulingBlockedSince *metav1.Time` (status).
- **`pkg/certification/certification.go`** — the `--wait` watcher additionally
  prints Job/Workflow condition-reason transitions for InProgress workloads
  (e.g. `[watch] communication/nccl-all-reduce: InProgress — WorkloadSchedulingBlocked (2m)`)
  so a stalled category is distinguishable from a progressing one without
  leaving the stream.
- **Pod-list efficiency** — reuse the `PodNVCREJobIndexField` field index
  already registered for `NodeDiscoverer`; the blocked check only runs for
  workloads in the running path whose `WorkloadStartTime` is nil, so the
  common case adds no pod listings for healthy runs.

## Rationale

- **Why a condition, not an event or a phase?** An event is invisible to the
  report and the waiter (the two surfaces operators actually watch). A new
  phase would force every adapter to learn pod inspection and would blur the
  Kueue-pending contract (issue #213) that deliberately keeps queued time out
  of the clocks. A non-terminal reason inside `InProgress` is exactly what the
  state is: still in progress, specifically blocked on scheduling.
- **Why the Job tier?** Pods are the Job's direct children (via the workload);
  Workflow and Certification aggregate Job conditions upward already, so the
  reason propagates without new plumbing. This mirrors how
  `ReasonJobTimedOut` (set by the Workflow on the Job) and
  `ReasonIterationsFailed` propagate today.
- **Why a grace window?** Transient bin-packing (a pod evicted, rescheduled
  seconds later) is normal. The condition should mean "stuck", not "waited
  once". The default of 5 minutes matches the minimum meaningful
  `--wait` observation window and stays far below the smallest useful
  `timeoutPerJob`.

## Consequences

- Operators see `InProgress=True(WorkloadSchedulingBlocked)` with the
  scheduler's own message in the Job/Workflow/Certification conditions, the
  `--wait` stream, and (via status) the report — within one reconcile of the
  grace window elapsing, instead of never.
- `timeoutPerJob` no longer silently burns while pods cannot schedule (the
  clock does not start), so a 1h budget is no longer consumed by a 7h
  scheduling stall — the failure, when it eventually comes, is truthful.
- New status field `SchedulingBlockedSince` on Job (additive, optional).
- Adapters unchanged; no new permissions (pod read + event read are already
  held for node-health monitoring and log capture).
- Risk: misclassification of a slow-but-healthy scheduler as blocked is
  bounded by the grace window and self-corrects on recovery; no terminal
  state is ever written by this path.

## Alternatives Considered

- **New WorkloadPhase `WorkloadSchedulingBlocked` from adapters.** Rejected:
  pushes pod inspection into every adapter, duplicates the Job controller's
  existing pod-listing machinery, and overloads a phase contract that
  issue #213 just carefully partitioned (pending ≠ running for clock
  accounting). A phase would also claim to be a workload state when it is
  really a placement observation about pods.
- **Emit only a Kubernetes Warning event.** Rejected: invisible in the report
  and `--wait`; the scheduler's FailedScheduling event already exists — the
  problem is that nothing aggregates it into the status operators watch.
- **Treat scheduling-blocked as a stall (`ReasonWorkloadStalled`).** Rejected:
  the stall detector is goodput/log-based, requires `StallMultiplier`, and its
  clock starts from `WorkloadStartTime` — the exact field we must not set for
  a workload that never ran. Conflating them would make the stall message lie
  ("no training step observed") when the truth is "no pod placed".
- **Fail the Job immediately on detection.** Rejected: a scheduling stall is
  frequently transient (another tenant's job ends) and failing pre-empts
  recovery that Kubernetes would do for free. The operator's
  `timeoutPerJob` remains the ultimate bound — but now an honest one.

## Notes

- The incident that motivated this ADR is fully documented in the session
  log: hgx046 loopback, 2026-09-03/04, `h200-full-cert`/
  `h200-cert-r2`. The scheduler event text quoted in Decision ¶5 is verbatim
  from that incident (`kubectl describe pod` on
  `h200-full-cert-communicat-1d7-80545-workload-node-0-0-nzfv5`).
- Issue #213's pending-time exclusion is preserved exactly: suspended
  TrainJobs keep the `WorkloadPending` path with no clocks; this ADR extends
  the same protection to the unsuspended-but-unplaceable case.
- Follow-on (out of scope here): ADR for failure-log capture at terminal
  state — the second half of the observability gap hit in the same incident.

## References

- [ADR-061: NVCRE/NVSentinel remediation decoupling](061-nvcre-nvsentinel-remediation-decoupling.md) — reason vocabulary
- [ADR-068: group-nodes compressed ConfigMap](068-group-nodes-compressed-configmap.md) — ConfigMap capture precedent
- Issue #213 — pending time exclusion from timeout/stall clocks
- `pkg/workload/trainjob.go:187` — the fall-through this ADR augments


## Amendment: clock-pause semantics (post-live-testing)

Live testing on a 3-node on-prem cluster exposed a race in the original
guard: the `WorkloadStartTime != nil` early-return prevented detection in
the common case where the clock started on reconcile 1 (workload created,
clock started) and the scheduler rejected the pod on reconcile 2. The
detector's guard skipped detection precisely when the clock was already
ticking against a pod that had never run.

**Amended decision (point 4 revised):** detection runs on every reconcile
of a non-terminal workload, including after the clock started. When blocked
persists past the grace window, the caller **clears `WorkloadStartTime`**
(pauses the clock) in the same status write that sets the
`WorkloadSchedulingBlocked` condition; the first-observe logic restores the
timestamp on the next genuinely-running observation. This keeps timeout
accounting honest in both directions: a workload that never ran is never
charged, and a workload that ran and then became unschedulable is not
charged for the blocked period either.

The `schedulingBlockedSince` marker semantics are unchanged: set on first
blocked observation, cleared when any pod schedules; the caller never
writes it.

**Unit test updated:** the "WorkloadStartTime set — never blocked" case
becomes "WorkloadStartTime set — blocked still fires (clock-pause
amendment)".

**Deployment note:** the 3-node on-prem cluster validated the amendment
live: the pinned-job test produced the `WorkloadSchedulingBlocked`
condition with the scheduler's relayed message within the grace window
after this amendment.
