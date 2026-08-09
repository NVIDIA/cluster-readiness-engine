// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

// WorkloadRunReconciler reconciles a WorkloadRun object.
type WorkloadRunReconciler struct {
	client.Client
	Scheme *kruntime.Scheme
}

// +kubebuilder:rbac:groups=cre.nvidia.com,resources=workloadruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=workloadruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=workloadruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=cre.nvidia.com,resources=workflows,verbs=get;list;watch;create;update;patch;delete

const workloadRunRequeueInterval = 15 * time.Second

// Shared reason constants are in helpers.go (ReasonWorkflowCreated, etc.).

func (r *WorkloadRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var run burninv1alpha1.WorkloadRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// If terminal, nothing to do.
	if condIsTrue(run.Status.Conditions, burninv1alpha1.WorkloadRunSucceeded) ||
		condIsTrue(run.Status.Conditions, burninv1alpha1.WorkloadRunFailed) {
		return ctrl.Result{}, nil
	}

	// If Workflow exists, mirror its status.
	if run.Status.WorkflowRef != nil {
		return r.mirrorWorkflowStatus(ctx, &run)
	}

	// Build and create Workflow.
	log.Info("Building Workflow for WorkloadRun")

	workflowSpec := r.buildWorkflowSpec(ctx, &run)

	workflow := &burninv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cluster-readiness-engine",
				"cre.nvidia.com/workload-run":  run.Name,
			},
		},
		Spec: *workflowSpec,
	}

	if err := controllerutil.SetControllerReference(&run, workflow, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
	}

	if err := r.Create(ctx, workflow); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{RequeueAfter: workloadRunRequeueInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("creating Workflow: %w", err)
	}

	// Update status with workflow ref.
	run.Status.WorkflowRef = &burninv1alpha1.WorkflowReference{
		Name:      workflow.Name,
		Namespace: workflow.Namespace,
	}
	r.setWorkloadRunCondition(&run, burninv1alpha1.WorkloadRunInProgress, ReasonWorkflowCreated,
		fmt.Sprintf("Workflow %s created", workflow.Name))
	if err := r.Status().Update(ctx, &run); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Created Workflow", "workflow", workflow.Name)
	return ctrl.Result{RequeueAfter: workloadRunRequeueInterval}, nil
}

// mirrorWorkflowStatus copies the Workflow's terminal conditions to the WorkloadRun.
func (r *WorkloadRunReconciler) mirrorWorkflowStatus(ctx context.Context, run *burninv1alpha1.WorkloadRun) (ctrl.Result, error) {
	var workflow burninv1alpha1.Workflow
	key := client.ObjectKey{Name: run.Status.WorkflowRef.Name, Namespace: run.Namespace}
	if err := r.Get(ctx, key, &workflow); err != nil {
		if apierrors.IsNotFound(err) {
			r.setWorkloadRunCondition(run, burninv1alpha1.WorkloadRunFailed, ReasonWorkflowDeleted, "Workflow was deleted")
			return ctrl.Result{}, r.Status().Update(ctx, run)
		}
		return ctrl.Result{}, err
	}

	// Mirror detected platform/GPU from orchestration status.
	if workflow.Status.Orchestration != nil {
		run.Status.DetectedPlatform = workflow.Status.Orchestration.DetectedPlatform
		run.Status.DetectedGPUArchitecture = workflow.Status.Orchestration.DetectedGPUArchitecture
	}

	// Mirror the succeeded-nodes and failed-nodes ConfigMap references.
	run.Status.SucceededNodesRef = workflow.Status.SucceededNodesRef
	run.Status.FailedNodesRef = workflow.Status.FailedNodesRef

	// Mirror terminal conditions.
	if condIsTrue(workflow.Status.Conditions, burninv1alpha1.WorkflowSucceeded) {
		r.setWorkloadRunCondition(run, burninv1alpha1.WorkloadRunSucceeded, ReasonWorkflowSucceeded, "Workflow completed successfully")
		return ctrl.Result{}, r.Status().Update(ctx, run)
	}
	if condIsTrue(workflow.Status.Conditions, burninv1alpha1.WorkflowFailed) {
		msg := condMsg(workflow.Status.Conditions, burninv1alpha1.WorkflowFailed)
		r.setWorkloadRunCondition(run, burninv1alpha1.WorkloadRunFailed, ReasonWorkflowFailed, msg)
		return ctrl.Result{}, r.Status().Update(ctx, run)
	}

	// Mirror validation failed (independent condition).
	if condIsTrue(workflow.Status.Conditions, burninv1alpha1.WorkflowValidationFailed) {
		msg := condMsg(workflow.Status.Conditions, burninv1alpha1.WorkflowValidationFailed)
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:    burninv1alpha1.WorkloadRunValidationFailed,
			Status:  metav1.ConditionTrue,
			Reason:  ReasonThresholdViolation,
			Message: msg,
		})
	}

	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: workloadRunRequeueInterval}, nil
}

