// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/podlogs"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/shard"
)

// Labels on a shard ConfigMap. The workflow-uid label is the selector
// WorkflowStatus publishes, and the rest let nvcrectl narrow a bundle to one
// group, Job or attempt without decoding anything.
const (
	labelShardWorkflowUID = "nvcre.nvidia.com/workflow-uid"
	labelShardJob         = "nvcre.nvidia.com/job"
	labelShardKind        = "nvcre.nvidia.com/shard-kind"

	shardKindRows       = "rows"
	shardKindTruncation = "truncation"

	// shardNamePrefix prefixes every shard ConfigMap, so a human listing a
	// namespace can tell them from the node-result objects.
	shardNamePrefix = "shard-"
)

// componentResultsSelector is the label selector that resolves every shard a
// Workflow owns. It is published on WorkflowStatus rather than a list of
// references, because shard count tracks Job count: a one-node-per-Job entry
// over a thousand nodes produces a thousand shards, and a reference list that
// long is tens of kilobytes of status (ADR-090 decision 3).
func componentResultsSelector(workflowUID string) string {
	return labelShardWorkflowUID + "=" + workflowUID
}

// shardNameStem returns the deterministic stem of a Job's shard names; shard i
// is "<stem>-<i padded to four digits>".
//
// Derived rather than stored. The retry and deletion paths need to name an
// attempt's shards after its Job object is gone, and the controller has no
// list permission on ConfigMaps to find them by selector — but both inputs are
// state those paths already hold: retirePreviousAttempts re-derives each
// earlier attempt's Job name itself, and the Workflow UID is on the object it
// is iterating. Storing the stem cost 200 bytes per seal of the largest field
// in the struct to save this concatenation.
//
// The Job name already distinguishes group, iteration and attempt, because
// getGroupJobName appends "-retry-N". The Workflow UID prefix guards a
// Workflow recreated under the same name, which is the same guard
// nodeResultsCMName uses.
func shardNameStem(workflowUID, jobName string) string {
	uid := workflowUID
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return shardNamePrefix + uid + "-" + jobName
}

func shardName(stem string, index int) string {
	return fmt.Sprintf("%s-%04d", stem, index)
}

// componentResultsRequeue is how long to wait before retrying a shard write
// that failed for a reason that might clear.
const componentResultsRequeue = 10 * time.Second

// componentReadRetryBudget is how long a failing log read is retried.
//
// Deliberately SHORTER than componentSealGracePeriod, which is how long
// completeTerminalGroup waits for this seal. Both are measured from the same
// drainStart, so retrying for the full grace period meant a read that failed
// throughout it produced its seal at the moment the group stopped waiting: the
// retry would have been for nothing, and the group would record a Job that
// owed rows as having never sealed.
//
// The margin is three requeues, which leaves time for the final attempt to
// fail, the seal to be written, and the group to observe it on its next pass.
const componentReadRetryBudget = componentSealGracePeriod - 3*componentResultsRequeue

