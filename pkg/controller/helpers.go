// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"time"

	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
)

// --- Shared reason constants (all tiers) ---

const (
	// ReasonNotApplicable is the default condition reason for inactive conditions.
	ReasonNotApplicable = "NotApplicable"

	// ReasonNICResourceDetection is shared by the Certification and WorkloadRun
	// tiers: both emit a Normal event under it when NIC resource auto-detection
	// ran (on-prem GB200/GB300 or x86 HGX B200/B300 target with
	// nicResourceName unset) and found
	// zero or multiple qualifying candidates, so nothing was injected.
	ReasonNICResourceDetection = "NICResourceDetection"

	// ReasonGKENetworkDetection is the Certification tier's Warning event when
	// GKE TCPXO network auto-detection ran (GCP H100 target) and did not find
	// exactly the GPU NIC networks TCPXO needs on every target node, so the
	// catalog's default network names were rendered.
	ReasonGKENetworkDetection = "GKENetworkDetection"

	// ReasonTCPXOPluginDetection is the Certification tier's Warning event
	// when TCPXO plugin version detection ran (GCP H100 target) and did not
	// find one mapped plugin release on every target node, so the nearest
	// safe profile's GCP H100 workload and tcpxo-daemon images were rendered
	// (the latest mapped release for a newer tag, the minimum otherwise).
	ReasonTCPXOPluginDetection = "TCPXOPluginDetection"

	// ReasonGPUArchitectureDefaults is the Warning event both the Certification
	// and WorkloadRun tiers emit when the detected GPU architecture is not
	// listed in the catalog's gpu-defaults.yaml, so the workflow was sized
	// with the Go-side fallback (four GPUs, no NICs) instead of a known node
	// shape (ADR-094).
	ReasonGPUArchitectureDefaults = "GPUArchitectureDefaults"
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

	// ReasonInvalidResources marks a Certification whose resources the CRD
	// schema cannot reject on every supported API server: a request above its
	// limit, or a negative integer quantity (ADR-101). Set before any Workflow
	// is created, like ReasonCategoryNotFound.
	ReasonInvalidResources = "InvalidResources"

	// ReasonWorkflowNameCollision marks a Certification whose generated child
	// Workflow name is already taken by a Workflow this Certification does not
	// own. The foreign object is neither adopted nor recorded for cleanup.
	ReasonWorkflowNameCollision = "WorkflowNameCollision"

	// ReasonWorkflowCreationError indicates a child Workflow could not be
	// created. Emitted as a Warning event at the failed Create call by the
	// Certification and WorkloadRun reconcilers, which both create Workflows,
	// so the API server's rejection is visible on the parent object rather
	// than only in the controller log. Mirrors ReasonJobCreationError
	// (Workflow tier) and ReasonWorkloadCreationError (Job tier).
	ReasonWorkflowCreationError = "WorkflowCreationError"
)

// WorkloadRun tier reasons (WorkloadRun → Workflow).
const (
	// ReasonBuildFailedStatusUpdateFailed reports a WorkloadRun build error
	// when recording its Failed condition also fails. This action-failure
	// Warning does not claim that the Failed phase was persisted.
	ReasonBuildFailedStatusUpdateFailed = "BuildFailedStatusUpdateFailed"
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

	// ReasonJobSchedulingBlocked marks a running Workflow iteration in which
	// at least one group's Job reports WorkloadSchedulingBlocked or
	// WorkloadCreationRejected. The message relays that Job's diagnosis
	// (ADR-083, issue #488).
	ReasonJobSchedulingBlocked = "JobSchedulingBlocked"

	// ReasonJobTimedOut marks a Job's Failed condition set by the Workflow when
	// the Job exceeded timeoutPerJob. Timed-out jobs are never retried.
	ReasonJobTimedOut = "JobTimedOut"

	ReasonGroupsPartitioned  = "GroupsPartitioned"
	ReasonIterationCompleted = "IterationCompleted"
	ReasonIterationsFailed   = "IterationsFailed"

	ReasonDependencyCreationError = "DependencyCreationError"
	ReasonNodeDiscoveryError      = "NodeDiscoveryError"
	ReasonPartitionError          = "PartitionError"

	// ReasonHeterogeneousPlatformStatusUpdateFailed reports inconsistent node
	// platforms when recording the Workflow's Failed condition also fails.
	// This action-failure Warning does not claim a persisted Failed phase.
	ReasonHeterogeneousPlatformStatusUpdateFailed = "HeterogeneousPlatformStatusUpdateFailed"

	// ReasonOverrideErrorStatusUpdateFailed reports an override application
	// error when recording the Workflow's Failed condition also fails. Both
	// override guards use this action-failure Warning, not a phase notification.
	ReasonOverrideErrorStatusUpdateFailed = "OverrideErrorStatusUpdateFailed"

	// ReasonJobNameCollision marks a Workflow whose generated Job name is
	// already taken by a Job this Workflow does not control. The foreign
	// object is neither adopted nor recorded for cleanup.
	ReasonJobNameCollision = "JobNameCollision"
	// ReasonDependencyNameCollision marks a Workflow whose dependency resource
	// name is already taken by an object this Workflow did not create. The
	// foreign object is neither adopted nor recorded for cleanup.
	ReasonDependencyNameCollision = "DependencyNameCollision"
	// ReasonNamespacePermissionsMissing marks a Workflow waiting for the
	// controller's write access in its namespace: a SelfSubjectAccessReview
	// was denied, or a write was forbidden and a fresh review confirmed the
	// denial. Non-terminal until --namespace-access-timeout expires, when the
	// Workflow fails with the same reason and the RoleBinding to apply
	// (ADR-092).
	ReasonNamespacePermissionsMissing = "NamespacePermissionsMissing"
	// ReasonNamespaceAccessCheckFailed marks a Workflow whose access check
	// could not run because a SelfSubjectAccessReview could not be created.
	// An unanswered check is never treated as permission, so the Workflow
	// waits under the same bound as ReasonNamespacePermissionsMissing.
	ReasonNamespaceAccessCheckFailed = "NamespaceAccessCheckFailed"
	// ReasonNamespaceAccessGranted is the NamespaceAccess condition's reason
	// once a Workflow that waited has its access.
	ReasonNamespaceAccessGranted = "NamespaceAccessGranted"
	// ReasonNamespaceAccessDenied and ReasonNamespaceAccessReviewError are the
	// Warning events emitted when a Workflow starts waiting. They differ from
	// the InProgress reasons on purpose: a Warning sharing the transition's
	// reason would aggregate with it into one row (ADR-080 decision 5), and a
	// wait that begins mid-run makes no transition at all.
	ReasonNamespaceAccessDenied      = "NamespaceAccessDenied"
	ReasonNamespaceAccessReviewError = "NamespaceAccessReviewError"
	// ReasonNodeResultsWriteRejected is the Warning event emitted when a
	// node-result ConfigMap write is rejected although the controller has
	// access, by an admission policy or ResourceQuota. The Workflow goes on
	// without the record, as before ADR-092, but no longer only in the log.
	ReasonNodeResultsWriteRejected = "NodeResultsWriteRejected"

	// ReasonGangSchedulingConflict marks a Workflow whose resolved Job template
	// is invalid after overrides. This includes workload metadata that fails
	// label validation even when no gang scheduler is configured, as well as a
	// template that no longer agrees with the gang-scheduling intent its owner
	// persisted. The invalid result is reported rather than repaired.
	ReasonGangSchedulingConflict = "GangSchedulingConflict"
)