// setWorkloadRunCondition sets a condition with mutual exclusivity for execution conditions.
func (r *WorkloadRunReconciler) setWorkloadRunCondition(run *burninv1alpha1.WorkloadRun, condType, reason, message string) {
	executionTypes := []string{
		burninv1alpha1.WorkloadRunInProgress,
		burninv1alpha1.WorkloadRunSucceeded,
		burninv1alpha1.WorkloadRunFailed,
	}
	for _, t := range executionTypes {
		status := metav1.ConditionFalse
		r2, msg := "Superseded", ""
		if t == condType {
			status = metav1.ConditionTrue
			r2 = reason
			msg = message
		}
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:    t,
			Status:  status,
			Reason:  r2,
			Message: msg,
		})
	}
}

// buildWorkflowSpec translates a WorkloadRunSpec into a WorkflowSpec.
func (r *WorkloadRunReconciler) buildWorkflowSpec(ctx context.Context, run *burninv1alpha1.WorkloadRun) *burninv1alpha1.WorkflowSpec {
	spec := &run.Spec

	// Best-effort node discovery for GPU + platform defaults. The Workflow
	// controller does its own authoritative discovery and will fail if no
	// nodes match.
	gpusPerNode := catalog.GPUDefaults("", "").GpusPerNode
	mlnxPerNode := int32(0)
	enableMNNVL := false
	detectedPlatform := ""
	gpuArch := ""
	// Cordoned nodes are discarded here: this call only detects GPU and platform
	// defaults, and a WorkloadRun has no coverage verdict to qualify.
	nodes, _, _ := discoverTargetNodes(ctx, r.Client, spec.Target)
	if len(nodes) > 0 {
		gpuArch = DetectGPUArchitecture(nodes)
		detectedPlatform = DetectPlatform(nodes)
		nd := catalog.GPUDefaults(gpuArch, detectedPlatform)
		gpusPerNode = nd.GpusPerNode
		mlnxPerNode = nd.MlnxPerNode
		enableMNNVL = DefaultEnableMNNVL(gpuArch)
	}

	if spec.GpusPerNode != nil {
		gpusPerNode = *spec.GpusPerNode
	}
	if spec.MlnxPerNode != nil {
		mlnxPerNode = *spec.MlnxPerNode
	}
	if spec.EnableMNNVL != nil {
		enableMNNVL = *spec.EnableMNNVL
	}

	// Determine framework type.
	frameworkType := FrameworkExec
	if spec.Framework.Torch != nil {
		frameworkType = "torch"
	} else if spec.Framework.MPI != nil {
		frameworkType = "mpi"
	}

	// Build merged env vars.
	baseEnv := platform.BaseNCCLEnvVars(enableMNNVL)
	mergedEnv := platform.MergeEnvVars(baseEnv, spec.Env)

	// Collect volumes and mounts, injecting config volume if needed.
	volumes := append([]corev1.Volume{}, spec.Volumes...)
	volumeMounts := append([]corev1.VolumeMount{}, spec.VolumeMounts...)
	if spec.Config != nil {
		configMapName := fmt.Sprintf("%s-config", run.Name)
		if spec.Config.ConfigMapRef != nil {
			configMapName = spec.Config.ConfigMapRef.Name
		}
		volumes = append(volumes, corev1.Volume{
			Name: "config-volume",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					DefaultMode:          func() *int32 { m := int32(0755); return &m }(),
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "config-volume",
			MountPath: "/config",
		})
	}

	// Build runtime dependency.
	rtCfg := platform.RuntimeConfig{
		EntryName:        run.Name,
		Image:            spec.Image,
		NodesPerJob:      spec.NumNodes,
		GpusPerNode:      gpusPerNode,
		Env:              mergedEnv,
		Volumes:          volumes,
		VolumeMounts:     volumeMounts,
		InitContainers:   spec.InitContainers,
		Resources:        spec.Resources,
		ImagePullSecrets: spec.ImagePullSecrets,
	}
	if spec.GangScheduler != nil {
		rtCfg.GangSchedulerName = spec.GangScheduler.SchedulerName
		rtCfg.GangSchedulerQueue = spec.GangScheduler.Queue
	}

	var runtimeDep burninv1alpha1.DependencySpec
	switch frameworkType {
	case FrameworkTorch:
		runtimeDep = platform.BuildTorchRuntime(rtCfg)
	case FrameworkMPI:
		runtimeDep = platform.BuildMPIRuntime(rtCfg)
	default:
		runtimeDep = platform.BuildExecRuntime(rtCfg)
	}

	// Build dependencies list.
	deps := []burninv1alpha1.DependencySpec{runtimeDep}

	// ConfigMap dependency (if inline config provided).
	if spec.Config != nil && len(spec.Config.Inline) > 0 {
		deps = append(deps, buildWRConfigMapDep(run.Name, spec.Config.Inline))
	}

	// PVC dependency (if checkpoint enabled).
	if spec.Checkpoint != nil {
		deps = append(deps, buildWRPVCDep(run.Name, spec.Checkpoint))
	}

	// Build OrchestrationSpec.
	orch := buildWROrchestration(spec)

	// Build platform overrides.
	overrideCfg := platform.OverrideConfig{
		EntryName:     run.Name,
		NodesPerJob:   spec.NumNodes,
		GpusPerNode:   gpusPerNode,
		MlnxPerNode:   mlnxPerNode,
		EnableMNNVL:   enableMNNVL,
		FrameworkType: frameworkType,
	}
	wrOverrides := platform.BuildOverrides(overrideCfg)
	octx := OverrideContext{
		Platform:        detectedPlatform,
		GPUArchitecture: gpuArch,
	}

	// Pre-template overrides: modify the WorkloadRun spec before the job
	// template is built so that changes are baked in at construction time.
	applyWRPreTemplateOverrides(spec, wrOverrides, octx)

	// Build JobTemplate.
	jobTemplate := r.buildJobTemplate(run, frameworkType, gpusPerNode)

	// Post-template overrides: modify the built job template before the
	// Workflow CR is created so changes are stored in well-known fields
	// rather than relying on the CRD schema to preserve override subfields.
	applyWRPostTemplateOverrides(jobTemplate, wrOverrides, octx)

	// Extract the base OverrideSpec slice for the WorkflowSpec and append
	// user's custom overrides.
	overrides := make([]burninv1alpha1.OverrideSpec, 0, len(wrOverrides)+len(spec.Overrides))
	for _, o := range wrOverrides {
		overrides = append(overrides, o.OverrideSpec)
	}
	overrides = append(overrides, spec.Overrides...)

	workflowSpec := &burninv1alpha1.WorkflowSpec{
		JobTemplate:   *jobTemplate,
		Orchestration: *orch,
		Dependencies:  deps,
		Overrides:     overrides,
	}

	// Build validation spec.
	if len(spec.Thresholds) > 0 {
		workflowSpec.Validation = &burninv1alpha1.ValidationSpec{
			Performance: &burninv1alpha1.PerformanceValidationSpec{
				Enabled: true,
				Thresholds: &burninv1alpha1.ThresholdSpec{
					Thresholds: spec.Thresholds,
				},
			},
		}
	}

	return workflowSpec
}

