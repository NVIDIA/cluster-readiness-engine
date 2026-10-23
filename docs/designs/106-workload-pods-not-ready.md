# ADR-106: Surface Workload Pods That Are Placed but Never Become Ready

> **Status:** Proposed

## Context

ADR-093 made the MPI worker's readiness probe open a real `ssh` session to the worker itself, so a worker whose `sshd` cannot serve sessions stays `0/1` and the launcher's `dependsOn: [{name: node, status: Ready}]` gate never releases. That moved the failure to where it starts, but it also moved it out of sight. ADR-093's Consequences say so directly: no NVCRE detector reports the state. `checkSchedulingBlocked` acts only on pods the scheduler could not place, and `checkStallTimeout` returns before it looks unless `stallMultiplier` is set, which the communication categories do not set. The Job reports `InProgress=True(WorkloadRunning)` until `timeoutPerJob`, which is 1h for the communication categories (`pkg/catalog/loader.go:67`) and 24h for a WorkloadRun. The diagnosis, `Readiness probe failed:` followed by `ssh`'s own stderr, exists only in `kubectl describe pod` on a worker. Issue #469 tracks relaying it.

The same blind spot covers a pod that is placed and never starts at all. A worker stuck in `ImagePullBackOff`, `CreateContainerConfigError` or a wedged init container is `Pending` with `spec.nodeName` set. The scheduler has done its part, so ADR-083 does not fire, and the Job again waits out `timeoutPerJob` reporting `WorkloadRunning`.

What the controller can see today, and what it cannot:

- **The pod list.** Since ADR-102, `checkSchedulingBlocked` reads `podread.JobPods` (`pkg/controller/job_controller.go:1088`), which answers from the job-label-scoped cache. Only the worker replicatedJob's pod template carries `nvcre.nvidia.com/job` (`InjectPodLabel` at `pkg/workload/trainjob.go:61`; `workerTargetJob` skips the launcher), and `pkg/podread/podread.go:173` states it outright: launchers carry no job label, so the cache cannot hold them. Any rule that reads "the launcher does not exist" off that list is always true, silently.
- **The pod's own status.** For a `Running` pod that fails its readiness probe, the kubelet sets `ContainersReady=False` with reason `ContainersNotReady` and the message `containers with unready status: [node]`. That names the container and nothing else. For a `Pending` pod, `status.containerStatuses[].state.waiting` (and `status.initContainerStatuses[]`) carry the kubelet's reason and message, for example `ImagePullBackOff` with the pull error.
- **Events.** The probe output lives only in the `Unhealthy` Event the prober emits for the pod (`Readiness probe failed: ...`). Nothing in the controller reads Events today. ADR-083 dropped its Event read in review because the pod condition carried the same text without it; here the condition does not.

## Decision

Add a second detector on the Job tier, next to the ADR-083 scheduling-stall detector, that reports workload pods which were placed but never became Ready. It reuses ADR-083's grace field, Workflow reason and report line, and it keeps `timeoutPerJob` running.

1. **Detection: any placed pod with `ContainersReady=False`.** The detector runs on the running path, on the same `podread.JobPods` list `checkSchedulingBlocked` already read. A pod counts as not Ready when all of these hold:
   - `spec.nodeName` is set;
   - its phase is `Pending` or `Running`, so a `Succeeded` or `Failed` pod, whose `ContainersReady` is also `False`, never counts;
   - it has no `deletionTimestamp`;
   - it carries `ContainersReady=False`.

   The state fires when at least one pod matches and the state has lasted past the grace window of decision 4. There is no launcher gate. The list is worker-only, so a launcher gate read from it is always open, and the gate is not what keeps the detector from firing mid-run anyway: a healthy worker is Ready while the workload runs. A worker that turns NotReady mid-run and stays that way past grace is reported, which is the truth. Widening from `Running` to `Pending or Running` costs nothing extra (it is the same list, and the `Pending` message is on the pod), and it closes the `ImagePullBackOff` and wedged-init gap above, so it is in scope.

