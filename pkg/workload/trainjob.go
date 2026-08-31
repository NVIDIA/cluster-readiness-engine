// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const (
	// RuntimePatchManager is the manager key for NVCRE controller-owned RuntimePatches.
	RuntimePatchManager = "nvcre.nvidia.com/controller"

	LauncherJobName = "launcher"
	NodeJobName     = "node"
)

// TrainJobAdapter implements Adapter for Kubeflow TrainJob.
type TrainJobAdapter struct{}

var _ Adapter = &TrainJobAdapter{}

func (a *TrainJobAdapter) GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "trainer.kubeflow.org",
		Version: "v1alpha1",
		Kind:    "TrainJob",
	}
}

func (a *TrainJobAdapter) NewObject() client.Object {
	return &trainerv1alpha1.TrainJob{}
}

func (a *TrainJobAdapter) Build(name, namespace string, spec *nvcrev1alpha1.WorkloadSpec) (client.Object, error) {
	if spec.TrainJob == nil {
		return nil, fmt.Errorf("WorkloadSpec.TrainJob is nil")
	}

	trainJob := &trainerv1alpha1.TrainJob{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "trainer.kubeflow.org/v1alpha1",
			Kind:       "TrainJob",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: *spec.TrainJob,
	}
	return trainJob, nil
}

func (a *TrainJobAdapter) InjectPodLabel(spec *nvcrev1alpha1.WorkloadSpec, key, value string) {
	if spec.TrainJob == nil {
		return
	}

	targetName := workerTargetJob(spec.TrainJob.RuntimePatches)
	podTemplate := ensurePodTemplatePatch(spec.TrainJob, targetName)

	if podTemplate.Metadata == nil {
		podTemplate.Metadata = &metav1.ObjectMeta{}
	}

	if podTemplate.Metadata.Labels == nil {
		podTemplate.Metadata.Labels = make(map[string]string)
	}

	podTemplate.Metadata.Labels[key] = value
}

func (a *TrainJobAdapter) SetNodeSelector(spec *nvcrev1alpha1.WorkloadSpec, selector map[string]string) {
	if spec.TrainJob == nil {
		return
	}

	targetName := workerTargetJob(spec.TrainJob.RuntimePatches)
	podSpec := ensurePodSpecPatch(spec.TrainJob, targetName)

	if podSpec.NodeSelector == nil {
		podSpec.NodeSelector = make(map[string]string)
	}

	maps.Copy(podSpec.NodeSelector, selector)
}

func (a *TrainJobAdapter) SetNodeAffinity(spec *nvcrev1alpha1.WorkloadSpec, affinity *corev1.NodeAffinity) {
	if spec.TrainJob == nil {
		return
	}

	// Apply node affinity to ALL replicatedJobs (worker + launcher).
	// The launcher must schedule on GPU nodes for MPI network connectivity.
	for _, target := range allTargetJobs(spec.TrainJob.RuntimePatches) {
		podSpec := ensurePodSpecPatch(spec.TrainJob, target)
		if podSpec.Affinity == nil {
			podSpec.Affinity = &corev1.Affinity{}
		}
		podSpec.Affinity.NodeAffinity = affinity
	}
}

func (a *TrainJobAdapter) SetTolerations(spec *nvcrev1alpha1.WorkloadSpec, tolerations []corev1.Toleration) {
	if spec.TrainJob == nil {
		return
	}

	// Apply tolerations to ALL replicatedJobs (worker + launcher).
	// The launcher must also tolerate GPU node taints since it's
	// co-located with worker nodes for MPI connectivity.
	for _, target := range allTargetJobs(spec.TrainJob.RuntimePatches) {
		podSpec := ensurePodSpecPatch(spec.TrainJob, target)
		podSpec.Tolerations = append(podSpec.Tolerations, tolerations...)
	}
}

func (a *TrainJobAdapter) NodesRequired(spec *nvcrev1alpha1.WorkloadSpec) (int, error) {
	if spec.TrainJob == nil {
		return 0, fmt.Errorf("WorkloadSpec.TrainJob is nil")
	}
	if spec.TrainJob.Trainer == nil || spec.TrainJob.Trainer.NumNodes == nil {
		return 0, fmt.Errorf("TrainJob.Trainer.NumNodes not set")
	}
	return int(*spec.TrainJob.Trainer.NumNodes), nil
}

func (a *TrainJobAdapter) SetNumNodes(spec *nvcrev1alpha1.WorkloadSpec, numNodes int) {
	if spec.TrainJob == nil || spec.TrainJob.Trainer == nil {
		return
	}
	n := int32(numNodes)
	spec.TrainJob.Trainer.NumNodes = &n
}