// buildJobTemplate constructs the JobTemplateSpec for the workload.
func (r *WorkloadRunReconciler) buildJobTemplate(run *burninv1alpha1.WorkloadRun, frameworkType string, gpusPerNode int32) *burninv1alpha1.JobTemplateSpec {
	spec := &run.Spec

	// Build trainer spec based on framework.
	var command []string
	var args []string

	switch frameworkType {
	case FrameworkTorch:
		torch := spec.Framework.Torch
		if torch.Module != "" {
			command = []string{"torchrun"}
			args = append([]string{"-m", torch.Module}, torch.Args...)
		} else {
			command = []string{"torchrun"}
			args = append([]string{torch.Script}, torch.Args...)
		}
	case FrameworkMPI:
		mpi := spec.Framework.MPI
		command = []string{"timeout", "3600", mpi.MpirunPath}
		baseCount := 10 // fixed args below
		mpiArgs := make([]string, 0, baseCount+len(mpi.MpiArgs)+1+len(mpi.Args))
		mpiArgs = append(mpiArgs,
			"-N", fmt.Sprintf("%d", gpusPerNode),
			"--allow-run-as-root",
			"--mca", "plm_rsh_args",
			"-o StrictHostKeyChecking=no -o ConnectionAttempts=10",
		)
		mpiArgs = append(mpiArgs, "-x", "NCCL_DEBUG=INFO")
		enableStr := "0"
		if spec.EnableMNNVL != nil && *spec.EnableMNNVL {
			enableStr = "1"
		}
		mpiArgs = append(mpiArgs, "-x", fmt.Sprintf("NCCL_MNNVL_ENABLE=%s", enableStr))
		mpiArgs = append(mpiArgs, mpi.MpiArgs...)
		mpiArgs = append(mpiArgs, mpi.Binary)
		mpiArgs = append(mpiArgs, mpi.Args...)
		args = mpiArgs
	default: // exec
		exec := spec.Framework.Exec
		command = exec.Command
		args = exec.Args
	}

	// Build the TrainJobSpec.
	trainJobSpec := &trainerv1alpha1.TrainJobSpec{
		RuntimeRef: trainerv1alpha1.RuntimeRef{
			Name: fmt.Sprintf("%s-runtime", run.Name),
			Kind: func() *string { s := "TrainingRuntime"; return &s }(),
		},
		Trainer: &trainerv1alpha1.Trainer{
			Image:          &spec.Image,
			Command:        command,
			Args:           args,
			NumNodes:       &spec.NumNodes,
			NumProcPerNode: &gpusPerNode,
		},
	}

	workload.SetImagePullSecrets(trainJobSpec, spec.ImagePullSecrets)

	jobSpec := burninv1alpha1.JobSpec{
		Workload: burninv1alpha1.WorkloadSpec{
			TrainJob: trainJobSpec,
		},
		NodeHealthMonitor: &burninv1alpha1.NodeHealthMonitor{
			CEL: &burninv1alpha1.CELNodeHealthCheck{
				Expression: "node.spec.unschedulable == true",
			},
		},
	}

	if spec.GoodputMeasurement != nil {
		jobSpec.GoodputMeasurement = spec.GoodputMeasurement
	}
	if spec.BandwidthMeasurement != nil {
		jobSpec.BandwidthMeasurement = spec.BandwidthMeasurement
	}

	if spec.Checkpoint != nil {
		pvcName := fmt.Sprintf("%s-checkpoint-pvc", run.Name)
		maxRestarts := int32(0)
		if spec.Checkpoint.MaxRestarts != nil {
			maxRestarts = *spec.Checkpoint.MaxRestarts
		}
		jobSpec.Checkpoint = &burninv1alpha1.CheckpointConfig{
			PVCName:     pvcName,
			MaxRestarts: &maxRestarts,
		}
	}

	return &burninv1alpha1.JobTemplateSpec{
		Spec: jobSpec,
	}
}

