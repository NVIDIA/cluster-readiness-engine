// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Shared reason constants (all tiers) ---

const (
	// ReasonNotApplicable is the default condition reason for inactive conditions.
	ReasonNotApplicable = "NotApplicable"
)

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

// SetExclusiveConditions sets one condition True and all others in the list to False.
// This implements the mutually exclusive condition pattern used across all tiers
// (Certification, Workflow, Job, WorkloadRun).
func SetExclusiveConditions(
	conditions *[]metav1.Condition, allTypes []string,
	activeType, reason, message string, generation int64,
) bool {
	changed := false
	for _, ct := range allTypes {
		status := metav1.ConditionFalse
		condReason := ReasonNotApplicable
		condMessage := ""

		if ct == activeType {
			status = metav1.ConditionTrue
			condReason = reason
			condMessage = message
		}

		if meta.SetStatusCondition(conditions, metav1.Condition{
			Type:               ct,
			Status:             status,
			Reason:             condReason,
			Message:            condMessage,
			ObservedGeneration: generation,
		}) {
			changed = true
		}
	}
	return changed
}

// --- Shared GPU defaults ---

// DefaultEnableMNNVL returns whether MNNVL should be enabled for the given GPU
// architecture. GB200 and GB300 use Multi-Node NVLink and benefit from MNNVL.
func DefaultEnableMNNVL(gpuArch string) bool {
	return gpuArch == "gb200" || gpuArch == "gb300"
}