func (a *TrainJobAdapter) GetStatus(obj client.Object) (*WorkloadStatus, error) {
	trainJob, ok := obj.(*trainerv1alpha1.TrainJob)
	if !ok {
		return nil, fmt.Errorf("expected *TrainJob, got %T", obj)
	}

	for _, cond := range trainJob.Status.Conditions {
		if cond.Type == trainerv1alpha1.TrainJobFailed && cond.Status == metav1.ConditionTrue {
			return &WorkloadStatus{
				Phase:   WorkloadFailed,
				Reason:  cond.Reason,
				Message: cond.Message,
			}, nil
		}
	}

	for _, cond := range trainJob.Status.Conditions {
		if cond.Type == trainerv1alpha1.TrainJobComplete && cond.Status == metav1.ConditionTrue {
			return &WorkloadStatus{
				Phase:   WorkloadSucceeded,
				Reason:  cond.Reason,
				Message: cond.Message,
			}, nil
		}
	}

	// A suspended TrainJob has no pods and typically no conditions — it is
	// queued, not running. Kueue's webhook creates TrainJobs with
	// spec.suspend=true and flips it only once quota is admitted, so falling
	// through to Running here would start stall/timeout clocks against
	// hardware that never ran anything. Terminal conditions are checked first
	// so a finished TrainJob that was suspended afterwards still reports its
	// true outcome.
	if trainJob.Spec.Suspend != nil && *trainJob.Spec.Suspend {
		message := "TrainJob is suspended (spec.suspend=true), waiting for it to be resumed"
		if trainJob.Spec.ManagedBy != nil && *trainJob.Spec.ManagedBy != "" {
			message += "; managed by " + *trainJob.Spec.ManagedBy
		}
		return &WorkloadStatus{
			Phase:   WorkloadPending,
			Reason:  trainerv1alpha1.TrainJobSuspendedReason,
			Message: message,
		}, nil
	}

	return &WorkloadStatus{Phase: WorkloadRunning}, nil
}

// workerTargetJob returns the replicated job name for the worker by inspecting
// existing runtimePatches. Falls back to "node" if no existing patch is found.
// The name must match the Kubeflow Trainer constants.Node value so that
// PET_MASTER_ADDR DNS resolves correctly.
func workerTargetJob(patches []trainerv1alpha1.RuntimePatch) string {
	for _, patch := range patches {
		if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
			patch.TrainingRuntimeSpec.Template.Spec == nil {
			continue
		}
		for _, rjob := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
			if rjob.Name != "" && rjob.Name != LauncherJobName {
				return rjob.Name
			}
		}
	}
	return "node"
}

// HasLauncherTarget reports whether the workload spec contains an MPI launcher
// target in its runtimePatches. Used by the workflow controller to decide
// whether to apply global tolerations (only MPI workloads need them).
func HasLauncherTarget(spec *nvcrev1alpha1.WorkloadSpec) bool {
	if spec.TrainJob == nil {
		return false
	}
	for _, patch := range spec.TrainJob.RuntimePatches {
		if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
			patch.TrainingRuntimeSpec.Template.Spec == nil {
			continue
		}
		for _, rjob := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
			if rjob.Name == LauncherJobName {
				return true
			}
		}
	}
	return false
}

// allTargetJobs returns the unique set of replicated job names from existing
// runtimePatches. If no targets are found, it defaults to ["node"].
// MPI catalog entries that need launcher tolerations must include an explicit
// runtimePatch targeting "launcher".
func allTargetJobs(patches []trainerv1alpha1.RuntimePatch) []string {
	seen := make(map[string]bool)
	var names []string
	for _, patch := range patches {
		if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
			patch.TrainingRuntimeSpec.Template.Spec == nil {
			continue
		}
		for _, rjob := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
			if rjob.Name != "" && !seen[rjob.Name] {
				seen[rjob.Name] = true
				names = append(names, rjob.Name)
			}
		}
	}
	if len(names) == 0 {
		return []string{"node"}
	}
	return names
}

func getOrCreateRuntimePatch(spec *trainerv1alpha1.TrainJobSpec) *trainerv1alpha1.RuntimePatch {
	for i := range spec.RuntimePatches {
		if spec.RuntimePatches[i].Manager == RuntimePatchManager {
			return &spec.RuntimePatches[i]
		}
	}
	spec.RuntimePatches = append(spec.RuntimePatches, trainerv1alpha1.RuntimePatch{
		Manager: RuntimePatchManager,
	})
	return &spec.RuntimePatches[len(spec.RuntimePatches)-1]
}

