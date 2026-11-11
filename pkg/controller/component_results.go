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
	if produceErr != nil && result.rows == nil {
		// Nothing was read at all. Record why and seal Partial, rather than
		// requeueing forever on a log that will not become readable.
		log.Error(produceErr, "Could not produce component result rows")
		return ctrl.Result{}, r.sealComponentResults(ctx, job, &nvcrev1alpha1.ComponentResultSeal{
			Phase: nvcrev1alpha1.ComponentResultsPartial,
			Truncations: []nvcrev1alpha1.ComponentTruncation{{
				Reason:  result.reason,
				Count:   0,
				Message: truncateMessage(produceErr.Error()),
			}},
		})
	}

	built, buildErr := buildComponentShards(result.rows)
	if buildErr != nil {
		// A build failure is a bug rather than data: the options are fixed and
		// validated at construction. Surface it rather than sealing a result
		// set that was collected correctly.
		return ctrl.Result{}, fmt.Errorf("build component result shards for %s: %w", job.Name, buildErr)
	}

	stem := shardNameStem(string(workflow.UID), job.Name)
	truncations := componentTruncations(built, result, produceErr)

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
		if errors.As(err, new(*accessDeniedWriteError)) {
			return ctrl.Result{}, err
		}
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
		if apierrors.IsForbidden(err) && r.AccessReviewer != nil {
			return fmt.Errorf("write shard %s: %w", cm.Name, err)
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
func componentTruncations(
	built shard.Result, produced producedRows, readErr error,
) []nvcrev1alpha1.ComponentTruncation {
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

	for _, d := range built.Dropped {
		add(d.Reason, d.Count, "")
	}
	// DroppedTotal is exact while the ledger is bounded, so a per-reason sum
	// over a capped ledger can understate. Reconcile the difference onto the
	// reason that produced it, which is the only one that can overflow.
	var ledgerTotal int64
	for _, t := range byReason {
		ledgerTotal += t.Count
	}
	if short := built.DroppedTotal - ledgerTotal; short > 0 {
		add(shard.ReasonRowBudgetExceeded, short, "ledger bounded; count is exact")
	}

	if produced.partial && readErr != nil {
		add(reasonLogUnpageable, 0, readErr.Error())
	}
	// Lines that declared a row and could not be read. RowUnencodable rather
	// than a reason of its own: the enum's meaning is a row that could not be
	// represented, and a line that will not parse into one is the same loss
	// seen from the other side. The message names the first offender, which is
	// what a human needs to find the harness bug.
	if produced.rejected > 0 {
		add(shard.ReasonRowUnencodable, produced.rejected, produced.firstReject)
	}

	out := make([]nvcrev1alpha1.ComponentTruncation, 0, len(byReason))
	for _, t := range byReason {
		out = append(out, *t)
	}
	// Sorted so the status is stable across reconciles and golden tests.
	sortTruncations(out)
	return out
}

// Reasons the controller raises, as opposed to the encoder's own. These must
// stay within the ComponentTruncation enum.
const (
	reasonLogUnpageable       = "LogUnpageable"
	reasonProducerUnavailable = "ProducerUnavailable"
)

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
