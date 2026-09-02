# ADR-053: Ordered Dependency Deletion via Reverse Topological Sort

> **Status:** Proposed

## Context

During certification teardown on GB200/GB300, CUDA failure 719 ("unspecified launch failure") occurs. The error manifests in `include/alloc.h:250` as `NCCL WARN Cuda failure 719 'unspecified launch failure'`. The same workload runs without errors in isolation (manually applied YAMLs).

The root cause is a race condition in the Workflow controller's deletion logic. When a Certification is deleted (tearing down all Workflows), the Workflow controller's `handleDeletion()` deletes dependency resources — including ComputeDomain and ResourceClaimTemplate — **before** deleting Jobs and their workloads. For GB200/GB300 workloads using DRA (Dynamic Resource Allocation) for NVLink/NVSwitch multi-node communication, deleting the ComputeDomain revokes the DRA allocation while CUDA kernels are still executing on the GPU interconnect.

Four deletion paths exhibit this bug:

1. **`handleDeletion()`** — Dependencies deleted at line 1360, Jobs deleted at line 1382. Triggered during forced Workflow deletion.
2. **`setFinalStatus()`** — `cleanupDependencies()` called before `deleteTerminalJobs()`. Triggered on normal Workflow completion.
3. **`handleIterationComplete()`** — `cleanupScopedDependencies()` called before Job deletion. Triggered between iterations.
4. **`updateStatusFromJobs()` retry** — `cleanupScopedDependencies()` called before Job deletion. Triggered on retry.

Additionally, dependency deletion uses creation (topological) order instead of reverse topological order, so if dependency A references dependency B, B is deleted first even though it should outlive A.

ADR-034 established that dependencies are created in topological order (via `orderDependencies()`) and stored in `workflow.Status.DependencyRefs` in that order. The same graph can be leveraged for deletion by reversing the order.

## Decision

Fix all four deletion paths to enforce two invariants:

1. **Jobs/workloads are deleted before dependencies.** In `handleDeletion()`, use a phased requeue pattern: delete Jobs in Phase 1, wait for them to be confirmed gone in Phase 2, then delete dependencies in Phase 3. In other paths, reorder calls so Jobs are deleted before dependency cleanup.

2. **Dependencies are deleted in reverse topological order.** Add a `reverseDependencyRefs()` utility that reverses `DependencyRefs` (which are already in creation/topological order). Use it in both `cleanupDependencies()` and `cleanupScopedDependencies()`.

## Implementation

### `reverseDependencyRefs()` — `pkg/controller/workflow_deps.go`

New utility function that returns `DependencyResourceRef` slice in reverse order. Since refs are stored in creation (topological) order by `orderDependencies()`, reversing gives a safe deletion order: resources that depend on others are deleted first, followed by the resources they depend on.

### `handleDeletion()` — `pkg/controller/workflow_controller.go`

Replace the current flat deletion with a phased approach:

- **Phase 1:** Delete all owned Jobs by label selector.
- **Phase 2:** Re-list Jobs. If any still exist (Job controller finalizer not yet processed), return `RequeueAfter`. This ensures the Job controller has time to delete the workload (TrainJob) and wait for pod termination before dependencies are removed.
- **Phase 3:** Delete all dependency resources in reverse topological order via `reverseDependencyRefs()`.
- **Phase 4:** PV cleanup (unchanged).
- **Phase 5:** Remove finalizer (unchanged).

The requeue-wait pattern is already established in this function for PV cleanup.

### `setFinalStatus()` — `pkg/controller/workflow_controller.go`

Hoist `deleteTerminalJobs()` to the top. Remove duplicate calls from the success/failure branches. Call `cleanupDependencies()` after `deleteTerminalJobs()`.

### `handleIterationComplete()` — `pkg/controller/workflow_controller.go`

Swap order: delete Jobs before calling `cleanupScopedDependencies()`.

### `updateStatusFromJobs()` retry path — `pkg/controller/workflow_controller.go`

Swap order: delete the failed Job before calling `cleanupScopedDependencies()`.

### `cleanupDependencies()` — `pkg/controller/workflow_controller.go`

Iterate `reverseDependencyRefs(workflow.Status.DependencyRefs)` instead of the forward list.

### `cleanupScopedDependencies()` — `pkg/controller/workflow_deps.go`

Collect matching refs, reverse them, then delete. Preserve PVC special-case logic (PV reclaim policy patching).

## Rationale

