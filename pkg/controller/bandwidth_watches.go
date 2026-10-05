// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"reflect"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// The BandwidthMeasurement and Job controllers hand off to each other: a Job
// reaching a terminal state starts the measurement's final log read while the
// launcher pod still exists, and the measurement completing lets the Job
// evaluate its thresholds. Both directions are watched so neither waits out a
// polling interval, and both are filtered so routine status writes on either
// side do not fan out into reconciles of the other.

// jobToBandwidthMeasurements maps a Job event to the BandwidthMeasurements
// that reference it.
func (r *BandwidthMeasurementReconciler) jobToBandwidthMeasurements(ctx context.Context, obj client.Object) []reconcile.Request {
	var list nvcrev1alpha1.BandwidthMeasurementList
	if err := r.List(ctx, &list, matchingJobRef(obj.GetNamespace(), obj.GetName())...); err != nil {
		logf.FromContext(ctx).V(1).Info("Failed to list BandwidthMeasurements for Job, skipping enqueue",
			"job", obj.GetName(), "error", err)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return requests
}

// jobPhaseChangePredicate passes Job updates that change its phase or its
// workload, and Job deletions.
func jobPhaseChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldJob, ok1 := e.ObjectOld.(*nvcrev1alpha1.Job)
			newJob, ok2 := e.ObjectNew.(*nvcrev1alpha1.Job)
			if !ok1 || !ok2 {
				return false
			}
			for _, t := range []string{nvcrev1alpha1.JobInProgress, nvcrev1alpha1.JobSucceeded, nvcrev1alpha1.JobFailed} {
				if meta.IsStatusConditionTrue(oldJob.Status.Conditions, t) != meta.IsStatusConditionTrue(newJob.Status.Conditions, t) {
					return true
				}
			}
			return !reflect.DeepEqual(oldJob.Status.WorkloadRef, newJob.Status.WorkloadRef)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// bandwidthMeasurementToJob maps a BandwidthMeasurement event to the Job it
// measures.
func bandwidthMeasurementToJob(_ context.Context, obj client.Object) []reconcile.Request {
	bm, ok := obj.(*nvcrev1alpha1.BandwidthMeasurement)
	if !ok || bm.Spec.JobRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{Namespace: bm.Namespace, Name: bm.Spec.JobRef.Name}}
}

// bandwidthCompletedPredicate passes only the update that completes a
// BandwidthMeasurement, not its periodic samples.
func bandwidthCompletedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldBM, ok1 := e.ObjectOld.(*nvcrev1alpha1.BandwidthMeasurement)
			newBM, ok2 := e.ObjectNew.(*nvcrev1alpha1.BandwidthMeasurement)
			if !ok1 || !ok2 {
				return false
			}
			return !meta.IsStatusConditionTrue(oldBM.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete) &&
				meta.IsStatusConditionTrue(newBM.Status.Conditions, nvcrev1alpha1.BandwidthMeasurementComplete)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
