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

	ReasonGroupsPartitioned  = "GroupsPartitioned"
	ReasonIterationCompleted = "IterationCompleted"
	ReasonIterationsFailed   = "IterationsFailed"

	ReasonDependencyCreationError = "DependencyCreationError"
	ReasonNodeDiscoveryError      = "NodeDiscoveryError"
	ReasonPartitionError          = "PartitionError"
)

// Job tier reasons (Job → Workload).
const (
	ReasonWorkloadCreated       = "WorkloadCreated"
	ReasonWorkloadRunning       = "WorkloadRunning"
	ReasonWorkloadCompleted     = "WorkloadCompleted"
	ReasonWorkloadFailed        = "WorkloadFailed"
	ReasonWorkloadCreationError = "WorkloadCreationError"
	ReasonWorkloadStalled       = "WorkloadStalled"

	ReasonHardwareFailureDetected = "HardwareFailureDetected"
)

// Framework type constants for WorkloadRun.
const (
	FrameworkTorch = "torch"
	FrameworkMPI   = "mpi"
	FrameworkExec  = "exec"
)

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