1. **DRA allocations are tied to ComputeDomain lifecycle.** When a ComputeDomain is deleted, the DRA driver revokes NVLink/NVSwitch channel allocations. Active CUDA operations on those channels fault with error 719. The fix ensures ComputeDomain outlives all workloads that reference it.

2. **Reverse topological order is the natural deletion order.** If dependency A was created before B (because B references A), then B should be deleted before A. This is the reverse of creation order, which we already compute and store.

3. **Phased requeue is idempotent and crash-safe.** If the controller restarts between phases, it re-enters `handleDeletion()`, re-lists Jobs, and either waits or proceeds. No state is lost because the phase is determined by the presence/absence of Jobs in the API server.

4. **No foreground deletion propagation.** The codebase consistently uses default (background) deletion. The phased requeue achieves the same ordering guarantee without introducing a new deletion pattern.

## Consequences

### Positive
- Eliminates CUDA failure 719 during certification teardown for DRA-backed workloads (GB200/GB300).
- Dependencies that reference other dependencies are deleted in the correct relative order.
- The phased requeue pattern makes the deletion cascade explicit and inspectable via logs.

### Negative
- Workflow deletion takes slightly longer due to the requeue-wait cycle (one additional reconcile interval, typically 1s in tests, 15s in production).
- Workflow finalizer removal is delayed until Jobs are confirmed gone, which may briefly delay Kubernetes garbage collection of the Workflow object.

## Alternatives Considered

### Foreground deletion propagation on Jobs
Set `PropagationPolicy: Foreground` when deleting Jobs so Kubernetes blocks until all dependents (workloads, pods) are gone. **Rejected:** Not used anywhere in the codebase. Introduces a new deletion pattern and depends on the GC controller, which is absent in envtest.

### Grace period on dependency deletion
Add `GracePeriodSeconds` to dependency `Delete()` calls. **Rejected:** Does not guarantee pod termination completes within the grace period. A time-based approach is fundamentally less reliable than the state-based requeue-wait pattern.

### Defer all dependency cleanup to `handleDeletion()`
Remove `cleanupDependencies()` from `setFinalStatus()` entirely. **Rejected:** Dependencies would linger until the Workflow is deleted by the Certification controller, wasting cluster resources during the gap between Workflow completion and Certification deletion.

## Addendum: pod-drain barrier (issue #121)

Ordering alone proved insufficient. Deleting a workload (TrainJob) is
asynchronous: the object disappears immediately while its pods keep running
(Terminating) for their termination grace period. Every path that deleted the
workload and then called `cleanupScopedDependencies()` in the same reconcile —
the timeout, retry, and terminal-failure paths in `updateStatusFromJobs()` —
still deleted the ComputeDomain under live pods, and the Job finalizer
(`JobReconciler.handleDeletion()`) removed itself right after issuing the
workload delete, so `handleDeletion()`'s Phase 1→2 wait on Job objects was not
actually a barrier for pods.

The fix gates every DRA-revoking step on the workload's pods being gone
(`shouldWaitForPodDrain()` in `pkg/controller/pod_drain.go`):

- The Workflow tier defers scoped-dependency cleanup — keeping the group
  Running and requeueing at the normal interval — until no pod carrying the
  Job's `nvcre.nvidia.com/job` label remains in a non-terminal phase.
- The Job finalizer stays registered until the same condition holds, so
  "no Job objects remain" (the Phase 1→2 signal in the Workflow's
  `handleDeletion()`, and the Job-NotFound group handling) truthfully implies
  "no workload pods remain".
- The wait is bounded by `podDrainGracePeriod` (5 minutes), measured from
  persisted timestamps only (the Job's terminal-condition transition times
  and its `deletionTimestamp`), so a pod stuck Terminating cannot wedge a
  Workflow forever; after the grace period cleanup proceeds with a logged
  warning.

## References

- [ADR-034](034-inferred-dependency-lifecycle.md) — Inferred dependency scope and topological ordering
- ADR-011 (internal; predates the public repository) — Original dependency lifecycle design (superseded by ADR-034)
- `pkg/controller/workflow_deps.go` — `orderDependencies()`, `classifyDependencies()`, `cleanupScopedDependencies()`
- `pkg/controller/workflow_controller.go` — `handleDeletion()`, `setFinalStatus()`, `cleanupDependencies()`
- `pkg/controller/job_controller.go` — `handleDeletion()` (Job-level workload cleanup)

## Change Log

| Date | Change |
|------|--------|
| 2026-02-25 | Initial proposal |
| 2026-08-31 | Addendum: pod-drain barrier gating dependency cleanup and Job finalizer removal (issue #121) |
