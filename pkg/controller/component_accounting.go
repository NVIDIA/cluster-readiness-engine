// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
)

// componentSealTerminal reports whether a Job has finished accounting for its
// per-component rows, one way or another.
//
// The spec branch is what makes this safe to gate a wait on. A Job that
// declares no component results never writes a seal at all, so an absent seal
// is ambiguous on its own — it means either "owed nothing" or "owes rows and
// has not produced them yet" — and only the spec tells the two apart. Reading
// it here is why every Job in the repository today is already terminal by this
// measure, and why the wait cannot stall an existing Workflow.
func componentSealTerminal(job *nvcrev1alpha1.Job) bool {
	seal := job.Status.ComponentResults
	if seal == nil {
		return job.Spec.ComponentResults == nil
	}
	return componentResultsSealed(seal.Phase)
}

// observeComponentSeal copies a terminal Job's seal onto its group.
//
// Assigned by attempt, never accumulated. completeTerminalGroup is re-entered:
// it returns early while pods drain and again on a stale view, and it is
// reached afresh after any failed status write. A running sum would count
// those passes twice, which is why the Workflow total is recomputed from these
// entries rather than incremented.
//
// Returns whether anything changed, so the caller can skip a status write.
func observeComponentSeal(g *nvcrev1alpha1.GroupStatus, job *nvcrev1alpha1.Job, attempt int) bool {
	seal := job.Status.ComponentResults
	if seal == nil || seal.Phase == nvcrev1alpha1.ComponentResultsNone {
		return false
	}

	var truncated int64
	for _, t := range seal.Truncations {
		truncated += t.Count
	}
	entry := nvcrev1alpha1.GroupComponentSeal{
		Attempt:           int32(attempt), //nolint:gosec // bounded by retryFailedGroups
		JobName:           job.Name,
		ShardCount:        seal.ShardCount,
		RowCount:          seal.RowCount,
		Phase:             seal.Phase,
		TruncatedRowCount: truncated,
		// Carried, not recomputed: the Job derived this from rows the Workflow
		// never sees, and resolving it here would mean reading every shard.
		DegradedComponentCount: seal.DegradedComponentCount,
		DegradedComponents:     seal.DegradedComponents,
	}

	for i := range g.ComponentSeals {
		if g.ComponentSeals[i].Attempt == entry.Attempt {
			// DeepEqual, not ==: the seal now carries a slice.
			if equality.Semantic.DeepEqual(g.ComponentSeals[i], entry) {
				return false
			}
			g.ComponentSeals[i] = entry
			return true
		}
	}
	g.ComponentSeals = append(g.ComponentSeals, entry)
	return true
}

// recomputeComponentResults derives the Workflow's totals from its own status.
//
// Recomputed, never incremented, and that is the whole point. The function
// that observes a terminal group is re-entered on a pod-drain wait, on a stale
// view, and after any failed status write, so a `+=` counter double-counts.
// recordFailedNodes survives the same re-entry only because it is a set union.
// A pure function of status converges across re-entry, conflict retries and
// controller restarts alike.
//
// Three sources are summed, and missing any one of them loses rows:
//
//   - the live groups' seals for the attempt each group is on,
//   - the snapshotted iterations in IterationHistory,
//   - retiredComponentResults, which is where fault isolation's folded totals
//     go because it replaces the group list outright and never snapshots.
func recomputeComponentResults(
	status *nvcrev1alpha1.WorkflowStatus, workflowUID string, final bool,
) bool {
	orch := status.Orchestration
	if orch == nil {
		return false
	}

	totals := nvcrev1alpha1.ComponentResultTotals{}
	if retired := orch.RetiredComponentResults; retired != nil {
		totals = *retired
	}

	for _, it := range orch.IterationHistory {
		for _, g := range it.Groups {
			if g.ComponentSeal != nil {
				addSealToTotals(&totals, *g.ComponentSeal)
			}
		}
	}
	for _, g := range orch.Groups {
		// The live attempt only. A superseded attempt's seal is still present
		// until retirement drops it, and counting both would double its rows.
		if seal := liveSeal(g); seal != nil {
			addSealToTotals(&totals, *seal)
		}
	}

	// Published only when there is something to say: any shard, any row, or
	// any row dropped. Absent is the answer otherwise, and that is the same
	// rule the Job tier applies to its own seal.
	//
	// Keying this on final instead would publish a zero block with a live
	// selector on every Workflow that reaches a terminal condition, which is
	// every Workflow in the system, to report that a feature none of them uses
	// produced nothing. A reader cannot tell that block from a run that
	// genuinely finished with an empty result, so it is worse than absent: it
	// invites a bundle for a run that never had a producer.
	//
	// Truncation belongs in the test because it is the one case where a zero
	// count is the finding rather than the absence of one. A run that dropped
	// every row it produced has no shards and no rows, and suppressing it
	// would hide exactly the evidence this mechanism exists to surface.
	if totals.ShardCount == 0 && totals.RowCount == 0 &&
		totals.TruncatedRowCount == 0 && totals.DegradedComponentCount == 0 {
		return false
	}

	next := &nvcrev1alpha1.WorkflowComponentResults{
		ShardCount:             totals.ShardCount,
		RowCount:               totals.RowCount,
		Selector:               componentResultsSelector(workflowUID),
		Final:                  final,
		Truncated:              totals.TruncatedRowCount > 0,
		TruncatedRowCount:      totals.TruncatedRowCount,
		DegradedComponentCount: totals.DegradedComponentCount,
		DegradedComponents:     totals.DegradedComponents,
	}
	// equality.Semantic, not ==: the struct now carries a slice of degraded
	// components, so == no longer compiles. The comparison is load bearing
	// rather than an optimisation, because recompute runs on every pass and a
	// status write that changes nothing still costs an API call and a
	// resourceVersion bump that every watcher sees.
	if existing := status.ComponentResults; existing != nil &&
		equality.Semantic.DeepEqual(existing, next) {
		return false
	}
	status.ComponentResults = next
	return true
}

