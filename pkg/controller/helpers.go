// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Shared reason constants (all tiers) ---

const (
	// ReasonNotApplicable is the default condition reason for inactive conditions.
	ReasonNotApplicable = "NotApplicable"
)

// requeueImmediate is a short self-requeue delay used to advance a reconciler's
// state machine to its next step within the same logical transition.
//
// It replaces the deprecated ctrl.Result{Requeue: true}, which controller-runtime
// removed guidance for because it reuses the error rate limiter for non-error
// requeues. A fixed short delay is used rather than returning an empty Result
// because these call sites persist status conditionally (setExclusiveCondition
// only writes when a condition actually changed), so there is no guaranteed
// watch event to drive the next reconcile.
const requeueImmediate = 50 * time.Millisecond

// maxLogLookback bounds how far back a SinceTime-anchored pod log read may
// reach. The measurement controllers hold their anchor still in some recovery
// paths (e.g. a crash-looping container that has produced no parseable output
// yet), which without a clamp means re-reading from pod start on every sample
// for the life of the run.
const maxLogLookback = 30 * time.Minute

// Certification tier reasons (Certification → Workflow).
const (
	// ReasonWaitingForNodes marks a Certification that found no schedulable nodes
	// and is retrying. Without it the wait is invisible: the retry only logged, so
	// the CR carried no conditions at all for up to nodeDiscoveryTimeout and an
	// operator had nothing to explain why nothing was happening.
	ReasonWaitingForNodes          = "WaitingForNodes"
	ReasonWorkflowCreated          = "WorkflowCreated"
	ReasonWorkflowRunning          = "WorkflowRunning"
	ReasonAllWorkflowsSucceeded    = "AllWorkflowsSucceeded"
	ReasonWorkflowFailed           = "WorkflowFailed"
	ReasonWorkflowValidationFailed = "WorkflowValidationFailed"
	ReasonCategoryNotFound         = "CategoryNotFound"
	ReasonBuildFailed              = "BuildFailed"
	ReasonWorkflowDeleted          = "WorkflowDeleted"
	ReasonWorkflowSucceeded        = "WorkflowSucceeded"
	ReasonThresholdViolation       = "ThresholdViolation"

	// ReasonWorkflowNameCollision marks a Certification whose generated child
	// Workflow name is already taken by a Workflow this Certification does not
	// own. The foreign object is neither adopted nor recorded for cleanup.
	ReasonWorkflowNameCollision = "WorkflowNameCollision"
)

// Workflow tier reasons (Workflow → Job).
const (
	ReasonJobCreated          = "JobCreated"
	ReasonJobRunning          = "JobRunning"
	ReasonJobCompleted        = "JobCompleted"
	ReasonJobFailed           = "JobFailed"
	ReasonJobHardwareFailed   = "JobHardwareFailed"
	ReasonJobCreationError    = "JobCreationError"
	ReasonJobValidationFailed = "JobValidationFailed"

	// ReasonJobTimedOut marks a Job's Failed condition set by the Workflow when
	// the Job exceeded timeoutPerJob. Timed-out jobs are never retried.
	ReasonJobTimedOut = "JobTimedOut"

	ReasonGroupsPartitioned  = "GroupsPartitioned"
	ReasonIterationCompleted = "IterationCompleted"
	ReasonIterationsFailed   = "IterationsFailed"

	ReasonDependencyCreationError = "DependencyCreationError"
	ReasonNodeDiscoveryError      = "NodeDiscoveryError"
	ReasonPartitionError          = "PartitionError"

	// ReasonJobNameCollision marks a Workflow whose generated Job name is
	// already taken by a Job this Workflow does not control. The foreign
	// object is neither adopted nor recorded for cleanup.
	ReasonJobNameCollision = "JobNameCollision"
	// ReasonDependencyNameCollision marks a Workflow whose dependency resource
	// name is already taken by an object this Workflow did not create. The
	// foreign object is neither adopted nor recorded for cleanup.
	ReasonDependencyNameCollision = "DependencyNameCollision"
)

// Job tier reasons (Job → Workload).
const (
	ReasonWorkloadCreated = "WorkloadCreated"
	// ReasonWorkloadPending indicates the workload exists but has not started
	// running — e.g. a TrainJob suspended by Kueue while it waits for quota.
	// The Job stays InProgress; pending time is not counted against
	// timeoutPerJob or stall detection.
	ReasonWorkloadPending       = "WorkloadPending"
	ReasonWorkloadRunning       = "WorkloadRunning"
	ReasonWorkloadCompleted     = "WorkloadCompleted"
	ReasonWorkloadFailed        = "WorkloadFailed"
	ReasonWorkloadCreationError = "WorkloadCreationError"
	ReasonWorkloadStalled       = "WorkloadStalled"

	// ReasonMeasurementCreationError indicates a GoodputMeasurement or
	// BandwidthMeasurement child resource could not be created. Handling is
	// non-fatal, so this event is the operator-visible signal; a threshold that
	// depends on the missing measurement still fails closed via
	// ReasonMeasurementTimeout.
	ReasonMeasurementCreationError = "MeasurementCreationError"

	ReasonHardwareFailureDetected = "HardwareFailureDetected"
)

// Framework type constants for WorkloadRun.
const (
	FrameworkTorch = "torch"
	FrameworkMPI   = "mpi"
	FrameworkExec  = "exec"
)

// --- Shared name-collision error ---

// nameCollisionError reports that a child object the controller tried to
// create already exists but is not owned by the expected parent. Reconcilers
// detect it with errors.As, set their tier's Failed condition using Reason,
// and stop without recording the foreign object for cleanup.
type nameCollisionError struct {
	// Reason is the tier-specific condition reason (ReasonWorkflowNameCollision,
	// ReasonJobNameCollision, or ReasonDependencyNameCollision).
	Reason  string
	Message string
}

func (e *nameCollisionError) Error() string { return e.Message }

// --- Shared condition helpers ---

// CondIsTrue returns true if the named condition has Status=True.
func CondIsTrue(conditions []metav1.Condition, condType string) bool {
	c := meta.FindStatusCondition(conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// CondMessage returns the message for a condition type, or empty string if not found.
func CondMessage(conditions []metav1.Condition, condType string) string {
	c := meta.FindStatusCondition(conditions, condType)
	if c != nil {
		return c.Message
	}
	return ""
}

// --- Shared GPU defaults ---

// DefaultEnableMNNVL returns whether MNNVL should be enabled for the given GPU
// architecture. GB200 and GB300 use Multi-Node NVLink and benefit from MNNVL.
func DefaultEnableMNNVL(gpuArch string) bool {
	return gpuArch == "gb200" || gpuArch == "gb300"
}
