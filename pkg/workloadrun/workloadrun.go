// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigyaml "sigs.k8s.io/yaml"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/cluster"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/controller"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/gpu"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/noderesults"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/platform"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/render"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/report"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/setup"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/workload"
)

const (
	outputJSON   = "json"
	defaultNS    = "default"
	statusFailed = "Failed"
)

// nodesPerJobForScale returns how many nodes a single Job should span.
// intra-node means each node is tested on its own, so one node per Job however
// many the run targets; the Workflow then makes one group per node. Anything
// else keeps the requested count.
func nodesPerJobForScale(orch *crev1alpha1.WorkloadOrchestration, numNodes int32) int32 {
	if orch != nil && orch.TestScale == crev1alpha1.TestScaleIntraNode {
		return 1
	}
	return numNodes
}

// resolveWRTimeout turns a user-supplied timeoutPerJob into a duration, falling
// back to the WorkloadRun default. An unparseable value falls back too rather
// than leaving the Job unbounded, which is what used to happen silently.
func resolveWRTimeout(v string) *metav1.Duration {
	if v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return &metav1.Duration{Duration: d}
		}
	}
	d, err := time.ParseDuration(catalog.DefaultWorkloadRunTimeoutPerJob)
	if err != nil {
		return nil
	}
	return &metav1.Duration{Duration: d}
}

// NewCommand returns the "workloadrun" cobra command.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workloadrun",
		Short: "WorkloadRun management commands",
	}
	cmd.AddCommand(newWorkloadRunRenderCommand())
	cmd.AddCommand(newWorkloadRunRunCommand())
	cmd.AddCommand(newWorkloadRunReportCommand())
	cmd.AddCommand(newWorkloadRunStatusCommand())
	cmd.AddCommand(newWorkloadRunCancelCommand())
	return cmd
}

// --- render ---