// reconcileComponentResults produces a terminal Job's per-component rows and
// writes them as shards.
//
// It runs as its own pass after the workload reconcile, NOT inside the
// transition that makes the Job terminal, and that placement is the whole
// design. Three things follow from it.
//
// It fires for every terminal Job whatever set the condition. setJobFailed,
// setJobSucceeded, the hardware-failure path and failTimedOutJob — which
// writes the Job's condition from the Workflow tier and bypasses setJobFailed
// entirely — all land here. ADR-099 had to patch FailedNodes separately for
// that last path; this needs no equivalent.
//
// It can report failure. The extra closure setJobFailed takes is
// func(*Job) bool, with no channel for an error, so a seal computed there
// could not say it had failed.
//
// And it never withholds the verdict. updateStatusWithRetry re-invokes its
// mutation after every conflict refetch, so a log read and a gzip inside that
// closure would re-run on every 409. Worse, requeuing the Job because a
// ConfigMap write failed would leave the Job non-terminal, the group Running,
// and timeoutPerJob would eventually convert a successful run into a timed-out
// failure. ADR-090 asks for the Workflow to be held open, not for the verdict
// to be falsified: the verdict is already persisted before this runs, and only
// the write is retried.
func (r *JobReconciler) reconcileComponentResults(
	ctx context.Context, job *nvcrev1alpha1.Job,
) (ctrl.Result, error) {
	if !r.isTerminalState(job) {
		return ctrl.Result{}, nil
	}
	if seal := job.Status.ComponentResults; seal != nil && componentResultsSealed(seal.Phase) {
		return ctrl.Result{}, nil
	}

	// A Job that declares no component results writes nothing, and that is a
	// decision rather than an omission. Sealing a positive Phase: None here
	// would be tidier to read in status, and it would put a status write and a
	// stored field on every Job in every certification to record that a feature
	// none of them uses owes them nothing.
	//
	// Nothing needs it. The group wait resolves an absent declaration straight
	// from the spec (componentSealTerminal), observeComponentSeal discards
	// Phase: None on arrival, and the short circuit above re-derives this in a
	// nil check on the next pass rather than a Get. The absent field is the
	// answer: a Job that owed rows always has a seal, so a Job without one
	// owed none.
	if job.Spec.ComponentResults == nil {
		return ctrl.Result{}, nil
	}

	log := logf.FromContext(ctx)

	workflow := r.getOwnerWorkflow(ctx, job)
	if workflow == nil {
		// Shards are Workflow-owned, so without the Workflow there is nothing
		// to own them. Requeue rather than fall back to Job ownership: a
		// Job-owned shard is deleted by the iteration rollover that deletes
		// its Job, while the Workflow total still claims it, and envtest has
		// no garbage collector so no test would see it.
		log.V(1).Info("Owning Workflow not readable yet; deferring component results")
		return ctrl.Result{RequeueAfter: componentResultsRequeue}, nil
	}

	result, produceErr := r.produceComponentRows(ctx, job)

	// Retry a transient read before sealing anything, whether or not some pods
	// already produced rows.
	//
	// A pod log read fails transiently for ordinary reasons: the API server is
	// briefly unavailable, or the pod is terminating and its logs are not
	// servable for a moment. Sealing on the first error made every one of
	// those permanent at the one moment a retry was most likely to succeed.
	// Gating the retry on having read NOTHING was the narrower mistake inside
	// that one: a node whose eighth pod failed while seven succeeded has rows,
	// so it took the sealing path and the result set was finalised one pod
	// short, with the terminal phase then blocking any later read.
	//
	// ErrLogUnpageable is excluded deliberately. A log that cannot be paged
	// fails the same way on every attempt, so retrying it only delays the
	// group for the whole grace period before recording the same thing.
	//
	// Bounded by componentReadRetryBudget from the Job going terminal, which is
	// deliberately shorter than the group's wait so the seal always lands while
	// something is still listening for it. A Job with no terminal timestamp to
	// measure from is sealed rather than retried: an unbounded retry here would
	// hold the group open indefinitely.
	if produceErr != nil && !errors.Is(produceErr, podlogs.ErrLogUnpageable) {
		if start := drainStart(job); start != nil &&
			time.Since(start.Time) <= componentReadRetryBudget {
			log.Error(produceErr, "A component result log could not be read; will retry",
				"within", componentReadRetryBudget, "rowsSoFar", len(result.rows))
			return ctrl.Result{RequeueAfter: componentResultsRequeue}, nil
		}
	}

	if produceErr != nil && result.rows == nil {
		message := truncateMessage(produceErr.Error())
		log.Error(produceErr, "Could not produce component result rows; sealing partial")

		// Merge into whatever is already sealed rather than replacing it.
		// A Job can reach this branch on a re-drive, after its pods are gone:
		// a shard write that kept failing leaves the seal in Writing, the
		// workload is deleted, and the next pass finds no logs. Constructing a
		// fresh seal then overwrote a real ShardCount with 0 while the shards
		// it counted stayed on the cluster, and the group had already copied
		// the original count, so the selector walk resolved more shards than
		// the total admitted and the category's bundle was refused for good.
		owed := &nvcrev1alpha1.ComponentResultSeal{}
		if prev := job.Status.ComponentResults; prev != nil &&
			prev.Phase != nvcrev1alpha1.ComponentResultsNone {
			owed = prev.DeepCopy()
			// The earlier seal's counts are a claim, not a receipt. It is
			// written BEFORE the first shard, so a Job that reached Writing and
			// then failed every shard write holds a positive ShardCount backed
			// by nothing. Carrying that forward hands nvcrectl a total it can
			// never satisfy and the category's bundle is refused permanently,
			// which is the same failure as dropping the count, reached from the
			// other side.
			//
			// So the counts are reconciled against what is actually on the
			// cluster. Fewer shards than claimed is the normal outcome here.
			if owed.ShardCount > 0 {
				gotShards, gotRows, err := r.observedShards(
					ctx, job.Namespace, shardNameStem(string(workflow.UID), job.Name),
					owed.ShardCount)
				if err != nil {
					// Requeued rather than sealed on a guess: this runs inside
					// the grace period's tail and a wrong count here is
					// permanent.
					log.Error(err, "Could not confirm which component result shards exist; will retry")
					return ctrl.Result{RequeueAfter: componentResultsRequeue}, nil
				}
				if gotShards != owed.ShardCount {
					log.Info("Lowering a seal to the shards that were actually written",
						"claimed", owed.ShardCount, "observed", gotShards)
				}
				owed.ShardCount, owed.RowCount = gotShards, gotRows
			}
		}
		// Writing, not Partial, and the order matters as much here as on
		// the normal path below. Partial is terminal, so the short-circuit
		// at the top of this function returns before doing anything once
		// it is set: sealing it first would make the requeue below a
		// no-op and lose the truncation shard on the first failed write.
		owed.Phase = nvcrev1alpha1.ComponentResultsWriting
		owed.Truncations = mergeTruncation(owed.Truncations, nvcrev1alpha1.ComponentTruncation{
			Reason:  result.reason,
			Count:   0,
			Message: message,
		})
		if err := r.sealComponentResults(ctx, job, owed); err != nil {
			return ctrl.Result{}, err
		}
		// Write the record as a shard too, even though there are no rows to go
		// with it. This is the one case where the category has nothing else in
		// the bundle, so it is the case where a loss recorded only in Job
		// status is most completely invisible to a bundle reader: without this
		// the category resolves to no rows and no stated reason, which reads
		// as a run that produced nothing rather than one that could not be
		// read.
		// The whole merged set, not only the record just added, so a re-drive
		// rewrites a truncation shard that still names the earlier loss.
		dropped := make([]shard.Dropped, 0, len(owed.Truncations))
		for _, t := range owed.Truncations {
			dropped = append(dropped, shard.Dropped{
				Reason: t.Reason, Count: t.Count, Message: t.Message,
			})
		}
		stem := shardNameStem(string(workflow.UID), job.Name)
		if err := r.writeComponentShards(ctx, workflow, job, stem, shard.Result{
			Dropped: dropped,
		}); err != nil {
			log.Error(err, "Failed to write the truncation record; will retry")
			return ctrl.Result{RequeueAfter: componentResultsRequeue}, nil
		}
		final := owed.DeepCopy()
		final.Phase = nvcrev1alpha1.ComponentResultsPartial
		return ctrl.Result{}, r.sealComponentResults(ctx, job, final)
	}

	built, buildErr := buildComponentShards(result.rows)
	if buildErr != nil {
		// A build failure is a bug rather than data: the options are fixed and
		// validated at construction. Surface it rather than sealing a result
		// set that was collected correctly.
		return ctrl.Result{}, fmt.Errorf("build component result shards for %s: %w", job.Name, buildErr)
	}

	stem := shardNameStem(string(workflow.UID), job.Name)
	truncations, controllerDropped := componentTruncations(built, result, produceErr)
	// Fold the controller's own losses into the set the shards carry, so
	// truncation.json reports everything the status does. Without this a read
	// that could not be paginated is recorded only in Job status, which a
	// bundle reader never sees.
	built.Dropped = append(built.Dropped, controllerDropped...)

	// The seal is written BEFORE the first shard exists (ADR-090 decision 3).
	// A Job whose every shard write then fails still records that it owed
	// rows, so a short shard set is distinguishable from a complete one. Were
	// the counts written afterwards, nothing would say the Job owed anything.
	degradedCount, degraded := degradedComponents(result.rows)
	sealed := &nvcrev1alpha1.ComponentResultSeal{
		Phase:       nvcrev1alpha1.ComponentResultsWriting,
		ShardCount:  int32(len(built.Shards)), //nolint:gosec // bounded by shard.MaxShardsPerJob
		RowCount:    built.Rows,
		SealedAt:    &metav1.Time{Time: time.Now()},
		Truncations: truncations,
		// Derived from the rows while they are still in hand. Resolving this
		// later from the shards would only work for a reader that asked for a
		// bundle, and the point of it is to reach the report of a run that
		// passed.
		DegradedComponentCount: degradedCount,
		DegradedComponents:     degraded,
	}
	if err := r.sealComponentResults(ctx, job, sealed); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.writeComponentShards(ctx, workflow, job, stem, built); err != nil {
		// Requeue the write, not the verdict. The Job stays terminal and the
		// seal stays, so the shortfall is visible while this retries.
		log.Error(err, "Failed to write component result shards; will retry")
		return ctrl.Result{RequeueAfter: componentResultsRequeue}, nil
	}

	phase := nvcrev1alpha1.ComponentResultsComplete
	if len(truncations) > 0 {
		phase = nvcrev1alpha1.ComponentResultsPartial
	}
	final := sealed.DeepCopy()
	final.Phase = phase
	return ctrl.Result{}, r.sealComponentResults(ctx, job, final)
}