// liveSeal returns the seal for the attempt a group is currently on.
func liveSeal(g nvcrev1alpha1.GroupStatus) *nvcrev1alpha1.GroupComponentSeal {
	for i := range g.ComponentSeals {
		if int(g.ComponentSeals[i].Attempt) == g.Retries {
			return &g.ComponentSeals[i]
		}
	}
	return nil
}

func addSealToTotals(t *nvcrev1alpha1.ComponentResultTotals, s nvcrev1alpha1.GroupComponentSeal) {
	t.ShardCount += s.ShardCount
	t.RowCount += s.RowCount
	t.TruncatedRowCount += s.TruncatedRowCount
	t.DegradedComponentCount += s.DegradedComponentCount
	t.DegradedComponents = mergeDegraded(t.DegradedComponents, s.DegradedComponents)
}

// foldRetiredComponentResults folds a group list's seals into the retired
// totals before that list is discarded.
//
// Fault isolation is the only caller, and it needs one: diagnoseSetGroups
// rebuilds orch.Groups from scratch every round and never calls
// snapshotIteration, so without this the rows a round produced would vanish
// from the Workflow total the moment the next round's groups were installed.
// Fault isolation is also the mode where per-component rows are most worth
// having, so losing them there is the worst place to lose them.
//
// Folded in the same in-memory pass that replaces the groups, so the two are
// persisted by one status write: if that write fails neither happened, and the
// next pass recomputes from the same base.
func foldRetiredComponentResults(orch *nvcrev1alpha1.OrchestrationStatus) {
	var folded nvcrev1alpha1.ComponentResultTotals
	if orch.RetiredComponentResults != nil {
		folded = *orch.RetiredComponentResults
	}
	var any bool
	for _, g := range orch.Groups {
		if seal := liveSeal(g); seal != nil {
			addSealToTotals(&folded, *seal)
			any = true
		}
	}
	if !any && orch.RetiredComponentResults == nil {
		return
	}
	orch.RetiredComponentResults = &folded
}

// retireComponentSeals deletes a superseded attempt's shards and drops its
// seal from the group.
//
// This is what bounds GroupStatus.ComponentSeals. retryFailedGroups has no
// maximum, so keeping every attempt's seal would grow the list without bound
// and the API server would refuse the status write past MaxItems. Retirement
// already enumerates earlier attempts from durable state and already blocks
// the next launch until every one is confirmed gone, so retries cannot advance
// past an unretired seal and at most two are ever held.
//
// Deleting the shards with the seal is also what keeps the Workflow total
// monotonic. Dropping a seal while its shards survived would make the total
// fall at an iteration boundary, and a reader cannot tell a total that fell
// from one that was never complete.
//
// Best effort on the delete, like retirePreviousMeasurements: a leaked shard
// is outside the attempt the selector walk will accept, because nvcrectl
// filters on the sealed attempt's Job name. A failed delete must not hold up
// the next launch.
func (r *WorkflowReconciler) retireComponentSeals(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, g *nvcrev1alpha1.GroupStatus,
) {
	if len(g.ComponentSeals) == 0 {
		return
	}
	log := logf.FromContext(ctx)

	kept := g.ComponentSeals[:0]
	for _, seal := range g.ComponentSeals {
		if int(seal.Attempt) >= g.Retries {
			kept = append(kept, seal)
			continue
		}
		stem := shardNameStem(string(workflow.UID), seal.JobName)
		if err := deleteComponentShards(ctx, r.Client, workflow.Status.Namespace, stem, seal.ShardCount); err != nil {
			log.V(1).Info("Could not delete a superseded attempt's shards",
				"group", g.Name, "attempt", seal.Attempt, "error", err)
		}
	}
	g.ComponentSeals = slices.Clone(kept)
}