// buildWRConfigMapDep creates a ConfigMap dependency from inline config data.
func buildWRConfigMapDep(name string, data map[string]string) burninv1alpha1.DependencySpec {
	cm := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":   fmt.Sprintf("%s-config", name),
			"labels": map[string]any{"app": name},
		},
		"data": data,
	}
	raw, _ := json.Marshal(cm)
	return burninv1alpha1.DependencySpec{RawExtension: kruntime.RawExtension{Raw: raw}}
}

// buildWRPVCDep creates a PVC dependency for checkpoint storage.
func buildWRPVCDep(name string, checkpoint *burninv1alpha1.WorkloadRunCheckpoint) burninv1alpha1.DependencySpec {
	pvc := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":   fmt.Sprintf("%s-checkpoint-pvc", name),
			"labels": map[string]any{"app": name},
		},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteMany"},
			"resources":   map[string]any{"requests": map[string]any{"storage": checkpoint.StorageSize}},
		},
	}
	if checkpoint.StorageClassName != nil {
		pvc["spec"].(map[string]any)["storageClassName"] = *checkpoint.StorageClassName
	}
	raw, _ := json.Marshal(pvc)
	return burninv1alpha1.DependencySpec{RawExtension: kruntime.RawExtension{Raw: raw}}
}