func newWorkloadRunRenderCommand() *cobra.Command {
	var outputFormat string
	var platformFlag string
	var dryRun bool

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "render [flags] <workloadrun.yaml>",
		Short: "Render the Workflow that would be created from a WorkloadRun",
		Long: `Reads a WorkloadRun YAML and renders the Workflow that the controller would create,
including auto-generated TrainingRuntime, ConfigMap, platform overrides, and NCCL env vars.

Use --platform to simulate platform-specific overrides offline.
Use --dry-run to discover real nodes from the cluster and apply overrides based on actual platform and GPU.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if dryRun {
				return runWorkloadRunRenderDryRun(args[0], outputFormat, configFlags)
			}
			return runWorkloadRunRender(args[0], outputFormat, platformFlag)
		},
	}

	cmd.Flags().StringVar(&outputFormat, "output", "yaml", "Output format: yaml or json")
	cmd.Flags().StringVar(&platformFlag, "platform", "",
		"Simulate platform for override matching ("+platform.NamesList()+")")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Connect to cluster, discover real nodes, and render with actual platform/GPU detection")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

func runWorkloadRunRender(file, outputFormat, platformFlag string) error {
	if err := platform.ValidateFlag(platformFlag); err != nil {
		return err
	}

	run, err := readWorkloadRun(file)
	if err != nil {
		return err
	}

	// Extract GPU architecture from nodeSelector.
	var gpuProduct string
	if run.Spec.Target != nil {
		gpuProduct = run.Spec.Target.NodeSelector["nvidia.com/gpu.product"]
	}
	gpuArch := gpu.ParseProduct(gpuProduct)
	if gpuArch == "" {
		return fmt.Errorf("cannot determine GPU architecture: nvidia.com/gpu.product label required in target.nodeSelector")
	}

	// Resolve hardware defaults from the catalog. Platform comes from --platform
	// (used later for override matching); when absent, only architecture defaults
	// apply at template-render time.
	nd := catalog.GPUDefaults(gpuArch, platformFlag)
	gpusPerNode := nd.GpusPerNode
	mlnxPerNode := nd.MlnxPerNode
	if run.Spec.GpusPerNode != nil {
		gpusPerNode = *run.Spec.GpusPerNode
	}
	if run.Spec.MlnxPerNode != nil {
		mlnxPerNode = *run.Spec.MlnxPerNode
	}

	enableMNNVL := controller.DefaultEnableMNNVL(gpuArch)
	if run.Spec.EnableMNNVL != nil {
		enableMNNVL = *run.Spec.EnableMNNVL
	}

	// Determine framework type.
	frameworkType := controller.FrameworkExec
	if run.Spec.Framework.Torch != nil {
		frameworkType = controller.FrameworkTorch
	} else if run.Spec.Framework.MPI != nil {
		frameworkType = controller.FrameworkMPI
	}
	if err := validateExecFramework(&run.Spec, run.Name); err != nil {
		return err
	}

	// Build WorkflowSpec.
	workflowSpec := BuildWorkflowSpec(run, gpusPerNode, mlnxPerNode, enableMNNVL, frameworkType)

	// If platform specified, apply overrides using synthetic nodes.
	if platformFlag != "" {
		nodes := loadSyntheticNodes(platformFlag, gpuArch)
		orch := &crev1alpha1.OrchestrationStatus{
			DetectedPlatform:        platformFlag,
			DetectedGPUArchitecture: gpuArch,
		}
		octx := controller.BuildOverrideContext(workflowSpec, orch, nodes)
		if _, overrideErr := controller.ApplyOverridesWithTracking(workflowSpec, octx); overrideErr != nil {
			return fmt.Errorf("applying overrides: %w", overrideErr)
		}
		workflowSpec.Overrides = nil
	}

	// Build output Workflow.
	workflow := &crev1alpha1.Workflow{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cre.nvidia.com/v1alpha1",
			Kind:       "Workflow",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cluster-readiness-engine",
				"cre.nvidia.com/workload-run":  run.Name,
			},
			Annotations: map[string]string{
				"ncrectl.nvidia.com/detected-gpu-architecture": gpuArch,
			},
		},
		Spec: *workflowSpec,
	}

	if platformFlag != "" {
		workflow.Annotations["ncrectl.nvidia.com/detected-platform"] = platformFlag
	}

	switch outputFormat {
	case outputJSON:
		data, _ := json.MarshalIndent(workflow, "", "  ")
		fmt.Println(string(data))
	default:
		data, _ := sigyaml.Marshal(workflow)
		fmt.Print(string(data))
	}
	return nil
}

// BuildWorkflowSpec constructs a WorkflowSpec from a WorkloadRun
// (shared logic between controller and CLI).
func BuildWorkflowSpec(
	run *crev1alpha1.WorkloadRun,
	gpusPerNode, mlnxPerNode int32, enableMNNVL bool, frameworkType string,
) *crev1alpha1.WorkflowSpec {
	spec := &run.Spec

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
		defaultMode := int32(0755)
		volumes = append(volumes, corev1.Volume{
			Name: "config-volume",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					DefaultMode:          &defaultMode,
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
		NodesPerJob:      nodesPerJobForScale(spec.Orchestration, spec.NumNodes),
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

	var runtimeDep crev1alpha1.DependencySpec
	switch frameworkType {
	case controller.FrameworkTorch:
		runtimeDep = platform.BuildTorchRuntime(rtCfg)
	case controller.FrameworkMPI:
		runtimeDep = platform.BuildMPIRuntime(rtCfg)
	default:
		runtimeDep = platform.BuildExecRuntime(rtCfg)
	}

	deps := []crev1alpha1.DependencySpec{runtimeDep}

	if spec.Config != nil && len(spec.Config.Inline) > 0 {
		deps = append(deps, buildWRCLIConfigMapDep(run.Name, spec.Config.Inline))
	}

	// Build JobTemplate.
	jobTemplate := buildCLIJobTemplate(run, frameworkType, gpusPerNode, enableMNNVL)

	// Build OrchestrationSpec.
	orch := &crev1alpha1.OrchestrationSpec{
		Target:     spec.Target,
		Iterations: 1,
	}
	if spec.Orchestration != nil {
		if spec.Orchestration.RepeatCount != nil {
			orch.Iterations = int(*spec.Orchestration.RepeatCount)
		}
		switch spec.Orchestration.TestScale {
		case "intra-rack":
			// TopologyKey is set by platform override (workloadrun.yaml)
			// to the platform's physical rack label.
			orch.Topology = &crev1alpha1.TopologySpec{
				StrictDomain: true,
			}
		}
		exec := crev1alpha1.ExecutionSpec{}
		if spec.Orchestration.MaxConcurrent != nil {
			exec.MaxConcurrent = int(*spec.Orchestration.MaxConcurrent)
		}
		exec.TimeoutPerJob = resolveWRTimeout(spec.Orchestration.TimeoutPerJob)
		orch.Execution = exec
	}
	// A WorkloadRun with no orchestration block still needs a bound.
	if orch.Execution.TimeoutPerJob == nil {
		orch.Execution.TimeoutPerJob = resolveWRTimeout("")
	}

	// Build platform overrides.
	overrideCfg := platform.OverrideConfig{
		EntryName:     run.Name,
		NodesPerJob:   nodesPerJobForScale(spec.Orchestration, spec.NumNodes),
		GpusPerNode:   gpusPerNode,
		MlnxPerNode:   mlnxPerNode,
		EnableMNNVL:   enableMNNVL,
		FrameworkType: frameworkType,
	}
	wrOverrides := platform.BuildOverrides(overrideCfg)
	overrides := make([]crev1alpha1.OverrideSpec, 0, len(wrOverrides)+len(spec.Overrides))
	for _, o := range wrOverrides {
		overrides = append(overrides, o.OverrideSpec)
	}
	overrides = append(overrides, spec.Overrides...)

	workflowSpec := &crev1alpha1.WorkflowSpec{
		JobTemplate:   *jobTemplate,
		Orchestration: *orch,
		Dependencies:  deps,
		Overrides:     overrides,
	}

	if len(spec.Thresholds) > 0 {
		workflowSpec.Validation = &crev1alpha1.ValidationSpec{
			Performance: &crev1alpha1.PerformanceValidationSpec{
				Enabled: true,
				Thresholds: &crev1alpha1.ThresholdSpec{
					Thresholds: spec.Thresholds,
				},
			},
		}
	}

	return workflowSpec
}

// validateExecFramework returns an error when the exec framework is implied
// (neither Torch nor MPI is set) but spec.Framework.Exec is nil, which would
// cause a nil-pointer dereference inside buildCLIJobTemplate.
func validateExecFramework(spec *crev1alpha1.WorkloadRunSpec, name string) error {
	if spec.Framework.Torch == nil && spec.Framework.MPI == nil && spec.Framework.Exec == nil {
		return fmt.Errorf("workloadrun %s: exec framework selected but spec.framework.exec is nil", name)
	}
	return nil
}

func buildCLIJobTemplate(
	run *crev1alpha1.WorkloadRun, frameworkType string, gpusPerNode int32, enableMNNVL bool,
) *crev1alpha1.JobTemplateSpec {
	spec := &run.Spec

	var command []string
	var args []string

	switch frameworkType {
	case controller.FrameworkTorch:
		torch := spec.Framework.Torch
		command = []string{"torchrun"}
		if torch.Module != "" {
			args = append([]string{"-m", torch.Module}, torch.Args...)
		} else {
			args = append([]string{torch.Script}, torch.Args...)
		}
	case controller.FrameworkMPI:
		mpi := spec.Framework.MPI
		command = []string{"timeout", "3600", mpi.MpirunPath}
		baseCount := 10 // fixed args below
		mpiArgs := make([]string, 0, baseCount+len(mpi.MpiArgs)+1+len(mpi.Args))
		mpiArgs = append(mpiArgs,
			"-N", fmt.Sprintf("%d", gpusPerNode),
			"--allow-run-as-root",
			"--mca", "plm_rsh_args",
			"-o StrictHostKeyChecking=no -o ConnectionAttempts=10",
			"-x", "NCCL_DEBUG=INFO",
		)
		enableStr := "0"
		if enableMNNVL {
			enableStr = "1"
		}
		mpiArgs = append(mpiArgs, "-x", fmt.Sprintf("NCCL_MNNVL_ENABLE=%s", enableStr))
		mpiArgs = append(mpiArgs, mpi.MpiArgs...)
		mpiArgs = append(mpiArgs, mpi.Binary)
		mpiArgs = append(mpiArgs, mpi.Args...)
		args = mpiArgs
	default:
		exec := spec.Framework.Exec
		command = exec.Command
		args = exec.Args
	}

	trainJobSpec := &trainerv1alpha1.TrainJobSpec{
		RuntimeRef: trainerv1alpha1.RuntimeRef{
			Name: fmt.Sprintf("%s-runtime", run.Name),
			Kind: func() *string { s := "TrainingRuntime"; return &s }(),
		},
		Trainer: &trainerv1alpha1.Trainer{
			Image:          &spec.Image,
			Command:        command,
			Args:           args,
			NumNodes:       new(nodesPerJobForScale(spec.Orchestration, spec.NumNodes)),
			NumProcPerNode: &gpusPerNode,
		},
	}

	workload.SetImagePullSecrets(trainJobSpec, spec.ImagePullSecrets)

	jobSpec := crev1alpha1.JobSpec{
		Workload: crev1alpha1.WorkloadSpec{
			TrainJob: trainJobSpec,
		},
		NodeHealthMonitor: &crev1alpha1.NodeHealthMonitor{
			CEL: &crev1alpha1.CELNodeHealthCheck{
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

	return &crev1alpha1.JobTemplateSpec{Spec: jobSpec}
}

func buildWRCLIConfigMapDep(name string, data map[string]string) crev1alpha1.DependencySpec {
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
	return crev1alpha1.DependencySpec{
		RawExtension: kruntime.RawExtension{Raw: raw},
	}
}

// runWorkloadRunRenderDryRun connects to a live cluster to discover
// real nodes, detect platform/GPU, and render with actual overrides.
func runWorkloadRunRenderDryRun(
	file, outputFormat string, configFlags *kubeconfig.ConfigFlags,
) error {
	run, err := readWorkloadRun(file)
	if err != nil {
		return err
	}

	ctx := context.Background()
	c, err := render.NewK8sClient(configFlags)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	nodes, gpuProduct, nodeErr := cluster.DiscoverGPUNodes(ctx, c, run.Spec.Target)
	if nodeErr != nil {
		return nodeErr
	}

	detectedPlatform := controller.DetectPlatform(nodes)
	gpuArch := controller.DetectGPUArchitecture(nodes)
	nd := catalog.GPUDefaults(gpuArch, detectedPlatform)
	gpusPerNode := nd.GpusPerNode
	mlnxPerNode := nd.MlnxPerNode
	if run.Spec.GpusPerNode != nil {
		gpusPerNode = *run.Spec.GpusPerNode
	}
	if run.Spec.MlnxPerNode != nil {
		mlnxPerNode = *run.Spec.MlnxPerNode
	}
	enableMNNVL := controller.DefaultEnableMNNVL(gpuArch)
	if run.Spec.EnableMNNVL != nil {
		enableMNNVL = *run.Spec.EnableMNNVL
	}

	_, _ = fmt.Fprintf(os.Stderr,
		"Discovered %d nodes: %s (%s on %s)\n",
		len(nodes), gpuProduct, gpuArch, detectedPlatform)

	frameworkType := controller.FrameworkExec
	if run.Spec.Framework.Torch != nil {
		frameworkType = controller.FrameworkTorch
	} else if run.Spec.Framework.MPI != nil {
		frameworkType = controller.FrameworkMPI
	}
	if err := validateExecFramework(&run.Spec, run.Name); err != nil {
		return err
	}

	workflowSpec := BuildWorkflowSpec(
		run, gpusPerNode, mlnxPerNode, enableMNNVL, frameworkType)

	orch := &crev1alpha1.OrchestrationStatus{
		DetectedPlatform:        detectedPlatform,
		DetectedGPUArchitecture: gpuArch,
	}
	octx := controller.BuildOverrideContext(workflowSpec, orch, nodes)
	if _, overrideErr := controller.ApplyOverridesWithTracking(
		workflowSpec, octx); overrideErr != nil {
		return fmt.Errorf("applying overrides: %w", overrideErr)
	}
	workflowSpec.Overrides = nil

	workflow := &crev1alpha1.Workflow{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cre.nvidia.com/v1alpha1",
			Kind:       "Workflow",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.Name,
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cluster-readiness-engine",
				"cre.nvidia.com/workload-run":  run.Name,
			},
			Annotations: map[string]string{
				"ncrectl.nvidia.com/detected-gpu-architecture": gpuArch,
				"ncrectl.nvidia.com/detected-platform":         detectedPlatform,
			},
		},
		Spec: *workflowSpec,
	}

	switch outputFormat {
	case outputJSON:
		data, _ := json.MarshalIndent(workflow, "", "  ")
		fmt.Println(string(data))
	default:
		data, _ := sigyaml.Marshal(workflow)
		fmt.Print(string(data))
	}
	return nil
}

// --- run ---

func newWorkloadRunRunCommand() *cobra.Command {
	var doWait, doSetup, doCleanup bool
	var controllerPullSecret string
	var workloadRegistry, workloadRegistryUsername, workloadRegistryPassword string
	var controllerImage string
	var resultsFile string
	var timeout time.Duration
	var nameOverride, nodeList, topologyDomain, topologyKey, testScale string

	configFlags := kubeconfig.NewConfigFlags(true)

	cmd := &cobra.Command{
		Use:   "run [flags] <workloadrun.yaml>",
		Short: "Create a WorkloadRun on the target cluster",
		Long: `Creates a WorkloadRun resource in the cluster from a YAML file.

Use --setup to install CRDs, controller, and LogProfiles before creating.
Use --wait to watch for completion and print a report.
Use --cleanup to teardown after completion.

Override flags (--name, --node-list, --topology-domain, --test-scale) modify
the WorkloadRun spec before submission. When --node-list is used and the
number of nodes is less than spec.numNodes, numNodes is automatically clamped.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pullSet := 0
			for _, v := range []string{workloadRegistry, workloadRegistryUsername, workloadRegistryPassword} {
				if v != "" {
					pullSet++
				}
			}
			if pullSet > 0 && pullSet < 3 {
				return fmt.Errorf("--workload-registry, --workload-registry-username, and --workload-registry-password must all be set together and non-empty")
			}
			return runWorkloadRunExecute(args[0], *configFlags.Namespace,
				workloadRegistry, workloadRegistryUsername, workloadRegistryPassword,
				controllerImage, controllerPullSecret, doWait, doSetup, doCleanup, timeout,
				resultsFile, configFlags,
				nameOverride, nodeList, topologyDomain, topologyKey, testScale)
		},
	}

	cmd.Flags().BoolVar(&doWait, "wait", false, "Watch for completion")
	cmd.Flags().BoolVar(&doSetup, "setup", false, "Install CRDs, controller, LogProfiles before creating")
	cmd.Flags().BoolVar(&doCleanup, "cleanup", false, "Delete WorkloadRun and installed components after completion")
	cmd.Flags().StringVar(&controllerPullSecret, "controller-pull-secret", "",
		"Token for controller registry authentication during --setup (e.g. GitHub PAT for ghcr.io) — separate from workload image credentials")
	cmd.Flags().StringVar(&workloadRegistry, "workload-registry", "",
		"Registry server for workload image pull (e.g. nvcr.io, ghcr.io) — required when --workload-registry-password is set")
	cmd.Flags().StringVar(&workloadRegistryUsername, "workload-registry-username", "",
		"Registry username for workload image pull (e.g. \\$oauthtoken for NGC) — required when --workload-registry-password is set")
	cmd.Flags().StringVar(&workloadRegistryPassword, "workload-registry-password", "",
		"Registry password or API key for workload image pull — creates an imagePullSecret in the WorkloadRun namespace")
	cmd.Flags().StringVar(&controllerImage, "image", "", "Override controller image")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "Wait timeout")
	cmd.Flags().StringVar(&resultsFile, "results-file", "",
		"Write report as JSON to this file path (requires --wait)")
	cmd.Flags().StringVar(&nameOverride, "name", "",
		"Override metadata.name")
	cmd.Flags().StringVar(&nodeList, "node-list", "",
		"Comma-separated node names (sets target.nodeNames)")
	cmd.Flags().StringVar(&topologyDomain, "topology-domain", "",
		"Topology domain name (sets target.matchExpressions)")
	cmd.Flags().StringVar(&topologyKey, "topology-key", "",
		"Override topology label key for --topology-domain")
	cmd.Flags().StringVar(&testScale, "test-scale", "",
		"Override testScale (intra-node, intra-rack, full-scale)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

func runWorkloadRunExecute(
	file, namespace, workloadRegistry, workloadRegistryUsername, workloadRegistryPassword, controllerImage, controllerPullSecret string,
	doWait, doSetup, _ bool, timeout time.Duration,
	resultsFile string, configFlags *kubeconfig.ConfigFlags,
	nameOverride, nodeList, topologyDomain,
	topologyKey, testScale string,
) error {

	run, err := readWorkloadRun(file)
	if err != nil {
		return err
	}

	// Apply CLI overrides to the WorkloadRun spec.
	applyRunOverrides(run, nameOverride, nodeList,
		topologyDomain, topologyKey, testScale)

	if namespace != "" {
		run.Namespace = namespace
	}
	if run.Namespace == "" {
		run.Namespace = defaultNS
	}

	out := os.Stderr

	// Build client.
	wc, err := render.NewK8sWatchClient(configFlags)
	if err != nil {
		return fmt.Errorf("build client: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Setup phase.
	if doSetup {
		_, _ = fmt.Fprintln(out, "Installing CRE components...")
		if initErr := setup.RunInit("", controllerImage, controllerPullSecret, "",
			true, configFlags, "", os.Stdin, out); initErr != nil {
			return fmt.Errorf("setup: %w", initErr)
		}
		_, _ = fmt.Fprintln(out, "Setup complete.")
	}

	// Create namespace if needed.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: run.Namespace}}
	if err := wc.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", run.Namespace, err)
	}

	// Create pull secret before the WorkloadRun so pods can pull immediately.
	// OwnerReference is set after creation to enable automatic GC.
	// wasCreatedByUs is false when an existing secret was only updated (concurrent
	// run) — do not delete on rollback in that case.
	wasCreatedByUs := false
	if workloadRegistryPassword != "" {
		secretName, created, secretErr := setup.CreateImagePullSecret(ctx, wc,
			run.Namespace, setup.WorkloadPullSecretName(run.Name), workloadRegistry, workloadRegistryUsername, workloadRegistryPassword)
		if secretErr != nil {
			return fmt.Errorf("create image pull secret: %w", secretErr)
		}
		wasCreatedByUs = created
		run.Spec.ImagePullSecrets = append(run.Spec.ImagePullSecrets,
			corev1.LocalObjectReference{Name: secretName})
		_, _ = fmt.Fprintf(out, "Created image pull secret %q in namespace %s.\n", secretName, run.Namespace)
	}

	// Resolve auto topology key before discovery. The __auto__ placeholder
	// can't be used for node filtering, so we do a preliminary discovery
	// without matchExpressions to detect the platform first.
	if topologyDomain != "" && topologyKey == "" {
		prelimNodes, _, _ := cluster.DiscoverGPUNodes(ctx, wc, nil)
		if len(prelimNodes) > 0 {
			detectedPlatform := controller.DetectPlatform(prelimNodes)
			resolved := cluster.DefaultTopologyKey(detectedPlatform)
			if resolved == "" {
				return fmt.Errorf(
					"--topology-domain requires --topology-key on platform %q",
					detectedPlatform)
			}
			for i := range run.Spec.Target.MatchExpressions {
				if run.Spec.Target.MatchExpressions[i].Key == "__auto__" {
					run.Spec.Target.MatchExpressions[i].Key = resolved
				}
			}
		}
	}

	// Discover and validate target nodes before submitting.
	nodes, gpuProduct, nodeErr := cluster.DiscoverGPUNodes(ctx, wc, run.Spec.Target)
	if nodeErr != nil {
		return nodeErr
	}
	_, _ = fmt.Fprintf(out, "Discovered %d GPU nodes with product: %s\n",
		len(nodes), gpuProduct)

	// Auto-infer numNodes from target node count.
	// --node-list: clamp down if fewer nodes than spec.
	// --topology-domain: set to discovered count (all nodes in the domain).
	if nodeList != "" && run.Spec.NumNodes > int32(len(nodes)) {
		run.Spec.NumNodes = int32(len(nodes))
	}
	if topologyDomain != "" {
		run.Spec.NumNodes = int32(len(nodes))
	}

	// Create WorkloadRun.
	if err := wc.Create(ctx, run); err != nil {
		// Only delete the secret on rollback if we actually created it —
		// if we updated a pre-existing secret, deleting it would break a concurrent run.
		if wasCreatedByUs {
			pullSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: setup.WorkloadPullSecretName(run.Name), Namespace: run.Namespace,
			}}
			_ = wc.Delete(ctx, pullSec)
		}
		return fmt.Errorf("create WorkloadRun: %w", err)
	}
	_, _ = fmt.Fprintf(out, "WorkloadRun %s created in namespace %s.\n",
		run.Name, run.Namespace)

	// Set OwnerReference on the pull secret now that we have the WorkloadRun UID.
	// Only when wasCreatedByUs: if we only updated an existing secret we don't own
	// it and must not manage its lifecycle.
	if wasCreatedByUs {
		sec := &corev1.Secret{}
		if getErr := wc.Get(ctx, client.ObjectKey{Name: setup.WorkloadPullSecretName(run.Name), Namespace: run.Namespace}, sec); getErr != nil {
			_, _ = fmt.Fprintf(out, "Warning: could not retrieve pull secret %q to set OwnerReference: %v\n",
				setup.WorkloadPullSecretName(run.Name), getErr)
		} else {
			sec.OwnerReferences = append(sec.OwnerReferences, metav1.OwnerReference{
				APIVersion: "cre.nvidia.com/v1alpha1",
				Kind:       "WorkloadRun",
				Name:       run.Name,
				UID:        run.UID,
			})
			if updateErr := wc.Update(ctx, sec); updateErr != nil {
				_, _ = fmt.Fprintf(out, "Warning: could not set OwnerReference on pull secret %q — it will not be GC'd automatically: %v\n",
					setup.WorkloadPullSecretName(run.Name), updateErr)
			}
		}
	}

	if !doWait {
		_, _ = fmt.Fprintf(out, "\nTo check status:\n")
		_, _ = fmt.Fprintf(out, "  kubectl get workloadrun %s -n %s\n", run.Name, run.Namespace)
		return nil
	}

	// Wait for completion.
	_, _ = fmt.Fprintf(out, "\nWaiting for completion (timeout: %s)...\n", timeout)
	finalRun, waitErr := watchWorkloadRun(ctx, wc, run.Name, run.Namespace, timeout, out)

	// Print report if we have a terminal WorkloadRun.
	if finalRun != nil {
		r := buildWorkloadRunReport(ctx, wc, finalRun)
		report.Print(out, r)
		if resultsFile != "" {
			if err := report.WriteJSON(resultsFile, []*report.CertReport{r}); err != nil {
				_, _ = fmt.Fprintf(out, "Warning: failed to write results: %v\n", err)
			} else {
				_, _ = fmt.Fprintf(out, "Results written to %s\n", resultsFile)
			}
		}
	}

	return waitErr
}

