// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// updateStatusWithRetry applies mutate to obj and writes the status subresource,
// retrying on optimistic-concurrency conflicts.
//
// Every reconciler here follows read-from-cache → mutate → write. The cached
// object can be stale by the time the write lands — another controller touched
// the object, or this controller's own previous write has not yet propagated
// back through the informer — and the API server rejects it with a 409. Without
// a retry that surfaces as a reconcile error: an ERROR log line and a rate-
// limited requeue for what is a routine, expected condition.
//
// On conflict the object is re-read in place and mutate is applied to the fresh
// state, so mutate must be idempotent with respect to the object it is given.
// It returns true when it changed something; returning false skips the write
// entirely, which keeps no-op reconciles from generating API traffic.
func updateStatusWithRetry[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	mutate func(T) bool,
) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !mutate(obj) {
			return nil
		}

		err := c.Status().Update(ctx, obj)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}

		// Refresh in place so the next attempt re-applies mutate to current state.
		// A failure here is terminal for this reconcile: returning a non-conflict
		// error stops RetryOnConflict rather than spinning on a stale object.
		if getErr := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); getErr != nil {
			return getErr
		}
		return err
	})
}

// setExclusiveStatusCondition sets conditionType to True and every other type in
// allTypes to False, so the tier's lifecycle conditions stay mutually exclusive.
//
// This is shared by the Certification, Workflow and Job reconcilers, which
// differ only in their condition-type triple.
//
// Returns whether a write was actually issued, so callers can keep status-change
// logging and metrics on the transition rather than firing them on every
// no-op reconcile.
func setExclusiveStatusCondition[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	conditions func(T) *[]metav1.Condition,
	allTypes []string,
	conditionType, reason, message string,
	extra ...func(T) bool,
) (bool, error) {
	wrote := false
	err := updateStatusWithRetry(ctx, c, obj, func(o T) bool {
		changed := false
		// Apply any caller-supplied status mutation inside this same callback, so
		// it is re-applied after the refetch on conflict and so it keeps the write
		// from being skipped as a no-op. Status mutated outside the callback is
		// silently lost on both paths.
		for _, f := range extra {
			if f != nil && f(o) {
				changed = true
			}
		}
		for _, ct := range allTypes {
			status := metav1.ConditionFalse
			condReason := ReasonNotApplicable
			condMessage := ""

			if ct == conditionType {
				status = metav1.ConditionTrue
				condReason = reason
				condMessage = message
			}

			if meta.SetStatusCondition(conditions(o), metav1.Condition{
				Type:               ct,
				Status:             status,
				Reason:             condReason,
				Message:            condMessage,
				ObservedGeneration: o.GetGeneration(),
			}) {
				changed = true
			}
		}
		wrote = wrote || changed
		return changed
	})
	if err != nil {
		return false, fmt.Errorf("failed to update %T status: %w", obj, err)
	}
	return wrote, nil
}
