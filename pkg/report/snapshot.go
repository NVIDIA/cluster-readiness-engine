// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
)

// ErrSnapshotNotReady means execution or measurement collection is still in
// progress. Callers should retry without freezing a report or releasing cleanup.
var ErrSnapshotNotReady = errors.New("report snapshot is not ready")

// ErrSourceNotFinal means execution or its parent status has not reached a
// consistent final result. It also matches ErrSnapshotNotReady. A deleting
// source may cancel this work, whereas a completed execution must keep its
// resources until pending measurements have been collected and saved.
var ErrSourceNotFinal = fmt.Errorf("%w: source has not reached its final state", ErrSnapshotNotReady)

const maxSnapshotNodeResultsBytes = 8 * 1024 * 1024

// BuildSnapshot builds a final report from verified source objects. Unlike Build,
// which offers best-effort CLI output, it rejects missing data and API failures.
// The caller must supply an uncached client: a cached terminal condition can
// otherwise refer to an earlier iteration or an object deleted and recreated
// under the same name. Reads are memoized for this call so rendering uses exactly
// the objects whose readiness and ownership were checked.
func BuildSnapshot(ctx context.Context, c client.Client, cert *nvcrev1alpha1.Certification) (*CertReport, error) {
	if c == nil || cert == nil || cert.UID == "" {
		return nil, errors.New("snapshot requires a client and a Certification with a UID")
	}
	reader := &snapshotClient{Client: c, reads: make(map[string]snapshotRead)}
	current := &nvcrev1alpha1.Certification{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(cert), current); err != nil {
		return nil, fmt.Errorf("read source Certification: %w", err)
	}
	if current.UID != cert.UID {
		return nil, errors.New("source Certification UID changed")
	}
	if cert.ResourceVersion != "" && current.ResourceVersion != cert.ResourceVersion {
		return nil, fmt.Errorf("%w: Certification changed while preparing the report", ErrSnapshotNotReady)
	}
	if !snapshotTerminal(current.Generation, current.Status.Conditions) {
		return nil, fmt.Errorf("%w: Certification is not terminal", ErrSourceNotFinal)
	}
	if err := validateSnapshotCategories(ctx, reader, current); err != nil {
		return nil, err
	}

	result := Build(ctx, reader, current)
	if err := errors.Join(reader.errors...); err != nil {
		return nil, fmt.Errorf("read report data: %w", err)
	}
	return result, nil
}

func validateSnapshotCategories(ctx context.Context, c *snapshotClient, cert *nvcrev1alpha1.Certification) error {
	failed := meta.IsStatusConditionTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed)
	if !failed && len(cert.Status.CategoryStatuses) != len(cert.Spec.Categories) {
		return fmt.Errorf("%w: Certification category statuses are incomplete", ErrSourceNotFinal)
	}
	for _, category := range cert.Status.CategoryStatuses {
		if category.WorkflowRef == nil {
			// A catalog, validation, or name-collision failure may stop the
			// Certification before it can create this category's Workflow.
			if failed && category.Status != statusSucceeded && category.FailedNodesRef == nil {
				continue
			}
			return fmt.Errorf("category %s/%s has no Workflow reference", category.Domain, category.Variant)
		}
		ns := category.WorkflowRef.Namespace
		if ns == "" {
			ns = cert.Namespace
		}
		if ns != cert.Namespace {
			return errors.New("referenced Workflow is outside the Certification namespace")
		}
		wf := &nvcrev1alpha1.Workflow{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: category.WorkflowRef.Name}, wf); err != nil {
			return fmt.Errorf("read Workflow %s: %w", category.WorkflowRef.Name, err)
		}
		if err := snapshotOwnedBy(wf, cert); err != nil {
			return err
		}
		if !meta.IsStatusConditionTrue(wf.Status.Conditions, category.Status) {
			return fmt.Errorf("%w: category %s/%s has not observed its Workflow result", ErrSourceNotFinal, category.Domain, category.Variant)
		}
		if err := validateSnapshotWorkflow(ctx, c, wf); err != nil {
			return err
		}
		if err := validateSnapshotFailedNodes(ctx, c, wf, category.FailedNodesRef); err != nil {
			return err
		}
	}
	return nil
}