// watchWorkloadRun polls until the WorkloadRun reaches a terminal state.
// It prints a "[watch]" line on every phase change and a periodic heartbeat
// (same format and interval as the certification watch) so long runs show
// progress instead of going silent until the terminal condition.
func watchWorkloadRun(
	ctx context.Context, c client.WithWatch,
	name, namespace string, timeout time.Duration, out io.Writer,
) (*crev1alpha1.WorkloadRun, error) {
	deadline := time.After(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	start := time.Now()
	lastPhase := ""
	var current crev1alpha1.WorkloadRun

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("interrupted")
		case <-deadline:
			return nil, fmt.Errorf("timeout waiting for WorkloadRun %s", name)
		case <-heartbeat.C:
			elapsed := time.Since(start).Truncate(time.Second)
			_, _ = fmt.Fprintln(out, workloadRunWatchLine(&current, name, elapsed))
		case <-ticker.C:
			key := client.ObjectKey{Name: name, Namespace: namespace}
			if err := c.Get(ctx, key, &current); err != nil {
				continue
			}
			elapsed := time.Since(start).Truncate(time.Second)
			if controller.CondIsTrue(current.Status.Conditions, crev1alpha1.WorkloadRunSucceeded) {
				_, _ = fmt.Fprintf(out, "[watch] WorkloadRun succeeded. (%s)\n", elapsed)
				return &current, nil
			}
			if controller.CondIsTrue(current.Status.Conditions, crev1alpha1.WorkloadRunFailed) {
				_, _ = fmt.Fprintf(out, "[watch] WorkloadRun failed. (%s)\n", elapsed)
				msg := controller.CondMessage(current.Status.Conditions, crev1alpha1.WorkloadRunFailed)
				return &current, fmt.Errorf("WorkloadRun failed: %s", msg)
			}
			if phase := workloadRunPhase(&current); phase != lastPhase {
				_, _ = fmt.Fprintln(out, workloadRunWatchLine(&current, name, elapsed))
				lastPhase = phase
			}
		}
	}
}

