// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// A group retry happens in two steps (issue #389).
//
// completeTerminalGroup records the retry: it bumps the group's retry count and
// moves it to Pending, and deletes nothing. The failed attempt's Job and its
// job-scoped dependencies stay where they are until that transition has been
// written. If the write never happens, the group is still Running on a failed
// Job that still exists, and the next pass reaches the same decision once.
//
// retirePreviousAttempts runs before the next attempt launches, from a state
// that is already durable: the group is Pending with the new retry count. It
// removes what earlier attempts left behind and holds the launch until the
// API server confirms it is gone. Every step is a live read followed by an
// idempotent delete, so an interruption anywhere is resumed by the next pass.
//
// Earlier attempts are identified without anything in the CRD. Each attempt's
// Job has its own name (getGroupJobName), and job-scoped dependency copies,
// which keep their names across attempts, record the attempt they were
// created for in annotationGroupAttempt.

// retirePreviousAttempts reports whether a Pending group's next attempt may
// launch. It deletes earlier attempts' Jobs, then their job-scoped
// dependencies, and returns ready only once none of them exist.
//
// Order follows ADR-053: Jobs first, dependencies only after every earlier
// Job is gone, because the dependencies (ComputeDomain channels, say) serve
// the pods those Jobs' finalizers are still draining.
//
// Dependency refs are dropped from workflow.Status only when the object is
// confirmed gone, not when its delete is issued, so a pass that stops partway
// leaves the next one something to find. The caller persists the refs with the
// launch.
func (r *WorkflowReconciler) retirePreviousAttempts(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	g *nvcrev1alpha1.GroupStatus, orch *nvcrev1alpha1.OrchestrationStatus,
) (bool, error) {
	if g.Retries == 0 {
		return true, nil
	}
	log := logf.FromContext(ctx).WithValues("group", g.Name, "attempt", g.Retries)

	jobsLeft := false
	for attempt := range g.Retries {
		name := r.getGroupJobName(workflow, g.Name, orch.CurrentIteration, attempt)
		left, err := r.retirePreviousJob(ctx, workflow, name)
		if err != nil {
			return false, err
		}
		jobsLeft = jobsLeft || left
	}
	if jobsLeft {
		log.V(1).Info("Waiting for the previous attempt's Job to be deleted")
		return false, nil
	}

	// The measurements an earlier attempt's Job had are now orphans: the
	// Workflow owns them, so they outlived the Job, and nothing reads them
	// once the group points at a new attempt. Best-effort; they never hold
	// up the launch.
	for attempt := range g.Retries {
		r.retirePreviousMeasurements(ctx, workflow, r.getGroupJobName(workflow, g.Name, orch.CurrentIteration, attempt))
	}

	depsLeft, err := r.retirePreviousDependencies(ctx, workflow, g, orch)
	if err != nil {
		return false, err
	}
	if depsLeft {
		log.V(1).Info("Waiting for the previous attempt's job-scoped dependencies to be deleted")
		return false, nil
	}
	return true, nil
}

// retirePreviousJob deletes an earlier attempt's Job if this Workflow controls
// it, and reports whether it still exists. A foreign Job holding the name is
// not ours to retire, and does not block the launch: the new attempt has a
// different name.
func (r *WorkflowReconciler) retirePreviousJob(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, name string,
) (bool, error) {
	job := &nvcrev1alpha1.Job{}
	key := client.ObjectKey{Namespace: workflow.Namespace, Name: name}
	if err := r.jobReader().Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read previous attempt's Job %s: %w", name, err)
	}
	if !metav1.IsControlledBy(job, workflow) {
		return false, nil
	}
	if job.DeletionTimestamp.IsZero() {
		logf.FromContext(ctx).Info("Deleting previous attempt's Job", "job", name)
		// UID precondition: delete the Job that was read, never a newer one
		// that took its name in between.
		if err := r.Delete(ctx, job, client.Preconditions{UID: new(job.UID)}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return false, fmt.Errorf("failed to delete previous attempt's Job %s: %w", name, err)
		}
	}
	return true, nil
}

// retirePreviousMeasurements deletes the Goodput and Bandwidth measurements the
// Job controller created for an earlier attempt's Job, when this Workflow
// controls them.
func (r *WorkflowReconciler) retirePreviousMeasurements(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow, jobName string,
) {
	log := logf.FromContext(ctx)
	for _, obj := range []client.Object{
		&nvcrev1alpha1.GoodputMeasurement{Name: goodputMeasurementName(jobName), Namespace: workflow.Namespace},
		&nvcrev1alpha1.BandwidthMeasurement{Name: bandwidthMeasurementName(jobName), Namespace: workflow.Namespace},
	} {
		if err := r.jobReader().Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if !apierrors.IsNotFound(err) {
				log.V(1).Info("Could not read previous attempt's measurement", "name", obj.GetName(), "error", err)
			}
			continue
		}
		if !metav1.IsControlledBy(obj, workflow) || obj.GetDeletionTimestamp() != nil {
			continue
		}
		log.Info("Deleting previous attempt's measurement", "name", obj.GetName())
		if err := r.Delete(ctx, obj, client.Preconditions{UID: new(obj.GetUID())}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			log.V(1).Info("Could not delete previous attempt's measurement", "name", obj.GetName(), "error", err)
		}
	}
}