// buildWROrchestration constructs the OrchestrationSpec from WorkloadRunSpec.
func buildWROrchestration(spec *burninv1alpha1.WorkloadRunSpec) *burninv1alpha1.OrchestrationSpec {
	orch := &burninv1alpha1.OrchestrationSpec{
		Target:     spec.Target,
		Iterations: 1,
	}
	if spec.Orchestration != nil {
		if spec.Orchestration.RepeatCount != nil {
			orch.Iterations = int(*spec.Orchestration.RepeatCount)
		}
		switch spec.Orchestration.TestScale {
		case burninv1alpha1.TestScaleIntraNode:
			// Handled by nodesPerJobForScale when the workload is built:
			// one node per Job, so the Workflow makes one group per node.
			// Nothing to set on the orchestration itself.
		case "intra-rack":
			// TopologyKey is set by platform override (workloadrun.yaml)
			// to the platform's physical rack label.
			orch.Topology = &burninv1alpha1.TopologySpec{
				StrictDomain: true,
			}
		}
		exec := burninv1alpha1.ExecutionSpec{}
		if spec.Orchestration.MaxConcurrent != nil {
			exec.MaxConcurrent = int(*spec.Orchestration.MaxConcurrent)
		}
		if spec.Orchestration.TimeoutPerJob != "" {
			d, parseErr := time.ParseDuration(spec.Orchestration.TimeoutPerJob)
			if parseErr == nil {
				exec.TimeoutPerJob = &metav1.Duration{Duration: d}
			}
		}
		orch.Execution = exec
	}
	return orch
}

// condIsTrue checks if a condition type is True.
func condIsTrue(conditions []metav1.Condition, condType string) bool {
	c := meta.FindStatusCondition(conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

// condMsg returns the message for a condition type.
func condMsg(conditions []metav1.Condition, condType string) string {
	c := meta.FindStatusCondition(conditions, condType)
	if c != nil {
		return c.Message
	}
	return ""
}

func (r *WorkloadRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&burninv1alpha1.WorkloadRun{}).
		Owns(&burninv1alpha1.Workflow{}).
		Complete(r)
}

// applyWRPreTemplateOverrides applies overrides that must modify the WorkloadRun
// spec before buildJobTemplate is called. Results are baked into the spec fields
// consumed by buildJobTemplate (e.g. MPI.MpiArgs).
func applyWRPreTemplateOverrides(spec *burninv1alpha1.WorkloadRunSpec, overrides []platform.WorkloadRunOverride, octx OverrideContext) {
	for _, o := range overrides {
		matches, err := matchesWhen(o.When, octx)
		if err != nil || !matches {
			continue
		}
		if len(o.MPIArgs) > 0 && spec.Framework.MPI != nil {
			spec.Framework.MPI.MpiArgs = append(o.MPIArgs, spec.Framework.MPI.MpiArgs...)
		}
	}
}

// applyWRPostTemplateOverrides applies overrides that modify the already-built
// JobTemplate. Results are stored in well-known trainer fields so they are
// preserved in the Workflow CR without requiring new CRD schema fields.
func applyWRPostTemplateOverrides(jt *burninv1alpha1.JobTemplateSpec, overrides []platform.WorkloadRunOverride, octx OverrideContext) {
	var preCommands []string
	for _, o := range overrides {
		matches, err := matchesWhen(o.When, octx)
		if err != nil || !matches {
			continue
		}
		preCommands = append(preCommands, o.PreCommand...)
	}
	applyPreCommands(jt, preCommands)
}