// workloadRunPhase returns which execution condition (InProgress, Succeeded,
// Failed) is currently true, or "" when the controller has not set one yet.
func workloadRunPhase(run *crev1alpha1.WorkloadRun) string {
	for _, t := range []string{
		crev1alpha1.WorkloadRunInProgress,
		crev1alpha1.WorkloadRunSucceeded,
		crev1alpha1.WorkloadRunFailed,
	} {
		if controller.CondIsTrue(run.Status.Conditions, t) {
			return t
		}
	}
	return ""
}

// workloadRunWatchLine formats one "[watch]" progress line, matching the
// certification watch format. It shows the current phase, its condition
// message (which carries the underlying Workflow state, e.g. "Workflow
// <name> created"), and elapsed time. Before the controller sets any
// execution condition it reports that status is still pending.
func workloadRunWatchLine(run *crev1alpha1.WorkloadRun, name string, elapsed time.Duration) string {
	phase := workloadRunPhase(run)
	if phase == "" {
		return fmt.Sprintf("[watch] Waiting for status... (%s)", elapsed)
	}
	line := fmt.Sprintf("[watch] %s: %s (%s)", name, phase, elapsed)
	if msg := controller.CondMessage(run.Status.Conditions, phase); msg != "" {
		line += " — " + msg
	}
	return line
}