// retirePreviousDependencies removes earlier attempts' job-scoped dependency
// copies for the group's current iteration and reports whether any remain.
//
// Two sets are checked, because neither is complete on its own: the refs
// recorded for the group, and the names the next attempt will create. Refs
// lag reality in both directions. cleanupScopedDependencies drops a ref when it
// issues the delete, not when the copy is gone, and a crash before the status
// write can leave a copy that was never recorded. The names catch both; the
// refs catch a copy the template no longer names.
//
// A copy created for the attempt about to launch belongs to it (an earlier
// pass of this launch created it and then requeued), and is left in place.
func (r *WorkflowReconciler) retirePreviousDependencies(
	ctx context.Context, workflow *nvcrev1alpha1.Workflow,
	g *nvcrev1alpha1.GroupStatus, orch *nvcrev1alpha1.OrchestrationStatus,
) (bool, error) {
	expected, err := r.expectedJobDependencyRefs(workflow, g, orch)
	if err != nil {
		return false, err
	}

	ownsRef := func(ref nvcrev1alpha1.DependencyResourceRef) bool {
		return ref.Scope == scopeJob && ref.GroupName == g.Name && ref.Iteration == orch.CurrentIteration
	}
	var others, candidates []nvcrev1alpha1.DependencyResourceRef
	tracked := map[string]bool{}
	for _, ref := range workflow.Status.DependencyRefs {
		if !ownsRef(ref) {
			others = append(others, ref)
			continue
		}
		candidates = append(candidates, ref)
		tracked[dependencyRefKey(ref)] = true
	}
	for _, ref := range expected {
		if !tracked[dependencyRefKey(ref)] {
			candidates = append(candidates, ref)
		}
	}

	left := false
	var keep []nvcrev1alpha1.DependencyResourceRef
	// Reverse of creation order, as cleanupScopedDependencies deletes.
	for _, ref := range reverseDependencyRefs(candidates) {
		isTracked := tracked[dependencyRefKey(ref)]
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(ref.APIVersion)
		obj.SetKind(ref.Kind)
		if err := r.jobReader().Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				// A gone PVC still has its backing PV to release, and keeps its
				// ref until that is done. Hold the launch with it: while a ref
				// for the group remains, launching would skip creating the
				// group's copies.
				if isTracked && ref.Kind == kindPVC && r.deleteScopedDependency(ctx, workflow, ref, nil) {
					left = true
					keep = append(keep, ref)
				}
				continue
			}
			return false, fmt.Errorf("failed to read job-scoped dependency %s %s: %w", ref.Kind, ref.Name, err)
		}
		switch {
		case !dependencyOwnedForCleanup(obj, workflow):
			// Foreign: never ours to delete. Launch reports the collision.
		case obj.GetDeletionTimestamp() != nil:
			left = true
			if isTracked {
				keep = append(keep, ref)
			}
		case dependencyAttempt(obj) == g.Retries:
			// This attempt's own copy. An untracked one stays untracked, so
			// launch still creates the rest of the set and adopts this one.
			if isTracked {
				keep = append(keep, ref)
			}
		default:
			left = true
			// Tracked until it is confirmed gone, whether or not it was before:
			// the finalizer then cleans it up if the Workflow is deleted first.
			r.deleteScopedDependency(ctx, workflow, ref, obj)
			keep = append(keep, ref)
		}
	}
	// keep was built in reverse; restore creation order for the next pass.
	workflow.Status.DependencyRefs = append(others, reverseDependencyRefs(keep)...)
	return left, nil
}

// expectedJobDependencyRefs returns refs for the job-scoped dependency copies
// the group's next attempt will create, in creation order, without creating
// anything.
func (r *WorkflowReconciler) expectedJobDependencyRefs(
	workflow *nvcrev1alpha1.Workflow,
	g *nvcrev1alpha1.GroupStatus, orch *nvcrev1alpha1.OrchestrationStatus,
) ([]nvcrev1alpha1.DependencyResourceRef, error) {
	prepared, err := prepareJobDependencies(workflow, g, orch, groupJobSpec(workflow))
	if err != nil {
		return nil, fmt.Errorf("failed to prepare job dependencies for group %s: %w", g.Name, err)
	}
	refs := make([]nvcrev1alpha1.DependencyResourceRef, 0, len(prepared.renamed))
	for _, dep := range prepared.renamed {
		obj := &unstructured.Unstructured{}
		if err := json.Unmarshal(dep.Raw, &obj.Object); err != nil {
			return nil, fmt.Errorf("failed to unmarshal suffixed dependency: %w", err)
		}
		// Namespace defaulting as createDependencyResource does it.
		if obj.GetNamespace() == "" {
			if namespaced, err := r.IsObjectNamespaced(obj); err != nil || namespaced {
				obj.SetNamespace(workflow.Namespace)
			}
		}
		refs = append(refs, nvcrev1alpha1.DependencyResourceRef{
			APIVersion: obj.GetAPIVersion(),
			Kind:       obj.GetKind(),
			Name:       obj.GetName(),
			Namespace:  obj.GetNamespace(),
			Scope:      scopeJob,
			GroupName:  g.Name,
			Iteration:  orch.CurrentIteration,
		})
	}
	return refs, nil
}