// deleteWorkflowComponentShards removes every shard a Workflow owns.
//
// Needed because envtest has no garbage collector, so the owner reference that
// cleans these up on a real cluster does nothing in tests, and a test cannot
// assert cleanup it never sees. Names are derived from the same durable seals
// the totals are computed from, so no list permission is required.
func (r *WorkflowReconciler) deleteWorkflowComponentShards(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
) error {
	orch := workflow.Status.Orchestration
	if orch == nil {
		return nil
	}
	namespace := workflow.Status.Namespace
	if namespace == "" {
		namespace = workflow.Namespace
	}

	var errs []error
	del := func(seal nvcrev1alpha1.GroupComponentSeal) {
		stem := shardNameStem(string(workflow.UID), seal.JobName)
		if err := deleteComponentShards(ctx, r.Client, namespace, stem, seal.ShardCount); err != nil {
			errs = append(errs, err)
		}
	}
	for _, g := range orch.Groups {
		for _, seal := range g.ComponentSeals {
			del(seal)
		}
	}
	for _, it := range orch.IterationHistory {
		for _, g := range it.Groups {
			if g.ComponentSeal != nil {
				del(*g.ComponentSeal)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete component result shards: %w", errs[0])
	}
	return nil
}

// componentSealGracePeriod bounds a group's wait for its Job's seal.
//
// The wait holds back the delete that frees the group's GPUs, so the bound is
// what keeps a stuck Job tier from wedging a cluster: past it the group
// finalizes with whatever was sealed, and the Workflow total is short rather
// than the nodes being held. The Job tier retries a failed shard write every
// componentResultsRequeue, so this allows roughly a dozen attempts, and the
// ordinary case is satisfied by the next reconcile.
//
// Measured from drainStart, the same persisted anchor the pod-drain barrier
// uses, so the bound survives a controller restart instead of resetting.
const componentSealGracePeriod = 2 * time.Minute

// shouldWaitForComponentSeal reports whether a group must stay Running until
// the Job tier has accounted for its per-component rows.
//
// Two things break without the wait, and they pull the same way.
//
// The Workflow total is computed from the seals observed on its groups. The
// two controllers are independent, so the Workflow can reach a terminal group
// on the same status write that made the Job terminal, before the Job tier has
// produced a single row. Finalizing there records no seal and the group is
// never revisited, so the total is permanently short of shards that do exist
// and nvcrectl refuses the bundle (ADR-090). The retry path is worse than
// short: the attempt's shards are written and labelled with the Workflow UID,
// but no seal names them, so retireComponentSeals cannot delete them and the
// selector resolves more shards than the total admits.
//
// And on a failure the caller's very next act is deleteWorkloadForJob, which
// cascade-deletes the pods whose logs are the only source those rows have.
// Freeing the GPUs before the Job tier has read them does not make the rows
// late, it makes them unrecoverable. captureTimeoutLog guards the same hazard
// one tier up, for the same reason.
//
// A Job that declares no component results needs no wait and gets none, which
// is every Job in the repository today.
func shouldWaitForComponentSeal(ctx context.Context, job *nvcrev1alpha1.Job) bool {
	if componentSealTerminal(job) {
		return false
	}
	if start := drainStart(job); start != nil && time.Since(start.Time) > componentSealGracePeriod {
		logf.FromContext(ctx).Info(
			"Warning: component results were not sealed within the grace period;"+
				" finalizing the group with a short result total",
			"job", job.Name, "gracePeriod", componentSealGracePeriod, "phase", componentSealPhase(job))
		return false
	}
	return true
}

// componentSealPhase returns a Job's seal phase for logging, or the empty
// phase when it has not sealed at all.
func componentSealPhase(job *nvcrev1alpha1.Job) nvcrev1alpha1.ComponentResultPhase {
	if job.Status.ComponentResults == nil {
		return ""
	}
	return job.Status.ComponentResults.Phase
}

// Assert the reason constants stay inside the CRD enum. pkg/shard's four
// reasons plus the controller's are a subset of ComponentTruncation's eight,
// and a mismatch costs the whole status write rather than one field, because
// the seal is written before the first shard.
var _ = []string{
	shard.ReasonRowTooLarge,
	shard.ReasonRowBudgetExceeded,
	shard.ReasonRowUnencodable,
	shard.ReasonShardLimitExceeded,
	reasonLogUnpageable,
	reasonProducerUnavailable,
}

// workflowStatusTerminal reports whether a Workflow has reached a terminal
// condition, reading status alone.
//
// A free function on status rather than the reconciler's isTerminal method,
// because the recompute runs inside a status-mutation closure that has the
// object but not the receiver. Final is what tells a reader the total can be
// checked: shards are produced per Job and Jobs finish at different times, so
// a total alone cannot distinguish "complete so far" from "missing".
func workflowStatusTerminal(status *nvcrev1alpha1.WorkflowStatus) bool {
	for _, cond := range status.Conditions {
		if cond.Status != metav1.ConditionTrue {
			continue
		}
		if cond.Type == nvcrev1alpha1.WorkflowSucceeded || cond.Type == nvcrev1alpha1.WorkflowFailed {
			return true
		}
	}
	return false
}
