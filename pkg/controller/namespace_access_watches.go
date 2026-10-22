// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// namespaceAccessGrantedPredicate passes only the end of an access wait.
func namespaceAccessGrantedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldWorkflow, oldOK := e.ObjectOld.(*nvcrev1alpha1.Workflow)
			newWorkflow, newOK := e.ObjectNew.(*nvcrev1alpha1.Workflow)
			if !oldOK || !newOK || oldWorkflow == nil || newWorkflow == nil {
				return false
			}
			return meta.IsStatusConditionFalse(oldWorkflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess) &&
				meta.IsStatusConditionTrue(newWorkflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// namespaceAccessToJobs wakes running groups on recovery. Mapping the refs
// needs no API read: even a Job that already created its workload can reconcile
// once, while a failed lookup must not lose the wake-up. No timeout is reset.
func namespaceAccessToJobs(_ context.Context, obj client.Object) []reconcile.Request {
	workflow, ok := obj.(*nvcrev1alpha1.Workflow)
	if !ok || workflow == nil || !workflow.DeletionTimestamp.IsZero() || workflow.Status.Orchestration == nil ||
		!meta.IsStatusConditionTrue(workflow.Status.Conditions, nvcrev1alpha1.WorkflowNamespaceAccess) {
		return nil
	}
	var requests []reconcile.Request
	for _, group := range workflow.Status.Orchestration.Groups {
		if group.Phase != nvcrev1alpha1.GroupRunning || group.JobRef == nil || group.JobRef.Name == "" {
			continue
		}
		namespace := group.JobRef.Namespace
		if namespace == "" {
			namespace = workflow.Namespace
		}
		requests = append(requests, reconcile.Request{Namespace: namespace, Name: group.JobRef.Name})
	}
	return requests
}