// --- helpers ---

func readWorkloadRun(file string) (*crev1alpha1.WorkloadRun, error) {
	data, err := os.ReadFile(file) // #nosec G304 -- file is a user-provided CLI argument

	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}

	var run crev1alpha1.WorkloadRun
	if err := yaml.NewYAMLOrJSONDecoder(
		bytes.NewReader(data), 4096).Decode(&run); err != nil {
		return nil, fmt.Errorf("parsing WorkloadRun: %w", err)
	}

	if run.Kind != "" && run.Kind != "WorkloadRun" {
		return nil, fmt.Errorf("expected kind WorkloadRun, got %q", run.Kind)
	}

	return &run, nil
}

// loadSyntheticNodes creates synthetic nodes for offline override matching.
// It tries the embedded templates first (via render.LoadEmbeddedNodes); if no
// template exists for the given platform+arch it falls back to a minimal
// synthetic node.
func loadSyntheticNodes(platformName, gpuArch string) []corev1.Node {
	if nodes, err := render.LoadEmbeddedNodes(platformName, gpuArch); err == nil {
		return nodes
	}
	// Fallback: create a minimal synthetic node.
	labels := map[string]string{
		"nvidia.com/gpu.present": "true",
		"nvidia.com/gpu.product": fmt.Sprintf("NVIDIA-%s", gpuArch),
	}
	if platformName == "forge" {
		labels["kubernetes.io/hostname"] = "synthetic-forge-node"
	}
	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "synthetic-node-0",
			Labels: labels,
		},
		Spec: corev1.NodeSpec{
			ProviderID: render.SyntheticProviderID(platformName),
		},
	}
	// nscale shares the openstack:// providerID prefix; detection disambiguates
	// via the rdmashare allocatable (see pkg/render/nodes.go), so the synthetic
	// node must carry it for node-based detection to resolve to nscale.
	if platformName == "nscale" {
		node.Status.Allocatable = corev1.ResourceList{
			"nscale.com/rdmashare": resource.MustParse("8"),
		}
	}
	return []corev1.Node{node}
}