func validateSnapshotWorkflow(ctx context.Context, c *snapshotClient, wf *nvcrev1alpha1.Workflow) error {
	if !snapshotTerminal(wf.Generation, wf.Status.Conditions) {
		return fmt.Errorf("%w: Workflow %s is not terminal", ErrSourceNotFinal, wf.Name)
	}
	if !wf.DeletionTimestamp.IsZero() {
		return fmt.Errorf("referenced Workflow %s is already being deleted", wf.Name)
	}
	if err := validateSnapshotFailedNodes(ctx, c, wf, wf.Status.FailedNodesRef); err != nil {
		return err
	}
	orch := wf.Status.Orchestration
	jobs := make(map[string]*nvcrev1alpha1.Job)
	if orch != nil {
		allGroupsTerminal := len(orch.Groups) > 0
		for _, group := range orch.Groups {
			terminal := group.Phase == nvcrev1alpha1.GroupSucceeded || group.Phase == nvcrev1alpha1.GroupFailed
			allGroupsTerminal = allGroupsTerminal && terminal
			if group.JobRef == nil {
				if terminal || !meta.IsStatusConditionTrue(wf.Status.Conditions, nvcrev1alpha1.WorkflowFailed) {
					return fmt.Errorf("referenced Workflow %s group %s has no Job reference", wf.Name, group.Name)
				}
				// An infrastructure failure can leave groups that never launched.
				continue
			}
			if !terminal {
				return fmt.Errorf("%w: Workflow %s group %s is still active", ErrSourceNotFinal, wf.Name, group.Name)
			}
			if group.JobRef.Namespace != "" && group.JobRef.Namespace != wf.Namespace {
				return fmt.Errorf("referenced Workflow %s group %s references a Job in another namespace", wf.Name, group.Name)
			}
			job := &nvcrev1alpha1.Job{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: group.JobRef.Name}, job); err != nil {
				return fmt.Errorf("read current Job %s: %w", group.JobRef.Name, err)
			}
			if err := validateSnapshotJob(job, wf); err != nil {
				return err
			}
			jobs[job.Name] = job
		}
		if allGroupsTerminal && orch.Diagnose == nil && orch.CompletedIterations < max(1, wf.Spec.Orchestration.Iterations) {
			return fmt.Errorf("%w: Workflow %s has unfinished iterations", ErrSourceNotFinal, wf.Name)
		}
		if orch.Diagnose != nil {
			var list nvcrev1alpha1.JobList
			if err := c.List(ctx, &list, client.InNamespace(wf.Namespace),
				client.MatchingLabels{labelWorkflow: wf.Name}); err != nil {
				return fmt.Errorf("list diagnose Jobs: %w", err)
			}
			for i := range list.Items {
				job := &list.Items[i]
				if err := validateSnapshotJob(job, wf); err != nil {
					return err
				}
				jobs[job.Name] = job
			}
		}
	}
	return validateSnapshotMeasurements(ctx, c, wf, jobs)
}

func validateSnapshotJob(job *nvcrev1alpha1.Job, wf *nvcrev1alpha1.Workflow) error {
	if err := snapshotOwnedBy(job, wf); err != nil {
		return err
	}
	if !snapshotTerminal(job.Generation, job.Status.Conditions) {
		return fmt.Errorf("%w: Job %s has not finished executing", ErrSourceNotFinal, job.Name)
	}
	if !job.DeletionTimestamp.IsZero() {
		return fmt.Errorf("current Job %s is already being deleted", job.Name)
	}
	return nil
}