// observedShards counts the row shards that actually exist for a stem, and the
// rows they hold, up to the number a seal claimed.
//
// By name rather than by selector, for the same reason deleteComponentShards
// is: the controller's ConfigMap grant has no list and no watch. Row counts
// come from each shard's data block rather than from decoding its payload,
// which is what the block carries them for.
//
// A read error is returned rather than swallowed. Guessing low would shrink a
// seal that was in fact complete, and guessing high would reinstate the
// mismatch this exists to avoid.
func (r *JobReconciler) observedShards(
	ctx context.Context, namespace, stem string, claimed int32,
) (int32, int64, error) {
	var shards int32
	var rows int64
	for i := range int(claimed) {
		cm := &corev1.ConfigMap{}
		key := client.ObjectKey{Namespace: namespace, Name: shardName(stem, i)}
		if err := r.Get(ctx, key, cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return 0, 0, fmt.Errorf("read shard %s: %w", key.Name, err)
		}
		shards++
		if n, err := strconv.ParseInt(cm.Data[shard.RowCountKey], 10, 64); err == nil {
			rows += n
		}
	}
	return shards, rows, nil
}

// componentResultsSealed reports whether a phase is terminal. Writing is not:
// a Job left there by a crash is re-driven, which is safe because the shard
// bytes are deterministic, so re-creating them is content-identical.
func componentResultsSealed(p nvcrev1alpha1.ComponentResultPhase) bool {
	switch p {
	case nvcrev1alpha1.ComponentResultsNone,
		nvcrev1alpha1.ComponentResultsComplete,
		nvcrev1alpha1.ComponentResultsPartial:
		return true
	default:
		return false
	}
}