// Job tier reasons (Job → Workload).
const (
	ReasonWorkloadCreated = "WorkloadCreated"
	// ReasonWorkloadAdopted indicates the Job found the workload it had
	// already created still on the API server, because the write that records
	// status.WorkloadRef was lost after the Create succeeded, and took the
	// reference back rather than treating the name as taken.
	ReasonWorkloadAdopted = "WorkloadAdopted"
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

	// ReasonWorkloadSchedulingBlocked indicates the workload is unsuspended
	// and admitted, but at least one of its pods cannot be scheduled
	// (PodScheduled=False/Unschedulable). The Job stays InProgress and the
	// blocked time does not count against timeoutPerJob or stall detection:
	// the same clock-neutral treatment as WorkloadPending (issue #213).
	// See ADR-083.
	ReasonWorkloadSchedulingBlocked = "WorkloadSchedulingBlocked"

	// ReasonWorkloadCreationRejected indicates the API server is rejecting
	// the workload create (admission webhook or policy denial, exhausted
	// ResourceQuota). The Job stays InProgress and the controller keeps
	// retrying, since some rejections clear (a TrainingRuntime that is not
	// ready yet, quota that frees up). A rejection that outlasts the
	// workload creation deadline fails the Job with
	// ReasonWorkloadCreationError. See issue #488.
	ReasonWorkloadCreationRejected = "WorkloadCreationRejected"

	// ReasonMeasurementCreationError indicates a GoodputMeasurement or
	// BandwidthMeasurement child resource could not be created. Handling is
	// non-fatal, so this event is the operator-visible signal; a threshold that
	// depends on the missing measurement still fails closed via
	// ReasonMeasurementTimeout.
	ReasonMeasurementCreationError = "MeasurementCreationError"

	ReasonHardwareFailureDetected = "HardwareFailureDetected"
)

// defaultNodeHealthFailureMessage is the per-node message recorded when the node
// health detector flags a node but supplies no message of its own. A detector
// that does supply one has it kept verbatim (ADR-099).
const defaultNodeHealthFailureMessage = "Hardware failure detected by node health check"

// Framework type constants for WorkloadRun, shared with pkg/platform so the
// builders and ValidateWorkloadVolumes read the names this package selects by.
const (
	FrameworkTorch = platform.FrameworkTorch
	FrameworkMPI   = platform.FrameworkMPI
	FrameworkExec  = platform.FrameworkExec
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

// jobCreateError marks a failure to create (or adopt) a group's Job, as
// opposed to one of its job-scoped dependencies, so a rejection is reported
// with the reason that names what was rejected.
type jobCreateError struct {
	err error
}

func (e *jobCreateError) Error() string { return e.err.Error() }

func (e *jobCreateError) Unwrap() error { return e.err }

// gangSchedulingConflictError reports that a resolved Workflow no longer
// agrees with the gang-scheduling intent its owner persisted. It is terminal:
// both the intent and a created Job's workload metadata are immutable, so no
// retry can reconcile the two, and a plain error would leave the Workflow
// retrying at InProgress with nothing naming the cause.
type gangSchedulingConflictError struct {
	err error
}

func (e *gangSchedulingConflictError) Error() string { return e.err.Error() }

func (e *gangSchedulingConflictError) Unwrap() error { return e.err }

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