func ensureJobSetSpecPatch(spec *trainerv1alpha1.TrainJobSpec) *trainerv1alpha1.JobSetSpecPatch {
	patch := getOrCreateRuntimePatch(spec)
	if patch.TrainingRuntimeSpec == nil {
		patch.TrainingRuntimeSpec = &trainerv1alpha1.TrainingRuntimeSpecPatch{}
	}
	if patch.TrainingRuntimeSpec.Template == nil {
		patch.TrainingRuntimeSpec.Template = &trainerv1alpha1.JobSetTemplatePatch{}
	}
	if patch.TrainingRuntimeSpec.Template.Spec == nil {
		patch.TrainingRuntimeSpec.Template.Spec = &trainerv1alpha1.JobSetSpecPatch{}
	}
	return patch.TrainingRuntimeSpec.Template.Spec
}

func findReplicatedJobPatchIndex(jobs []trainerv1alpha1.ReplicatedJobPatch, name string) int {
	for i, job := range jobs {
		if job.Name == name {
			return i
		}
	}
	return -1
}

func getOrCreateReplicatedJobPatch(spec *trainerv1alpha1.TrainJobSpec, name string) *trainerv1alpha1.ReplicatedJobPatch {
	jobSetSpec := ensureJobSetSpecPatch(spec)
	idx := findReplicatedJobPatchIndex(jobSetSpec.ReplicatedJobs, name)
	if idx < 0 {
		jobSetSpec.ReplicatedJobs = append(jobSetSpec.ReplicatedJobs, trainerv1alpha1.ReplicatedJobPatch{
			Name: name,
		})
		idx = len(jobSetSpec.ReplicatedJobs) - 1
	}
	return &jobSetSpec.ReplicatedJobs[idx]
}

func ensurePodTemplatePatch(spec *trainerv1alpha1.TrainJobSpec, replicatedJobName string) *trainerv1alpha1.PodTemplatePatch {
	rjob := getOrCreateReplicatedJobPatch(spec, replicatedJobName)
	if rjob.Template == nil {
		rjob.Template = &trainerv1alpha1.JobTemplatePatch{}
	}
	if rjob.Template.Spec == nil {
		rjob.Template.Spec = &trainerv1alpha1.JobSpecPatch{}
	}
	if rjob.Template.Spec.Template == nil {
		rjob.Template.Spec.Template = &trainerv1alpha1.PodTemplatePatch{}
	}
	return rjob.Template.Spec.Template
}

func ensurePodSpecPatch(spec *trainerv1alpha1.TrainJobSpec, replicatedJobName string) *trainerv1alpha1.PodSpecPatch {
	podTemplate := ensurePodTemplatePatch(spec, replicatedJobName)
	if podTemplate.Spec == nil {
		podTemplate.Spec = &trainerv1alpha1.PodSpecPatch{}
	}
	return podTemplate.Spec
}

// EnsureLauncherTarget registers the worker and launcher replicated jobs in
// the controller-owned RuntimePatch. SetNodeAffinity and SetTolerations derive
// their targets from existing runtimePatches (allTargetJobs), and the Workflow
// controller's blanket MPI toleration is gated on HasLauncherTarget — so an
// MPI TrainJob without a launcher entry gets its launcher pod scheduled with
// no node pinning and no tolerations. Catalog MPI entries satisfy this
// contract with an explicit bare `- name: launcher` runtimePatch entry; MPI
// WorkloadRuns build their TrainJob programmatically and must register the
// launcher here. The bare entries are a no-op for the trainer's patch merge.
func EnsureLauncherTarget(trainJobSpec *trainerv1alpha1.TrainJobSpec) {
	if trainJobSpec == nil {
		return
	}
	getOrCreateReplicatedJobPatch(trainJobSpec, NodeJobName)
	getOrCreateReplicatedJobPatch(trainJobSpec, LauncherJobName)
}

// SetImagePullSecrets applies imagePullSecrets to the worker replicated job via RuntimePatches.
func SetImagePullSecrets(trainJobSpec *trainerv1alpha1.TrainJobSpec, secrets []corev1.LocalObjectReference) {
	if trainJobSpec == nil || len(secrets) == 0 {
		return
	}
	podSpec := ensurePodSpecPatch(trainJobSpec, NodeJobName)
	podSpec.ImagePullSecrets = secrets
}