// sealComponentResults persists a seal, re-applying it inside the conflict
// retry so a 409 cannot silently discard it.
func (r *JobReconciler) sealComponentResults(
	ctx context.Context, job *nvcrev1alpha1.Job, seal *nvcrev1alpha1.ComponentResultSeal,
) error {
	return updateStatusWithRetry(ctx, r.Client, job, func(j *nvcrev1alpha1.Job) bool {
		if j.Status.ComponentResults != nil &&
			equality.Semantic.DeepEqual(j.Status.ComponentResults, seal) {
			return false
		}
		j.Status.ComponentResults = seal.DeepCopy()
		return true
	})
}

// buildComponentShards encodes rows into shards under the package defaults.
func buildComponentShards(rows []shard.Row) (shard.Result, error) {
	b, err := shard.NewBuilder(shard.Options{})
	if err != nil {
		return shard.Result{}, err
	}
	for _, row := range rows {
		if addErr := b.Add(row); addErr != nil {
			return shard.Result{}, addErr
		}
	}
	return b.Build()
}

// writeComponentShards creates one ConfigMap per shard, plus a truncation
// sibling when any row was lost.
//
// Shards are Workflow-owned. The repository already settled this for the same
// class of object — "Own by Workflow so measurements survive iteration
// restarts (Job deletion)" — and the node-result ConfigMaps ADR-090 cites as
// precedent are Workflow-owned too. Job ownership would have them garbage
// collected by the iteration rollover that deletes every group's Job, while
// the Workflow's sealed total still claimed them, and nvcrectl would then
// refuse the bundle as short.
//
// Writes are idempotent by content: the shard bytes are a pure function of the
// rows, so re-running this after a partial failure rewrites identical objects.
func (r *JobReconciler) writeComponentShards(
	ctx context.Context,
	workflow *nvcrev1alpha1.Workflow,
	job *nvcrev1alpha1.Job,
	stem string,
	built shard.Result,
) error {
	for _, s := range built.Shards {
		cm := &corev1.ConfigMap{}
		cm.Name = shardName(stem, s.Index)
		cm.Namespace = job.Namespace
		if err := r.writeShardConfigMap(ctx, workflow, job, cm, shardKindRows, s.Data(), s.BinaryData()); err != nil {
			return err
		}
	}

	payload, err := shard.EncodeTruncation(built.Dropped)
	if err != nil {
		return fmt.Errorf("encode truncation records for %s: %w", job.Name, err)
	}
	if payload == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	cm.Name = stem + "-truncation"
	cm.Namespace = job.Namespace
	return r.writeShardConfigMap(ctx, workflow, job, cm, shardKindTruncation,
		map[string]string{
			shard.FormatVersionKey: strconv.Itoa(shard.FormatVersion),
			shard.RowCountKey:      strconv.FormatInt(built.DroppedTotal, 10),
		},
		map[string][]byte{shard.TruncationKey: payload})
}

