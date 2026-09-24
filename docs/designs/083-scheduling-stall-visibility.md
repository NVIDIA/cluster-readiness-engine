# ADR-083: Surface Runtime Scheduling Stalls as a First-Class Job Condition

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
workload's pods directly, reports the finding as a first-class, non-terminal
condition, and keeps blocked time off the timeout and stall clocks, without
changing adapter phase semantics or retry behavior.

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

3. **Classification: `WorkloadSchedulingBlocked` fires when ALL of:**
   - the workload is unsuspended and in the running path (phase would be
     `WorkloadRunning`), whether or not `WorkloadStartTime` is already set:
     the common failure is the clock starting on the reconcile that creates
     the workload and the scheduler rejecting the pod on the next one;
   - at least one workload pod has no `nodeName` and carries
     `PodScheduled=False` with reason **`Unschedulable`**. A pod with no
     `PodScheduled` condition has not been processed yet, and a pod with reason
     `SchedulingGated` is held on purpose (for example by a DRA ResourceClaim
     controller); neither is a scheduler rejection;
   - the blocked state has persisted for a grace window (default **5 minutes**,
     overridable via `Job.spec.schedulingStallGraceSeconds`, following the
     `StartupStallTimeoutSeconds` pattern), so a slow but healthy bin-packing
     event does not flap the condition.