func validateSnapshotMeasurements(ctx context.Context, c *snapshotClient, wf *nvcrev1alpha1.Workflow,
	jobs map[string]*nvcrev1alpha1.Job,
) error {
	var goodput nvcrev1alpha1.GoodputMeasurementList
	if err := c.List(ctx, &goodput, client.InNamespace(wf.Namespace)); err != nil {
		return fmt.Errorf("list GoodputMeasurements: %w", err)
	}
	var bandwidth nvcrev1alpha1.BandwidthMeasurementList
	if err := c.List(ctx, &bandwidth, client.InNamespace(wf.Namespace)); err != nil {
		return fmt.Errorf("list BandwidthMeasurements: %w", err)
	}
	hasGoodput, hasBandwidth := map[string]bool{}, map[string]bool{}
	for i := range goodput.Items {
		m := &goodput.Items[i]
		job := jobs[m.Spec.JobRef.Name]
		if job == nil {
			continue
		}
		if err := validateSnapshotMeasurement(m, m.Status.Conditions, wf, job); err != nil {
			return err
		}
		hasGoodput[job.Name] = true
	}
	for i := range bandwidth.Items {
		m := &bandwidth.Items[i]
		job := jobs[m.Spec.JobRef.Name]
		if job == nil {
			continue
		}
		if err := validateSnapshotMeasurement(m, m.Status.Conditions, wf, job); err != nil {
			return err
		}
		hasBandwidth[job.Name] = true
	}
	for _, job := range jobs {
		// Workload creation can fail before the measurement controllers have
		// anything to observe. That is a valid final report without metrics.
		if job.Status.WorkloadRef == nil && meta.IsStatusConditionTrue(job.Status.Conditions, nvcrev1alpha1.JobFailed) {
			continue
		}
		if job.Spec.GoodputMeasurement != nil && !hasGoodput[job.Name] {
			return fmt.Errorf("referenced Job %s is missing its GoodputMeasurement", job.Name)
		}
		if job.Spec.BandwidthMeasurement != nil && !hasBandwidth[job.Name] {
			return fmt.Errorf("referenced Job %s is missing its BandwidthMeasurement", job.Name)
		}
	}
	return nil
}

func validateSnapshotMeasurement(obj client.Object, conditions []metav1.Condition,
	wf *nvcrev1alpha1.Workflow, job *nvcrev1alpha1.Job,
) error {
	if snapshotOwnedBy(obj, wf) != nil && snapshotOwnedBy(obj, job) != nil {
		return fmt.Errorf("measurement %s does not belong to Workflow %s or Job %s", obj.GetName(), wf.Name, job.Name)
	}
	complete := meta.FindStatusCondition(conditions, "Complete")
	if complete == nil || complete.Status != metav1.ConditionTrue || complete.ObservedGeneration < obj.GetGeneration() {
		return fmt.Errorf("%w: measurement %s is not complete", ErrSnapshotNotReady, obj.GetName())
	}
	return nil
}

func validateSnapshotFailedNodes(ctx context.Context, c *snapshotClient, wf *nvcrev1alpha1.Workflow,
	ref *corev1.TypedLocalObjectReference,
) error {
	if ref == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: wf.Namespace, Name: ref.Name}, cm); err != nil {
		return fmt.Errorf("read failed nodes ConfigMap %s: %w", ref.Name, err)
	}
	if err := snapshotOwnedBy(cm, wf); err != nil {
		return err
	}
	if len(cm.BinaryData[noderesults.FailedNodesConfigMapKey]) == 0 {
		return fmt.Errorf("referenced ConfigMap %s has no failed-nodes data", cm.Name)
	}
	// A small ConfigMap can expand far beyond the exporter's memory budget.
	// Bound decompression before the CLI builder decodes these same cached bytes.
	if err := boundSnapshotNodeResults(cm.BinaryData[noderesults.FailedNodesConfigMapKey]); err != nil {
		return fmt.Errorf("decode ConfigMap %s: %w", cm.Name, err)
	}
	if _, err := noderesults.DecodeFailedNodesFromConfigMap(cm); err != nil {
		return fmt.Errorf("decode ConfigMap %s: %w", cm.Name, err)
	}
	return nil
}