2. **Precedence: ADR-083 first.** The not-ready check runs only after `checkSchedulingBlocked` returns not blocked and no scheduling-blocked episode is open. A workload with both unscheduled and placed-but-unready pods reports `WorkloadSchedulingBlocked`, and its clock stays paused, as ADR-083 decides.

3. **The message comes from the `Unhealthy` Event; the pod is the fallback.** The detector picks one pod, the first not-Ready pod by name so the choice is stable across reconciles, and relays one line for it:
   - **`Running` pod:** the newest `Unhealthy` Event for that pod. If there is none (Events expire, and a container in `CrashLoopBackOff` may never be probed), the first container's `state.waiting` reason and message, then the `ContainersReady` condition message as the last resort.
   - **`Pending` pod:** the first waiting init container's `state.waiting`, then the first waiting container's, as `<container>: <reason>: <message>`. No Event read.

   The order is the reverse of ADR-083's, for the reason ADR-083 gives: there kube-scheduler writes its own diagnosis into `PodScheduled`, so the condition is the better source; here the kubelet writes only the container name into `ContainersReady`, and the probe output is in the Event.

   **The Event read is live, once per episode, and scoped to one pod.** It goes through `r.APIReader` as a `corev1.EventList` in the Job's namespace with field selectors `involvedObject.name=<pod>` and `reason=Unhealthy`, and keeps only Events whose `involvedObject.uid` is the pod's UID, so an Event left by a deleted pod of the same name is never relayed. The newest wins by `series.lastObservedTime`, then `lastTimestamp`, then `eventTime`. It is read only on the reconcile that is about to raise the reason; later reconciles in the same episode keep the condition message they already wrote. It must not be a cached read: a cached Event read starts a cluster-wide informer that needs `list` and `watch` in every namespace, which loops on forbidden under `rbac.scope: namespaced` (issue #424), and it is why `ClientOptions` (`pkg/controller/cache.go:327`) keeps ConfigMaps and PVCs out of the cache. A failed read is logged and falls through to the pod's own fields, so a missing permission degrades the message, never the detection.

   The relayed text is capped at 1 KiB, cut on a UTF-8 boundary with a trailing `...`, because the Workflow appends it to its own condition message and the report prints it. The Job condition message is `Workload pods are not Ready: pod <pod>: <text>`, and adds `(<n> of <m> pods not Ready)` when more than one pod matches.

4. **Grace: reuse `schedulingStallGraceSeconds`.** It sits on `JobSpec` (`api/v1alpha1/job_types.go:297`), and a catalog entry's `jobTemplate.spec` is a `JobSpec`, so a category whose image installs `sshd` at start can widen it without a new knob. The default stays 5 minutes (`defaultSchedulingStallGrace`), which outlasts an `apt-get install openssh-server` on a reasonable mirror. The field's doc changes from "unschedulable" to "blocked: unschedulable, or placed but not Ready", and it says the two cases differ in clock accounting (decision 5).

5. **`timeoutPerJob` keeps running, and the start time is stamped in the same write.** Unlike a scheduling block, the nodes are held and the fault is inside the workload, so the time counts. But counting has to be enforced at the write, not inherited. The ADR-083 path returns at `job_controller.go:867`, before the status write that stamps `workloadStartTime` at `:911`. A not-ready path with the same shape would leave a Job that is not Ready on its first running-path observation with a `WorkloadRef` and no start time, which `isJobTimedOut` (`pkg/controller/workflow_controller.go:1266`) reads as not started and never times out. "Count it" would become "never terminates". So there is one write: the running branch picks the reason, `WorkloadPodsNotReady` past grace and `WorkloadRunning` otherwise, and passes it to the same `setJobInProgress` call that stamps `workloadStartTime` and sets or clears the episode marker. `isJobTimedOut` then needs no change, and a unit case pins that a Job whose first observation is already not Ready gets its start time in that write. Stall detection runs exactly as it does now, before the write.

6. **Restart safety: `status.workloadPodsNotReadySince`.** The first observation of an episode records it, in the same write as decision 5. A successful list that finds no not-Ready pod clears it and returns the reason to `WorkloadRunning`; a failed list changes nothing, so a transient error cannot reset grace. A checkpoint restart clears it with `workloadStartTime`. A terminal Job keeps it, as it keeps `schedulingBlockedSince`, so the Workflow's timeout path can say why: a Job that times out with the marker set fails with `Job exceeded timeoutPerJob while its pods were not Ready`. Unlike the scheduling-blocked case, its nodes are still attributed in `failedNodes`, since the pods were placed and held them (ADR-099).

7. **Surfacing reuses ADR-083's Workflow reason, so the report does not change.**
   - **Job:** `InProgress=True(WorkloadPodsNotReady)` with the message of decision 3, and one Warning Event `WorkloadPodsNotReady` on the Job when the reason is entered. A reason change inside `InProgress` raises no transition Event on its own (`setExclusiveStatusConditionUnless` reports type changes only), so the detector emits it.
   - **Workflow:** no new reason. `schedulingBlockedMessage` (`workflow_controller.go:1303`) gains a case returning `job <name> pods not ready: <Job message>`, and the running-group loop already raises `ReasonJobSchedulingBlocked` with that text. That reason already relays a cause that is not scheduling (`WorkloadCreationRejected`, issue #488).
   - **Report:** `runningStatusDetail` prints the Workflow message only for reasons in `blockedReasons` (`pkg/report/report.go:560`), and `JobSchedulingBlocked` is already there. The `Blocked:` line shows the new text with no report change. A new Workflow reason would have needed one.

8. **The reason is `WorkloadPodsNotReady`.** The detector observes any job-labelled pod that was placed and did not become Ready, not an MPI role, so `WorkloadWorkersNotReady` would name the wrong thing.

## Implementation

- **`api/v1alpha1/job_types.go`**: `status.workloadPodsNotReadySince` (`*metav1.Time`, optional), and the widened doc on `spec.schedulingStallGraceSeconds`. `make manifests` regenerates the CRD.
- **`pkg/controller/helpers.go`**: `ReasonWorkloadPodsNotReady` on the Job tier, documented as counting against `timeoutPerJob`, unlike `ReasonWorkloadSchedulingBlocked`.
- **`pkg/controller/job_controller.go`**
  - `checkPodsNotReady(ctx, job, pods)` takes the list `checkSchedulingBlocked` read, so a reconcile still makes one cached pod list. `checkSchedulingBlocked` returns that list alongside its result. The function applies decision 1 and returns the not-Ready pods; it writes nothing.
  - `podsNotReadyMessage(pod, events)`, a pure function that applies decision 3's order and the 1 KiB cap. The Event read sits in a small `unhealthyEvents(ctx, pod)` helper over `r.APIReader`.
  - In the `default` (running) branch, after the stall check: the final `setJobInProgress` takes its reason and message from the not-ready result, and one extra mutation sets or clears `workloadPodsNotReadySince` next to the existing `workloadStartTime` stamp. The Warning Event is emitted when the reason moves to `WorkloadPodsNotReady`.
  - `restartFromCheckpoint` clears the marker with `workloadStartTime`.
  - `+kubebuilder:rbac:groups="",resources=events,verbs=get;list`, for the record only.
- **`helm/cluster-readiness-engine/templates/run-role.yaml`**: a rule granting `get` and `list` on `events` in the core group, next to the `pods/log` read. Added by hand: `make manifests` writes no RBAC (ADR-092). ADR-092 put the controller's other read of workload content, pod logs, in `nvcre-run-role`, so this grant follows `rbac.scope` the same way: cluster-wide under `cluster`, and only in bound namespaces under `namespaced`. The Job is created in its Workflow's namespace, which that binding already covers. `nvcre-manager-role` is unchanged: it is bound cluster-wide under both scopes, and binding it per namespace would break its cluster-scoped grants. The namespace access check (`pkg/controller/namespace_access.go`) does not gain the verb, because the read is best effort (decision 3): a narrower custom Role without it degrades the message and does not hold the Workflow. The helm RBAC goldens under `test/helm/` and ADR-092's role table are updated with it.
- **`pkg/controller/workflow_controller.go`**: the `ReasonWorkloadPodsNotReady` case in `schedulingBlockedMessage`; the not-Ready timeout message in the timeout path. `isJobTimedOut` is unchanged.
- **`cmd/integration/integration_test.go`**: normalize `workloadPodsNotReadySince` to the fixed placeholder, the way `schedulingBlockedSince` is at line 1727, or every golden that carries it flakes.
- **Docs**: `docs/api-reference/job.md`, `docs/operations/troubleshooting.md` and `docs/concepts/health-monitoring-remediation.md` gain `WorkloadPodsNotReady` where they describe `WorkloadSchedulingBlocked`; ADR-093's Consequences point here; `CHANGELOG.md` gets an `[Unreleased]` entry.
- **API cost:** no new cached read. One live, namespaced, field-selected Event list per not-Ready episode, only for a `Running` pod.

### Testing plan

No GPUs are needed.

- **Message projection, as `testutil` goldens** under `pkg/controller/testdata/pods-not-ready-message/`, the shape of `not-enough-nodes-message` and `gpu-arch-fallback-message`. Cases: a `Running` pod with an `Unhealthy` Event; several Events, newest wins; an Event whose UID belongs to a deleted pod of the same name, ignored; no Event, falling back to `state.waiting` and then to the condition; a `Pending` pod in `ImagePullBackOff`; a wedged init container; several not-Ready pods; text over 1 KiB, cut on a rune boundary.
- **Tables for the nil-safety and classification pins**, next to `scheduling_stall_test.go`: no conditions, no container statuses, a `Succeeded` pod, a terminating pod, an unplaced pod (ADR-083's), a Ready pod.
- **Reconcile cases**: inside grace (marker set, reason `WorkloadRunning`) and past grace (reason `WorkloadPodsNotReady`, one Warning Event); recovery clearing the marker; a scheduling block taking precedence; a failed pod list leaving the marker alone; a failed Event read falling back to the pod. **The decision 5 pin:** a Job whose first running-path observation is already not Ready has `workloadStartTime` set in the same write as the reason, and `isJobTimedOut` times it out once `timeoutPerJob` passes. The Event read is pinned to `APIReader` by putting the Event only in the live fake client.
- **Workflow**: `schedulingBlockedMessage` returns `job <name> pods not ready: ...` for the new reason, and a timed-out Job with the marker set carries the not-Ready timeout message and its nodes in `failedNodes`.
- **Integration**, under `cmd/integration/testdata/reconcile/`: `job-pods-not-ready`, with job-labelled worker pods that are placed, `Running` and `ContainersReady=False`, an `Unhealthy` Event for one of them, and no launcher; and `job-pods-not-ready-image-pull`, with a placed `Pending` worker in `ImagePullBackOff`. As ADR-102 requires, any launcher pod in a fixture carries no job label. The harness's refused-read check must stay clean, which also shows the Event read did not go through the cache.

## Rationale

- **Why the Event, when ADR-083 rejected Events?** ADR-083 rejected them because the pod condition held the same diagnosis without a read. Here it does not: the only place the probe output exists is the `Unhealthy` Event. The costs ADR-083 named are each answered: the read is live and namespaced (no informer, no #424 loop), one per episode (not per reconcile), and filtered by UID (no relay from a deleted pod of the same name).
- **Why widen detection?** The widened predicate is the same loop over the same list, and the `Pending` message is on the pod, so it adds no read. Leaving it out would leave `ImagePullBackOff` and wedged init containers waiting out `timeoutPerJob` as `WorkloadRunning`, and nothing else catches them.
- **Why reuse `ReasonJobSchedulingBlocked`?** It keeps the report change at zero, and the reason already relays a cause that is not scheduling. Its Go name is narrower than its use, which is a naming cost; a rename touches the API's reason vocabulary and is not worth doing here.
- **Why count the time?** The resources are held and the fault is inside the workload. Pausing the clock would let a pod that can never become Ready hold GPUs for longer than the operator's budget. This state makes the wait visible; it does not change how long it may last.
- **Why one write?** Two writes on the running path is the bug decision 5 describes. One write cannot stamp the reason without the start time.

## Consequences

- A worker whose `sshd` refuses sessions, or any placed pod that never becomes Ready, shows as `InProgress=True(WorkloadPodsNotReady)` with the probe's or the kubelet's own text on the Job, as `JobSchedulingBlocked` with `job <name> pods not ready: ...` on the Workflow, and on the report's `Blocked:` line, within one reconcile of grace elapsing, instead of never.
- `timeoutPerJob` is unchanged in effect: the Job still times out when it would have, now with a message that says why.
- The controller reads Events for the first time, and `nvcre-run-role` gains `get` and `list` on core `events`. Under `rbac.scope: cluster` that exposes Event contents in every namespace to the controller's service account, as pod logs already are; under `namespaced`, only in the namespaces it is bound in. The read itself is confined to the Job's namespace and one pod.
- A new optional status field, `workloadPodsNotReadySince`. No new spec field, no new Workflow reason, no report change.
- `schedulingStallGraceSeconds` now also delays this condition. A site that set it high to tolerate slow scheduling also waits longer to see a not-Ready pod reported.
- Adapters are unchanged.

## Alternatives Considered

- **Keep the launcher-absent gate.** Rejected. Read from the cached list it is always true; read correctly it needs a live `podread.WorkloadPods(ctx, ns, workload, "launcher")` on every running reconcile, to guard against a case the Ready check already rules out.
- **The `ContainersReady` condition as the primary message.** Rejected: for a probe failure it names the container and nothing else.
- **A cached Event read.** Rejected: cluster-wide informer, `list` and `watch` in every namespace, and the #424 forbidden loop under `rbac.scope: namespaced`.
- **A new Workflow reason.** Rejected: invisible in the report until it is added to `blockedReasons`, for no gain over a `schedulingBlockedMessage` case.
- **A separate `workersNotReadyGraceSeconds`.** Not taken. The existing field is on `JobSpec` and already reachable from the catalog. It can be split later if a site needs the two windows apart.
- **Pause `timeoutPerJob` while not Ready.** Rejected; see Rationale.
- **`Running` pods only.** Rejected once widening proved to cost nothing; see decision 1.

## Notes

- The probe itself is out of scope; it is ADR-093's.
- The `--wait` stream still shows only category status, the gap ADR-083 decision 7 already tracks.
- A pod that keeps restarting and passes its probe between crashes can flip in and out of the state. Grace filters the flapping, and the restart count is already in the pod. A restart-count detector would be its own change.

## References

- Issue [#469](https://github.com/NVIDIA/cluster-readiness-engine/issues/469)
- [ADR-083](083-scheduling-stall-visibility.md): the scheduling-stall detector, grace field, Workflow relay and `Blocked:` line this record reuses
- [ADR-092](092-per-namespace-controller-rbac.md): the manager and run role split, `rbac.scope`, and RBAC maintained by hand
- [ADR-093](093-mpi-worker-sshd-capability-and-session-probe.md): the probe whose failure this relays
- [ADR-099](099-failed-node-attribution-relay.md): failed-node attribution on timeout
- [ADR-102](102-scoped-pod-cache.md): `podread`, and why launchers are not in the cached list
- Issue #424: cached reads that need cluster-wide `list` and `watch`
- Issue #488: `WorkloadCreationRejected`, the precedent for relaying a non-scheduling cause through `ReasonJobSchedulingBlocked`
- `pkg/controller/job_controller.go`: `checkSchedulingBlocked`, the running branch, `setJobInProgress`
- `pkg/controller/workflow_controller.go`: `isJobTimedOut`, `schedulingBlockedMessage`
- `pkg/report/report.go`: `blockedReasons`, `runningStatusDetail`