4. **Non-terminal, clock-neutral from the first blocked observation.** The
   grace window only delays the *condition*; it does not delay the clock
   pause. The first reconcile that sees a blocked pod records
   `status.schedulingBlockedSince`, and from that instant:
   - `timeoutPerJob` is frozen: the Workflow computes elapsed runtime as
     `schedulingBlockedSince - workloadStartTime` while the marker is set;
   - stall detection (startup and training) is skipped;
   - a workload whose clock never started keeps `WorkloadStartTime` unset
     (the issue #213 pending-time contract).

   The Job stays `InProgress`. No retry, no restart, no node-failure
   attribution. NVCRE does not modify the cluster (ADR-061); it reports.

5. **Message carries the scheduler's reason.** The condition message embeds the
   most recent `FailedScheduling` event message for the blocked pod (e.g.
   `0/3 nodes are available: 1 Insufficient nvidia.com/gpu, 2 node(s) didn't
   match Pod's node affinity/selector`). The scheduler already wrote the
   diagnosis; NVCRE's job is to relay it, not re-derive it.

6. **Recovery preserves consumed runtime.** When a reconcile finds no blocked
   pod, the detector ends the episode in one status write:
   - `WorkloadStartTime` moves forward by the paused interval
     (`now - max(schedulingBlockedSince, workloadStartTime)`), so the workload
     keeps exactly the `timeoutPerJob` budget it had left before the block.
     Resetting it to "now" instead would hand every recovered workload a fresh
     budget, and repeated blocks could extend execution indefinitely;
   - `status.schedulingResumedTime` is stamped. Training-stall detection
     measures from `max(lastStepTimestamp, schedulingResumedTime)`, so a
     workload whose last step predates a long block is not declared stalled
     the moment it recovers. Startup-stall detection is covered by the shifted
     `WorkloadStartTime`, which it already clamps to;
   - `schedulingBlockedSince` is cleared and the InProgress reason returns to
     `WorkloadRunning`.

   No terminal state is ever set by this detector. A checkpoint restart clears
   both markers along with `WorkloadStartTime`.

7. **Visibility paths.**
   - **Job:** `InProgress=True(WorkloadSchedulingBlocked)` with the relayed
     scheduler message.
   - **Workflow:** while any running group's Job is blocked, the Workflow's
     `InProgress` condition carries reason `JobSchedulingBlocked` and appends
     `job <name> scheduling blocked: <Job message>` to the iteration summary.
   - **Report:** `nvcrectl certification report` shows a `Blocked:` line (JSON
     `statusDetail`) on a Running category whose Workflow reports
     `JobSchedulingBlocked`.
   - **`--wait` stream: follow-up, not in this change.** The waiter watches
     only the Certification and prints category-status transitions
     ([certification.go](../../pkg/certification/certification.go)).
     Surfacing the reason there needs either a reason field on
     `CertificationCategoryStatus` (an API change on a third tier) or a
     per-category Workflow read inside the watch loop. Until then a blocked
     category still reads `InProgress` in the stream; the report and the
     Workflow condition are the operator surfaces.

## Implementation

- **`api/v1alpha1/job_types.go`**: `spec.schedulingStallGraceSeconds` (optional,
  default 300), `status.schedulingBlockedSince` and
  `status.schedulingResumedTime`.
- **`pkg/controller/helpers.go`**: `ReasonWorkloadSchedulingBlocked` (Job tier)
  and `ReasonJobSchedulingBlocked` (Workflow tier).
- **`pkg/controller/job_controller.go`**
  - `checkSchedulingBlocked(ctx, job)` lists pods through the existing
    `PodNVCREJobIndexField` index (label-selector fallback), applies the
    classification above, owns set and clear of `schedulingBlockedSince`, and
    calls `resumeFromSchedulingBlock` on clear.
  - Called from the `default` (running) branch before the
    `WorkloadStartTime`/stall logic. Past grace it sets
    `InProgress(WorkloadSchedulingBlocked)`; within grace it requeues. Both
    paths skip stall detection.
  - `checkStallTimeout` training branch anchors on
    `max(lastStepTimestamp, schedulingResumedTime)`.
- **`pkg/controller/workflow_controller.go`**: `isJobTimedOut` freezes elapsed
  runtime at `schedulingBlockedSince`; the running-group loop raises
  `JobSchedulingBlocked` on the Workflow.
- **Event reads are uncached.** `newestFailedSchedulingMessage` lists Events
  through the manager's API reader with an `involvedObject.name` field
  selector. An Event index would start an informer that caches every Event in
  the cluster in every manager replica, for a lookup that runs only for pods
  that are already blocked.
- **`helm/.../manager-role.yaml`**: `get`/`list` on `events` (core and
  `events.k8s.io`), required by the event relay. No `watch`: nothing informs
  on Events.
- **`pkg/report/report.go`**: `CategoryReport.StatusDetail` and the `Blocked:`
  card line. The text is relayed from Events, which any principal allowed to
  create Events in the namespace can write, so it is sanitized and wrapped
  like the failure log.
- **API cost:** one indexed (cached) pod list per running-path reconcile of a
  non-terminal workload, plus one field-selected Event list against the API
  server per blocked pod.

## Rationale

- **Why a condition, not an event or a phase?** An event is invisible to the
  report and the waiter (the two surfaces operators actually watch). A new
  phase would force every adapter to learn pod inspection and would blur the
  Kueue-pending contract (issue #213) that deliberately keeps queued time out
  of the clocks. A non-terminal reason inside `InProgress` is exactly what the
  state is: still in progress, specifically blocked on scheduling.
- **Why the Job tier?** Pods are the Job's direct children (via the workload),
  and the Job controller already lists them for node discovery. The Workflow
  relays the Job's reason with a small addition to its running-group loop.
- **Why a grace window, and why it does not delay the pause?** Transient
  bin-packing (a pod evicted, rescheduled seconds later) is normal, and the
  condition should mean "stuck", not "waited once". But a timeout budget
  charged during grace can expire before the condition ever appears: a Job
  started 2 minutes ago with a 90 s `timeoutPerJob` that becomes blocked
  would be timed out and deleted by the Workflow inside the 5-minute grace
  window. The pause therefore starts at the first blocked observation, and
  grace only filters the operator-facing condition.
- **Why shift `WorkloadStartTime` instead of clearing it?** Clearing it and
  re-stamping on recovery resets the budget: a workload that ran 49 minutes,
  was blocked for 10, and recovered would get a full fresh `timeoutPerJob`.
  Shifting keeps the field's meaning ("the anchor `timeoutPerJob` is measured
  from") and needs no extra accumulated-pause field.

## Consequences

- Operators see `InProgress=True(WorkloadSchedulingBlocked)` with the
  scheduler's own message on the Job, `InProgress=True(JobSchedulingBlocked)`
  on the Workflow, and a `Blocked:` line in the certification report, within
  one reconcile of the grace window elapsing, instead of never. The `--wait`
  stream is a tracked follow-up (Decision 7).
- `timeoutPerJob` no longer burns while pods cannot schedule, grace window
  included, so a 1h budget is no longer consumed by a 7h scheduling stall, and
  the runtime consumed before a block is still charged after it.
- New status fields `schedulingBlockedSince` and `schedulingResumedTime` on
  Job, new spec field `schedulingStallGraceSeconds` (all additive, optional).
- New RBAC: the manager reads Events (`get`/`list`) for the relay.
- Adapters unchanged.
- Risk: misclassification of a slow but healthy scheduler as blocked is
  bounded by the grace window for the condition and self-corrects on
  recovery. The clock pause applies from the first observation, so a brief
  `Unschedulable` blip pauses `timeoutPerJob` for its duration; that time was
  not spent running, so this is the honest accounting.

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
  the stall detector is goodput/log-based and requires `StallMultiplier`.
  Conflating them would make the stall message lie ("no training step
  observed") when the truth is "no pod placed".
- **Accumulated-pause field instead of shifting `WorkloadStartTime`.**
  Equivalent accounting, but every consumer of the start time would need to
  subtract it. Shifting keeps a single anchor that `isJobTimedOut` and
  startup-stall detection already read.
- **Fail the Job immediately on detection.** Rejected: a scheduling stall is
  frequently transient (another tenant's job ends) and failing pre-empts
  recovery that Kubernetes would do for free. The operator's
  `timeoutPerJob` remains the ultimate bound — but now an honest one.

## Notes

- The scheduler event text quoted in Decision 5 is verbatim from the
  motivating incident (`kubectl describe pod` on the blocked loopback
  workload pod).
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

## Revision History

- **Initial proposal:** detection only while `WorkloadStartTime` was unset.
  Live testing on a 3-node on-prem cluster showed the guard skipped the common
  case (clock started on reconcile 1, pod rejected on reconcile 2).
- **First amendment:** detection on every reconcile; past grace the controller
  cleared `WorkloadStartTime` and re-stamped it on recovery. Review found that
  this charged the grace window to `timeoutPerJob`, reset the budget on
  recovery, left the training-stall budget charged for the blocked interval,
  and matched `SchedulingGated` pods.
- **Current:** the decision above. Pause from the first blocked observation,
  shift `WorkloadStartTime` on recovery, `schedulingResumedTime` for training
  stall, `Unschedulable` reason filter, Workflow and report visibility, and
  `--wait` deferred to a follow-up. Renumbered from ADR-075 (taken by the
  on-prem GB200/GB300 override).