func (r *JobReconciler) writeShardConfigMap(
	ctx context.Context,
	workflow *nvcrev1alpha1.Workflow,
	job *nvcrev1alpha1.Job,
	cm *corev1.ConfigMap,
	kind string,
	data map[string]string,
	binaryData map[string][]byte,
) error {
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if setErr := controllerutil.SetControllerReference(workflow, cm, r.Scheme); setErr != nil {
			return fmt.Errorf("set owner reference on %s: %w", cm.Name, setErr)
		}
		if cm.Labels == nil {
			cm.Labels = map[string]string{}
		}
		cm.Labels[labelShardWorkflowUID] = string(workflow.UID)
		cm.Labels[labelShardJob] = job.Name
		cm.Labels[labelShardKind] = kind
		cm.Labels["app.kubernetes.io/managed-by"] = managedByValue
		cm.Data = data
		cm.BinaryData = binaryData
		return nil
	})
	if err != nil {
		// A Forbidden write is the one failure here that no amount of retrying
		// resolves on its own, and it is also the one an operator can fix. It
		// used to be indistinguishable from any other error: both branches of
		// this condition returned the same value, so the two errors.As checks
		// on *accessDeniedWriteError in reconcileComponentResults never
		// matched and a missing run-role binding retried every 10s forever
		// with nothing anywhere naming access as the cause.
		//
		// An event rather than a wrapped error, because the write is retried
		// deliberately: it is idempotent, and the group's grace period already
		// bounds how long the Workflow waits for the seal. So the retry
		// continues and the operator is told why it is not progressing. The
		// Job tier cannot run reviewNamespaceAccess, which is a Workflow-tier
		// method over a Workflow's namespaces, so this names the grant instead
		// of claiming a review it did not perform.
		if apierrors.IsForbidden(err) {
			r.eventf(job, corev1.EventTypeWarning, ReasonComponentShardWriteRejected,
				"Writing component result shard %s was forbidden: %v. The controller needs "+
					"create, get, patch, update and delete on configmaps in %s, which "+
					"nvcre-run-role grants; the write is being retried.",
				cm.Name, err, cm.Namespace)
		}
		return fmt.Errorf("write shard %s: %w", cm.Name, err)
	}
	return nil
}

