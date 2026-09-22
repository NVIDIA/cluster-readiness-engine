// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type conditionState struct {
	Present bool                   `json:"present"`
	Status  metav1.ConditionStatus `json:"status,omitempty"`
}

type conditionFlipResult struct {
	Type      string           `json:"type"`
	Before    conditionState   `json:"before"`
	After     conditionState   `json:"after"`
	Condition metav1.Condition `json:"condition"`
}

type conditionTransition struct {
	PreviousTrueType string           `json:"previousTrueType,omitempty"`
	NewTrueType      string           `json:"newTrueType"`
	Condition        metav1.Condition `json:"condition"`
}

// conditionFlip reports watched conditions whose presence or status changed.
// Changes to reason, message, timestamps, or observed generation alone are
// deliberately ignored. Results follow the order of watchedTypes.
func conditionFlip(before, after []metav1.Condition, watchedTypes []string) []conditionFlipResult {
	results := make([]conditionFlipResult, 0, len(watchedTypes))
	for _, conditionType := range watchedTypes {
		beforeCondition := meta.FindStatusCondition(before, conditionType)
		afterCondition := meta.FindStatusCondition(after, conditionType)
		beforeState := conditionState{Present: beforeCondition != nil}
		afterState := conditionState{Present: afterCondition != nil}
		if beforeCondition != nil {
			beforeState.Status = beforeCondition.Status
		}
		if afterCondition != nil {
			afterState.Status = afterCondition.Status
		}
		if beforeState == afterState {
			continue
		}
		result := conditionFlipResult{
			Type:   conditionType,
			Before: beforeState,
			After:  afterState,
		}
		if afterCondition != nil {
			result.Condition = *afterCondition
		}
		results = append(results, result)
	}
	return results
}

func trueConditionType(conditions []metav1.Condition, conditionTypes []string) string {
	for _, conditionType := range conditionTypes {
		condition := meta.FindStatusCondition(conditions, conditionType)
		if condition != nil && condition.Status == metav1.ConditionTrue {
			return conditionType
		}
	}
	return ""
}

// transitionEventType maps a newly-true condition type to its Event type, per
// ADR-080 decision 4: the tier's Failed type is a Warning and every other phase
// is Normal. failedType is the caller's own Failed constant rather than a
// hardcoded one, so a tier that renames its Failed condition cannot silently
// start reporting terminal failures as Normal.
//
// unparam is silenced deliberately: all four tiers spell their Failed condition
// "Failed" today, so the argument is redundant now and load-bearing the moment
// one of them diverges.
func transitionEventType(conditionType, failedType string) string { //nolint:unparam
	if conditionType == failedType {
		return corev1.EventTypeWarning
	}
	return corev1.EventTypeNormal
}

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
// Returns whether any attempt required a write and, after a successful final
// attempt, the exclusive true-type transition. Callers keep status-change
// logging and metrics off no-op reconciles and emit Events only from the final
// transition result.
func setExclusiveStatusCondition[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	conditions func(T) *[]metav1.Condition,
	allTypes []string,
	conditionType, reason, message string,
	extra ...func(T) bool,
) (bool, *conditionTransition, error) {
	return setExclusiveStatusConditionUnless(ctx, c, obj, conditions, allTypes,
		conditionType, reason, message, nil, extra...)
}

// stop is checked against each retry's object before any mutations. A caller
// can preserve a concurrent terminal decision without changing other tiers.
func setExclusiveStatusConditionUnless[T client.Object](
	ctx context.Context,
	c client.Client,
	obj T,
	conditions func(T) *[]metav1.Condition,
	allTypes []string,
	conditionType, reason, message string,
	stop func(T) bool,
	extra ...func(T) bool,
) (bool, *conditionTransition, error) {
	wrote := false
	var transition *conditionTransition
	err := updateStatusWithRetry(ctx, c, obj, func(o T) bool {
		// A conflict retry recomputes the transition from the freshly fetched
		// object. Clear any result from the previous failed attempt first.
		transition = nil
		if stop != nil && stop(o) {
			wrote = false
			return false
		}
		before := append([]metav1.Condition(nil), (*conditions(o))...)
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
		if !changed {
			return false
		}

		previousTrueType := trueConditionType(before, allTypes)
		newTrueType := trueConditionType(*conditions(o), allTypes)
		if previousTrueType != newTrueType && newTrueType != "" {
			condition := meta.FindStatusCondition(*conditions(o), newTrueType)
			transition = &conditionTransition{
				PreviousTrueType: previousTrueType,
				NewTrueType:      newTrueType,
				Condition:        *condition,
			}
		}
		return changed
	})
	if err != nil {
		return false, nil, fmt.Errorf("failed to update %T status: %w", obj, err)
	}
	return wrote, transition, nil
}