func boundSnapshotNodeResults(compressed []byte) error {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return fmt.Errorf("invalid failed-nodes compression: %w", err)
	}
	defer func() { _ = reader.Close() }()
	n, err := io.Copy(io.Discard, io.LimitReader(reader, maxSnapshotNodeResultsBytes+1))
	if err != nil {
		return fmt.Errorf("read failed-nodes data: %w", err)
	}
	if n > maxSnapshotNodeResultsBytes {
		return errors.New("failed-nodes data exceeds the 8 MiB decompression limit")
	}
	return nil
}

func snapshotTerminal(generation int64, conditions []metav1.Condition) bool {
	for _, typ := range []string{statusSucceeded, statusFailed} {
		condition := meta.FindStatusCondition(conditions, typ)
		if condition != nil && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration >= generation {
			return true
		}
	}
	return false
}

func snapshotOwnedBy(obj, owner client.Object) error {
	ref := metav1.GetControllerOf(obj)
	if ref == nil || owner.GetUID() == "" || ref.UID != owner.GetUID() || ref.Name != owner.GetName() || obj.GetNamespace() != owner.GetNamespace() {
		return fmt.Errorf("%T %s does not belong to source %s (UID %s)", obj, obj.GetName(), owner.GetName(), owner.GetUID())
	}
	return nil
}

type snapshotRead struct {
	data []byte
	err  error
}

// snapshotClient memoizes reads, including errors. Build deliberately tolerates
// errors for interactive output; recording them here prevents a strict snapshot
// from silently inheriting that behavior. An optional batch Job may already have
// been cleaned up; its saved NVCRE Job condition remains the authoritative cause.
type snapshotClient struct {
	client.Client
	reads  map[string]snapshotRead
	errors []error
}

func (c *snapshotClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	cacheKey := fmt.Sprintf("get:%T:%s", obj, key)
	if cached, ok := c.reads[cacheKey]; ok {
		return cached.copyTo(obj)
	}
	err := c.Client.Get(ctx, key, obj, opts...)
	cached := snapshotRead{err: err}
	if err == nil {
		cached.data, cached.err = json.Marshal(obj)
	}
	c.reads[cacheKey] = cached
	_, optionalBatchJob := obj.(*batchv1.Job)
	if cached.err != nil && (!optionalBatchJob || !apierrors.IsNotFound(cached.err)) {
		c.errors = append(c.errors, fmt.Errorf("get %T %s: %w", obj, key, cached.err))
	}
	return cached.err
}

func (c *snapshotClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	options := (&client.ListOptions{}).ApplyOptions(opts)
	cacheKey := fmt.Sprintf("list:%T:%s:%v:%v", list, options.Namespace, options.LabelSelector, options.FieldSelector)
	if cached, ok := c.reads[cacheKey]; ok {
		return cached.copyTo(list)
	}
	err := c.Client.List(ctx, list, opts...)
	cached := snapshotRead{err: err}
	if err == nil {
		cached.err = c.rememberListObjects(list)
		if cached.err == nil {
			cached.data, cached.err = json.Marshal(list)
		}
	}
	c.reads[cacheKey] = cached
	if cached.err != nil {
		c.errors = append(c.errors, fmt.Errorf("list %T: %w", list, cached.err))
	}
	return cached.err
}

func (c *snapshotClient) rememberListObjects(list client.ObjectList) error {
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok {
			return fmt.Errorf("report list contains a non-object %T", item)
		}
		key := fmt.Sprintf("get:%T:%s", obj, client.ObjectKeyFromObject(obj))
		if cached, exists := c.reads[key]; exists {
			if err := cached.copyTo(obj); err != nil {
				return err
			}
			continue
		}
		data, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		c.reads[key] = snapshotRead{data: data}
	}
	return meta.SetList(list, items)
}

func (r snapshotRead) copyTo(dst any) error {
	if r.err != nil {
		return r.err
	}
	// Unmarshal does not clear fields absent from the encoded object. Always
	// start with a zero value, just as a fresh API read would.
	v := reflect.ValueOf(dst).Elem()
	v.SetZero()
	return json.Unmarshal(r.data, dst)
}