// deleteComponentShards removes an attempt's shards by derived name.
//
// By name rather than by selector, because the controller's ConfigMap grant is
// create, delete, get, patch and update — no list and no watch. The sealed
// shard count says how many there are, which is why it is kept on durable
// status rather than recomputed.
func deleteComponentShards(
	ctx context.Context, c client.Client, namespace, stem string, shardCount int32,
) error {
	for i := range int(shardCount) {
		cm := &corev1.ConfigMap{}
		cm.Name = shardName(stem, i)
		cm.Namespace = namespace
		if err := c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete shard %s: %w", cm.Name, err)
		}
	}
	cm := &corev1.ConfigMap{}
	cm.Name = stem + "-truncation"
	cm.Namespace = namespace
	if err := c.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete truncation record %s: %w", cm.Name, err)
	}
	return nil
}

// getLogFetcher returns the configured LogFetcher or falls back to a
// Clientset-backed one, mirroring the two measurement controllers.
func (r *JobReconciler) getLogFetcher() podlogs.PodLogFetcher {
	if r.LogFetcher != nil {
		return r.LogFetcher
	}
	return podlogs.NewKubernetesLogFetcher(r.Clientset)
}

// producedRows is what a producer managed to read, and why it stopped if it
// did not finish.
type producedRows struct {
	rows   []shard.Row
	reason string
	// partial is set when the log could be read only in part, so the rows that
	// were read are kept and the shortfall is recorded. ADR-090 requires that
	// rows lost to pagination are "never collapsed to zero rows".
	partial bool
	// rejected counts lines that declared a row the producer could not read,
	// and firstReject describes the first of them. Same rule as partial: the
	// rows that did parse are kept and the loss is recorded beside them.
	rejected    int64
	firstReject string
}

// componentResultPods returns the pods whose logs a Job's rows come from.
//
// Which pods it must return, per the repository's Pod-read checklist: every
// pod that could have printed a row marker. Where those are depends on the
// workload, so the Job declares it and this read follows the declaration.
//
// With no replicatedJobName, the rows come from worker pods and the cached
// read is correct: workers carry the job label, which is what the manager's
// Pod cache selects on (ADR-102). That is the single-worker-per-node shape
// ADR-091's GEMM entry uses, and it costs no API call.
//
// With one set, the read must go live. MPI launchers carry no job label, so
// the cache cannot hold them, and a cached read for a launcher returns no pods
// and no error — the rows would silently come back empty from a Job that
// produced them perfectly well. WorkloadPods selects on the JobSet labels
// instead, which every pod in the workload carries. This is the same split the
// bandwidth controller makes for the same reason: NCCL's output is on the
// launcher, not the workers.
func (r *JobReconciler) componentResultPods(
	ctx context.Context, job *nvcrev1alpha1.Job, spec *nvcrev1alpha1.ComponentResultSpec,
) ([]corev1.Pod, error) {
	pods := newPodReader(r.Client, r.APIReader)
	if spec.ReplicatedJobName == "" {
		found, err := pods.JobPods(ctx, job.Namespace, job.Name)
		if err != nil {
			return nil, fmt.Errorf("read worker pods for %s: %w", job.Name, err)
		}
		return found, nil
	}
	if job.Status.WorkloadRef == nil || job.Status.WorkloadRef.Name == "" {
		return nil, fmt.Errorf("job %s declares replicatedJobName %q but has no workload reference",
			job.Name, spec.ReplicatedJobName)
	}
	found, err := pods.WorkloadPods(ctx, job.Namespace, job.Status.WorkloadRef.Name, spec.ReplicatedJobName)
	if err != nil {
		return nil, fmt.Errorf("read %s pods for %s: %w", spec.ReplicatedJobName, job.Name, err)
	}
	return found, nil
}