// syntheticProviderID is in run_common.go.

// --- report ---

func newWorkloadRunReportCommand() *cobra.Command {
	var resultsFile, output string

	configFlags := kubeconfig.NewConfigFlags(true)
	*configFlags.Namespace = defaultNS

	cmd := &cobra.Command{
		Use:   "report <workloadrun-name>",
		Short: "Generate a report for a WorkloadRun",
		Long: `Connects to the cluster, fetches the named WorkloadRun and its Workflow,
and generates the same report that 'ncrectl workloadrun run --wait' prints.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkloadRunReport(args[0], configFlags, resultsFile, output)
		},
	}

	cmd.Flags().StringVar(&resultsFile, "results-file", "",
		"Write report as JSON to this file path")
	cmd.Flags().StringVarP(&output, "output", "o", "text",
		"Output format: text, json")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

func runWorkloadRunReport(
	name string, configFlags *kubeconfig.ConfigFlags,
	resultsFile, output string,
) error {
	ctx := context.Background()
	namespace := *configFlags.Namespace

	c, err := render.NewK8sClient(configFlags)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	var run crev1alpha1.WorkloadRun
	if err := c.Get(ctx, client.ObjectKey{
		Name: name, Namespace: namespace,
	}, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf(
				"workloadrun %q not found in namespace %q", name, namespace)
		}
		return fmt.Errorf("get workloadrun %q: %w", name, err)
	}

	r := buildWorkloadRunReport(ctx, c, &run)

	if output == outputJSON {
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal report: %w", err)
		}
		fmt.Println(string(data))
	} else {
		report.Print(os.Stdout, r)
	}

	if resultsFile != "" {
		if err := report.WriteJSON(resultsFile, []*report.CertReport{r}); err != nil {
			return fmt.Errorf("write results file: %w", err)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Results written to %s\n", resultsFile)
	}

	succeeded := controller.CondIsTrue(
		run.Status.Conditions, crev1alpha1.WorkloadRunSucceeded)
	failed := controller.CondIsTrue(
		run.Status.Conditions, crev1alpha1.WorkloadRunFailed)
	if !succeeded && !failed {
		return fmt.Errorf("workloadrun %q is still running", name)
	}
	if failed {
		return fmt.Errorf("workloadrun %q failed", name)
	}
	return nil
}

// buildWorkloadRunReport creates a CertReport from a WorkloadRun by fetching
// its child Workflow. Reuses the same report model and PopulateCategoryFromWorkflow
// function as certification reports.
func buildWorkloadRunReport(
	ctx context.Context, c client.Client, run *crev1alpha1.WorkloadRun,
) *report.CertReport {
	result := "RUNNING"
	if controller.CondIsTrue(
		run.Status.Conditions, crev1alpha1.WorkloadRunFailed) {
		result = "FAILED"
	} else if controller.CondIsTrue(
		run.Status.Conditions, crev1alpha1.WorkloadRunSucceeded) {
		result = "PASSED"
	}

	r := &report.CertReport{
		Title:       "WorkloadRun Report",
		Name:        run.Name,
		Platform:    run.Status.DetectedPlatform,
		GPU:         run.Status.DetectedGPUArchitecture,
		FailedNodes: noderesults.FailedNodeNames(report.FailedNodesFromRef(ctx, c, run.Namespace, run.Status.FailedNodesRef)),
		Result:      result,
	}

	// Build a single category from the WorkloadRun's Workflow.
	cat := report.CategoryReport{
		Domain:  "workloadrun",
		Variant: run.Name,
	}

	if run.Status.WorkflowRef != nil {
		wf := &crev1alpha1.Workflow{}
		ns := run.Status.WorkflowRef.Namespace
		if ns == "" {
			ns = run.Namespace
		}
		key := client.ObjectKey{
			Name: run.Status.WorkflowRef.Name, Namespace: ns,
		}
		if err := c.Get(ctx, key, wf); err == nil {
			report.PopulateCategoryFromWorkflow(ctx, c, &cat, wf)
			r.TotalNodes = wf.Status.Orchestration.TotalNodes

			// Set category status from Workflow conditions.
			switch {
			case controller.CondIsTrue(wf.Status.Conditions, crev1alpha1.WorkflowSucceeded):
				cat.Status = "Succeeded"
			case controller.CondIsTrue(wf.Status.Conditions, crev1alpha1.WorkflowFailed):
				cat.Status = statusFailed
			case controller.CondIsTrue(wf.Status.Conditions, crev1alpha1.WorkflowInProgress):
				cat.Status = "Running"
			}

			// Build per-node results from orchestration groups. Failed nodes are
			// resolved from the Workflow's failed-nodes ConfigMap.
			r.NodeResults = buildNodeResults(wf, report.FailedNodesFromRef(ctx, c, wf.Namespace, wf.Status.FailedNodesRef))
		}
	}

	r.Categories = []report.CategoryReport{cat}
	return r
}

// applyRunOverrides modifies a WorkloadRun based on CLI override flags.
func applyRunOverrides(
	run *crev1alpha1.WorkloadRun,
	nameOverride, nodeList, topologyDomain, topologyKey, testScale string,
) {
	if nameOverride != "" {
		run.Name = nameOverride
	}
	if nodeList != "" {
		names := strings.Split(nodeList, ",")
		if run.Spec.Target == nil {
			run.Spec.Target = &crev1alpha1.TargetSpec{}
		}
		run.Spec.Target.NodeNames = names
		if int32(len(names)) < run.Spec.NumNodes {
			run.Spec.NumNodes = int32(len(names))
		}
	}
	if topologyDomain != "" {
		if run.Spec.Target == nil {
			run.Spec.Target = &crev1alpha1.TargetSpec{}
		}
		key := topologyKey
		if key == "" {
			key = "__auto__"
		}
		run.Spec.Target.MatchExpressions = append(
			run.Spec.Target.MatchExpressions,
			corev1.NodeSelectorRequirement{
				Key:      key,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{topologyDomain},
			})
	}
	if testScale != "" {
		if run.Spec.Orchestration == nil {
			run.Spec.Orchestration = &crev1alpha1.WorkloadOrchestration{}
		}
		run.Spec.Orchestration.TestScale = testScale
	}
}

// buildNodeResults flattens orchestration groups into per-node pass/fail results.
func buildNodeResults(wf *crev1alpha1.Workflow, failedNodes []crev1alpha1.FailedNode) []report.NodeResultReport {
	orch := wf.Status.Orchestration
	if orch == nil {
		return nil
	}
	failedSet := make(map[string]bool)
	for _, n := range failedNodes {
		failedSet[n.Name] = true
	}

	var results []report.NodeResultReport
	for _, g := range orch.Groups {
		groupStatus := "Passed"
		if g.Phase == crev1alpha1.GroupFailed {
			groupStatus = statusFailed
		}
		rack := ""
		if len(g.Domains) > 0 {
			rack = g.Domains[0]
		}
		for _, node := range g.Nodes {
			nodeStatus := groupStatus
			if failedSet[node] {
				nodeStatus = "Failed"
			}
			results = append(results, report.NodeResultReport{
				Name:   node,
				Group:  g.Name,
				Rack:   rack,
				Status: nodeStatus,
			})
		}
	}
	return results
}

// --- status ---

// WorkloadRunStatusOutput is the JSON output for ncrectl workloadrun status.
type WorkloadRunStatusOutput struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace"`
	Status      string   `json:"status"`
	FailedNodes []string `json:"failedNodes,omitempty"`
}

func newWorkloadRunStatusCommand() *cobra.Command {
	var output string

	configFlags := kubeconfig.NewConfigFlags(true)
	*configFlags.Namespace = defaultNS

	cmd := &cobra.Command{
		Use:   "status <workloadrun-name>",
		Short: "Check the status of a WorkloadRun",
		Long:  `Lightweight status check for polling. Returns the current phase without a full report.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			namespace := *configFlags.Namespace
			c, err := render.NewK8sClient(configFlags)
			if err != nil {
				return err
			}

			var run crev1alpha1.WorkloadRun
			if err := c.Get(ctx, client.ObjectKey{
				Name: args[0], Namespace: namespace,
			}, &run); err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("workloadrun %q not found in namespace %q", args[0], namespace)
				}
				return fmt.Errorf("get workloadrun: %w", err)
			}

			status := "Pending"
			switch {
			case controller.CondIsTrue(run.Status.Conditions, crev1alpha1.WorkloadRunSucceeded):
				status = "Succeeded"
			case controller.CondIsTrue(run.Status.Conditions, crev1alpha1.WorkloadRunFailed):
				status = "Failed"
			case controller.CondIsTrue(run.Status.Conditions, crev1alpha1.WorkloadRunInProgress):
				status = "InProgress"
			}

			if output == outputJSON {
				out := WorkloadRunStatusOutput{
					Name:        run.Name,
					Namespace:   run.Namespace,
					Status:      status,
					FailedNodes: noderesults.FailedNodeNames(report.FailedNodesFromRef(ctx, c, run.Namespace, run.Status.FailedNodesRef)),
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}

			fmt.Println(status)
			return nil
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "text", "Output format: text, json")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// --- cancel ---

func newWorkloadRunCancelCommand() *cobra.Command {
	configFlags := kubeconfig.NewConfigFlags(true)
	*configFlags.Namespace = defaultNS // Gap 1 (ADR-067): preserve today's default

	cmd := &cobra.Command{
		Use:   "cancel <name> [name...]",
		Short: "Cancel one or more running WorkloadRuns",
		Long:  `Deletes WorkloadRun resources, which cascades to their Workflows, Jobs, and workloads.`,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			namespace := *configFlags.Namespace
			c, err := render.NewK8sClient(configFlags)
			if err != nil {
				return err
			}

			var lastErr error
			for _, name := range args {
				run := &crev1alpha1.WorkloadRun{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: namespace,
					},
				}
				if err := c.Delete(ctx, run); err != nil {
					if apierrors.IsNotFound(err) {
						fmt.Fprintf(os.Stderr, "WorkloadRun %q not found in namespace %q\n", name, namespace)
					} else {
						fmt.Fprintf(os.Stderr, "Failed to cancel %q: %v\n", name, err)
						lastErr = err
					}
					continue
				}
				fmt.Printf("Cancelled %s\n", name)
			}
			return lastErr
		},
	}

	configFlags.AddFlags(cmd.Flags())

	return cmd
}