// produceComponentRows reads the workload's pod log and turns it into rows.
func (r *JobReconciler) produceComponentRows(
	ctx context.Context, job *nvcrev1alpha1.Job,
) (producedRows, error) {
	spec := job.Spec.ComponentResults
	producer, err := rowProducerFor(spec)
	if err != nil {
		return producedRows{reason: reasonProducerUnavailable}, err
	}

	pods, err := r.componentResultPods(ctx, job, spec)
	if err != nil {
		return producedRows{reason: reasonProducerUnavailable}, err
	}
	if len(pods) == 0 {
		return producedRows{reason: reasonProducerUnavailable},
			fmt.Errorf("no pods found for %s; its logs are gone", job.Name)
	}

	var (
		all     produceResult
		lastErr error
		partial bool
	)
	for i := range pods {
		pod := &pods[i]
		var lines []string
		readErr := podlogs.ReadAll(ctx, r.getLogFetcher(), pod.Namespace, pod.Name,
			podlogs.ReadAllOptions{Container: spec.ContainerName},
			func(line string) { lines = append(lines, line) })
		if readErr != nil {
			// A log that cannot be paged fails the same way on every attempt,
			// so the lines that were read are kept and the shortfall recorded.
			lastErr = readErr
			if errors.Is(readErr, podlogs.ErrLogUnpageable) {
				partial = true
			}
		}
		produced, produceErr := producer.Produce(pod.Spec.NodeName, lines)
		if produceErr != nil {
			// The producer could not run. Keep what earlier pods gave.
			return producedRows{
				rows: all.Rows, rejected: all.Rejected, firstReject: all.FirstReject,
				reason: reasonProducerUnavailable, partial: partial,
			}, produceErr
		}
		all.add(produced)
	}

	out := producedRows{
		rows: all.Rows, rejected: all.Rejected, firstReject: all.FirstReject, partial: partial,
	}
	if partial {
		out.reason = reasonLogUnpageable
	} else if lastErr != nil {
		out.reason = reasonProducerUnavailable
	}
	return out, lastErr
}

// componentTruncations turns everything that was lost into status records,
// aggregated per reason because the list is a listType=map keyed on reason.
// It returns the status records and, separately, the subset the controller
// raised itself, encoded as shard.Dropped for the caller to append to the set
// that reaches truncation.json.
func componentTruncations(
	built shard.Result, produced producedRows, readErr error,
) ([]nvcrev1alpha1.ComponentTruncation, []shard.Dropped) {
	byReason := map[string]*nvcrev1alpha1.ComponentTruncation{}
	add := func(reason string, count int64, message string) {
		if existing, ok := byReason[reason]; ok {
			existing.Count += count
			// Keep the first message, but let a later one fill an empty slot:
			// the encoder's own drops carry no message, so without this a
			// shard drop would silence the parse detail that follows it.
			if existing.Message == "" {
				existing.Message = truncateMessage(message)
			}
			return
		}
		byReason[reason] = &nvcrev1alpha1.ComponentTruncation{
			Reason: reason, Count: count, Message: truncateMessage(message),
		}
	}

	// Records the controller raises rather than the encoder. These are also
	// returned as shard.Dropped so writeComponentShards puts them in
	// truncation.json: ADR-090 requires a run that could not read all its
	// evidence to say so in the bundle, and the bundle is the only artifact
	// that leaves the cluster. Returning them from here rather than building a
	// second set keeps the status and the bundle provably identical.
	var extra []shard.Dropped
	addBoth := func(reason string, count int64, message string) {
		add(reason, count, message)
		extra = append(extra, shard.Dropped{
			Reason: reason, Count: count, Message: truncateMessage(message),
		})
	}

	for _, d := range built.Dropped {
		add(d.Reason, d.Count, "")
	}
	// DroppedTotal is exact while the ledger is bounded, so a per-reason sum
	// over a capped ledger can understate. Reconcile the difference onto the
	// reason that produced it, which is the only one that can overflow.
	//
	// Summed before anything below is added, so a controller-side record
	// cannot be mistaken for encoder output and absorb a real shortfall.
	var ledgerTotal int64
	for _, t := range byReason {
		ledgerTotal += t.Count
	}
	if short := built.DroppedTotal - ledgerTotal; short > 0 {
		addBoth(shard.ReasonRowBudgetExceeded, short, "ledger bounded; count is exact")
	}

	// Any read error, not only a pagination one. Gating this on produced.partial
	// meant a read that failed for any other reason, a transient API error
	// against one pod out of eight, went unrecorded: the rows from the other
	// seven were stored and the Job sealed Complete with a short count, so the
	// bundle certified an incomplete set as whole. That is the one outcome
	// ADR-090's integrity rule is written to prevent, and it was the quiet one.
	//
	// The reason comes from the producer, which already distinguishes a log
	// that could not be paged from one that could not be read at all.
	//
	// Count 0 on purpose: a page that could not be fetched gives no way to
	// know how many rows were on it, and inventing one would misstate the
	// magnitude. The record is the finding.
	if readErr != nil {
		reason := produced.reason
		if reason == "" {
			reason = reasonProducerUnavailable
		}
		addBoth(reason, 0, readErr.Error())
	}
	// Lines that declared a row and could not be read. RowUnencodable rather
	// than a reason of its own: the enum's meaning is a row that could not be
	// represented, and a line that will not parse into one is the same loss
	// seen from the other side. The message names the first offender, which is
	// what a human needs to find the harness bug.
	if produced.rejected > 0 {
		addBoth(shard.ReasonRowUnencodable, produced.rejected, produced.firstReject)
	}

	out := make([]nvcrev1alpha1.ComponentTruncation, 0, len(byReason))
	for _, t := range byReason {
		out = append(out, *t)
	}
	// Sorted so the status is stable across reconciles and golden tests.
	sortTruncations(out)
	return out, extra
}

// Reasons the controller raises, as opposed to the encoder's own. These must
// stay within the ComponentTruncation enum.
const (
	reasonLogUnpageable       = "LogUnpageable"
	reasonProducerUnavailable = "ProducerUnavailable"
)

// mergeTruncation adds one record to a set already on a seal, folding it into
// a matching reason rather than appending a duplicate.
//
// Needed because the list is a listType=map keyed on reason, so two entries
// with the same reason are rejected by the API server, and because a Job can
// be re-driven onto the same branch and must not lose the earlier record.
func mergeTruncation(
	existing []nvcrev1alpha1.ComponentTruncation, add nvcrev1alpha1.ComponentTruncation,
) []nvcrev1alpha1.ComponentTruncation {
	out := slices.Clone(existing)
	for i := range out {
		if out[i].Reason != add.Reason {
			continue
		}
		out[i].Count += add.Count
		// Keep the first message: it names the earliest cause, and on a
		// re-drive the later one is usually a consequence of it.
		if out[i].Message == "" {
			out[i].Message = add.Message
		}
		return out
	}
	out = append(out, add)
	sortTruncations(out)
	return out
}

// sortTruncations orders records by reason, so a status write is stable across
// reconciles. An unstable order would make every reconcile look like a change
// and would make golden tests flap.
func sortTruncations(t []nvcrev1alpha1.ComponentTruncation) {
	slices.SortFunc(t, func(a, b nvcrev1alpha1.ComponentTruncation) int {
		return cmp.Compare(a.Reason, b.Reason)
	})
}

// maxTruncationMessage matches the CRD's MaxLength on the field.
const maxTruncationMessage = 256

func truncateMessage(s string) string {
	if len(s) <= maxTruncationMessage {
		return s
	}
	return s[:maxTruncationMessage-3] + "..."
}
