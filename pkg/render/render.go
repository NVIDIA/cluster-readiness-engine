// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// workflow_render implements the "ncrectl workflow render" subcommand.
package render

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

//go:embed nodes/*.yaml
var embeddedNodes embed.FS

// NewWorkflowCommand returns the "workflow" parent cobra command with
// the "render" subcommand attached. This was previously newWorkflowCommand
// in root.go.
func NewWorkflowCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Workflow management commands",
	}
	cmd.AddCommand(newRenderCommand())
	return cmd
}

func newRenderCommand() *cobra.Command {
	var platform string
	var gpuArch string
	var nodesFile string
	var outputFormat string
	var dryRun bool

	configFlags := kubeconfig.NewConfigFlags(true)
	*configFlags.Namespace = "default"

	cmd := &cobra.Command{
		Use:   "render [flags] <workflow.yaml>",
		Short: "Render a Workflow with overrides applied (offline dry-run)",
		Long: `Reads a Workflow YAML file, applies platform/GPU overrides,
and prints the resolved jobTemplate and dependencies.

Node context (required for override matching) can be provided in three
mutually exclusive ways:

  --platform + --gpu-arch   Generate a mock node from built-in templates.
  --nodes-file <path>       Supply real or custom nodes from a YAML file.
  --dry-run                 Discover real nodes from cluster and validate
                            resolved resources via server-side dry-run.
                            Requires cluster access (--kubeconfig).

The first two modes are fully offline. --dry-run connects to a cluster
to discover nodes and validates the resolved resources without persisting
anything.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf(
					"requires a workflow YAML file as argument\n\n" +
						"Usage: ncrectl workflow render [flags] <workflow.yaml>",
				)
			}
			if err := validateFlags(dryRun, nodesFile, platform, gpuArch); err != nil {
				return err
			}
			return run(args[0], platform, gpuArch, nodesFile, outputFormat,
				configFlags, dryRun)
		},
	}

	cmd.Flags().StringVar(&platform, "platform", "", "Target platform (aws, gcp, azure, oci, mistral, forge)")
	cmd.Flags().StringVar(&gpuArch, "gpu-arch", "", "Target GPU architecture (h100, gb200, gb300)")
	cmd.Flags().StringVar(&nodesFile, "nodes-file", "",
		"Custom nodes YAML file (mutually exclusive with --platform/--gpu-arch)")
	cmd.Flags().StringVar(&outputFormat, "output", "yaml", "Output format: yaml or json")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Connect to cluster, discover real nodes, and validate via server-side dry-run")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// DryRunResult records the outcome of validating a single resource via server-side dry-run.
type DryRunResult struct {
	Resource string `json:"resource"`
	Valid    bool   `json:"valid"`
	Error    string `json:"error,omitempty"`
	Warning  string `json:"warning,omitempty"`
}

// renderMetadata captures detection and override results from resolving a Workflow.
type renderMetadata struct {
	DetectedPlatform        string                           `json:"detectedPlatform"`
	DetectedGPUArchitecture string                           `json:"detectedGPUArchitecture"`
	AppliedOverrides        []burninv1alpha1.AppliedOverride `json:"appliedOverrides"`
}

// readWorkflow parses a Workflow YAML file from disk.
func readWorkflow(path string) (*burninv1alpha1.Workflow, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is a user-provided CLI argument

	if err != nil {
		return nil, fmt.Errorf("read workflow: %w", err)
	}
	var workflow burninv1alpha1.Workflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	return &workflow, nil
}

// ResolveWorkflow applies overrides to a Workflow in-place and returns
// detection metadata. The workflow's spec is mutated directly.
func ResolveWorkflow(workflow *burninv1alpha1.Workflow, nodes []corev1.Node) (*renderMetadata, error) {
	orch := &burninv1alpha1.OrchestrationStatus{
		DetectedPlatform:        controller.DetectPlatform(nodes),
		DetectedGPUArchitecture: controller.DetectGPUArchitecture(nodes),
	}

	octx := controller.BuildOverrideContext(&workflow.Spec, orch, nodes)

	applied, err := controller.ApplyOverridesWithTracking(&workflow.Spec, octx)
	if err != nil {
		return nil, fmt.Errorf("apply overrides: %w", err)
	}

	// Clear overrides since they've been resolved.
	workflow.Spec.Overrides = nil

	return &renderMetadata{
		DetectedPlatform:        orch.DetectedPlatform,
		DetectedGPUArchitecture: orch.DetectedGPUArchitecture,
		AppliedOverrides:        applied,
	}, nil
}

// render resolves overrides and returns the full Workflow with metadata.
func render(workflowFile, platform, gpuArch, nodesFile string) (*burninv1alpha1.Workflow, *renderMetadata, error) {
	workflow, err := readWorkflow(workflowFile)
	if err != nil {
		return nil, nil, err
	}

	nodes, err := resolveNodes(platform, gpuArch, nodesFile)
	if err != nil {
		return nil, nil, err
	}

	workflow.TypeMeta = metav1.TypeMeta{
		APIVersion: "cre.nvidia.com/v1alpha1",
		Kind:       "Workflow",
	}

	meta, err := ResolveWorkflow(workflow, nodes)
	if err != nil {
		return nil, nil, err
	}

	return workflow, meta, nil
}

// validateFlags checks for conflicting flag combinations.
func validateFlags(dryRun bool, nodesFile, platform, gpuArch string) error {
	if dryRun {
		if nodesFile != "" || platform != "" || gpuArch != "" {
			return fmt.Errorf(
				"--dry-run discovers nodes from cluster; cannot combine with --nodes-file, --platform, or --gpu-arch")
		}
	}
	if nodesFile != "" && (platform != "" || gpuArch != "") {
		return fmt.Errorf("--nodes-file and --platform/--gpu-arch are mutually exclusive")
	}
	return nil
}

func run(workflowFile, platform, gpuArch, nodesFile, outputFormat string,
	configFlags *kubeconfig.ConfigFlags, dryRun bool) error {

	var workflow *burninv1alpha1.Workflow
	var meta *renderMetadata
	var dryRunResults []DryRunResult
	var err error

	if dryRun {
		workflow, meta, dryRunResults, err = renderDryRun(workflowFile, configFlags)
	} else {
		workflow, meta, err = render(workflowFile, platform, gpuArch, nodesFile)
	}
	if err != nil {
		return err
	}

	// Store render metadata as annotations.
	SetRenderAnnotations(workflow, meta)

	switch outputFormat {
	case "json":
		data, err := json.MarshalIndent(workflow, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Println(string(data))
	default:
		data, err := yaml.Marshal(workflow)
		if err != nil {
			return fmt.Errorf("marshal yaml: %w", err)
		}
		fmt.Print(string(data))
	}

	if len(dryRunResults) > 0 {
		PrintDryRunSummary("Dry-run validation: "+workflow.Name, dryRunResults)
	}

	return nil
}

// SetRenderAnnotations stores detection metadata as annotations on the Workflow.
func SetRenderAnnotations(workflow *burninv1alpha1.Workflow, meta *renderMetadata) {
	if workflow.Annotations == nil {
		workflow.Annotations = map[string]string{}
	}
	workflow.Annotations["ncrectl.nvidia.com/detected-platform"] = meta.DetectedPlatform
	workflow.Annotations["ncrectl.nvidia.com/detected-gpu-architecture"] = meta.DetectedGPUArchitecture
	if len(meta.AppliedOverrides) > 0 {
		overridesJSON, _ := json.Marshal(meta.AppliedOverrides)
		workflow.Annotations["ncrectl.nvidia.com/applied-overrides"] = string(overridesJSON)
	}
}

// renderDryRun connects to a cluster, discovers real nodes, applies overrides,
// and validates the resolved resources via server-side dry-run.
func renderDryRun(workflowFile string, configFlags *kubeconfig.ConfigFlags) (
	*burninv1alpha1.Workflow, *renderMetadata, []DryRunResult, error) {

	workflow, err := readWorkflow(workflowFile)
	if err != nil {
		return nil, nil, nil, err
	}

	c, err := NewK8sClient(configFlags)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build kubernetes client: %w", err)
	}

	ctx := context.Background()
	nodes, err := controller.DiscoverTargetNodes(ctx, c, workflow.Spec.Orchestration.Target)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("discover nodes: %w", err)
	}

	workflow.TypeMeta = metav1.TypeMeta{
		APIVersion: "cre.nvidia.com/v1alpha1",
		Kind:       "Workflow",
	}

	meta, err := ResolveWorkflow(workflow, nodes)
	if err != nil {
		return nil, nil, nil, err
	}

	results, err := DryRunCreate(ctx, c, *configFlags.Namespace, &workflow.Spec, nodes)
	if err != nil {
		return nil, nil, nil, err
	}

	return workflow, meta, results, nil
}

// NewK8sClient builds a controller-runtime client from the resolved ConfigFlags
// (kubeconfig, context, and every other kubectl-standard connection/auth flag).
func NewK8sClient(cf *kubeconfig.ConfigFlags) (client.Client, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = burninv1alpha1.AddToScheme(s)
	_ = trainerv1alpha1.AddToScheme(s)

	return client.New(restConfig, client.Options{Scheme: s})
}

// NewK8sWatchClient builds a watch-capable controller-runtime client from the
// resolved ConfigFlags.
func NewK8sWatchClient(cf *kubeconfig.ConfigFlags) (client.WithWatch, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = burninv1alpha1.AddToScheme(s)
	_ = trainerv1alpha1.AddToScheme(s)

	return client.NewWithWatch(restConfig, client.Options{Scheme: s})
}

// DryRunCreate validates the resolved Job, workload, and dependencies
// via server-side dry-run without persisting anything.
//
// Validation order: Dependencies → Job → Workload. Dependencies are validated
// first so that we can detect when a workload failure is caused by a missing
// dependency that would be created at runtime (e.g. a TrainingRuntime referenced
// by a TrainJob). Such failures are reported as warnings rather than errors.
func DryRunCreate(ctx context.Context, c client.Client, namespace string,
	spec *burninv1alpha1.WorkflowSpec, nodes []corev1.Node) ([]DryRunResult, error) {

	var results []DryRunResult

	// Build a Job from the template.
	specCopy := spec.JobTemplate.Spec.DeepCopy()

	// Get the workload adapter.
	adapter, err := workload.ForSpec(&specCopy.Workload)
	if err != nil {
		return nil, fmt.Errorf("resolve workload type: %w", err)
	}

	// Set node affinity using the first N nodes (simulating group-0).
	nodesRequired, err := adapter.NodesRequired(&specCopy.Workload)
	if err != nil {
		return nil, fmt.Errorf("determine nodes required: %w", err)
	}
	groupNodes := nodes
	if len(groupNodes) > nodesRequired {
		groupNodes = groupNodes[:nodesRequired]
	}
	nodeNames := make([]string, len(groupNodes))
	for i, n := range groupNodes {
		nodeNames[i] = n.Name
	}
	adapter.SetNodeAffinity(&specCopy.Workload, &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "kubernetes.io/hostname",
					Operator: corev1.NodeSelectorOpIn,
					Values:   nodeNames,
				}},
			}},
		},
	})

	// Set tolerations (tolerate all, same as controller).
	adapter.SetTolerations(&specCopy.Workload, []corev1.Toleration{{
		Operator: corev1.TolerationOpExists,
	}})

	// Set default node health monitor if nil.
	if specCopy.NodeHealthMonitor == nil {
		specCopy.NodeHealthMonitor = &burninv1alpha1.NodeHealthMonitor{
			CEL: &burninv1alpha1.CELNodeHealthCheck{
				Expression: `node.spec.unschedulable == true`,
			},
		}
	}

	// --- 1. Validate dependencies first ---
	var depNames []string
	for i, dep := range spec.Dependencies {
		depObj := &unstructured.Unstructured{}
		if err := json.Unmarshal(dep.Raw, &depObj.Object); err != nil {
			results = append(results, DryRunResult{
				Resource: fmt.Sprintf("Dependency/%d", i),
				Valid:    false,
				Error:    fmt.Sprintf("unmarshal dependency: %v", err),
			})
			continue
		}

		if depObj.GetNamespace() == "" {
			depObj.SetNamespace(namespace)
		}

		resourceName := fmt.Sprintf("%s/%s", depObj.GetKind(), depObj.GetName())
		depResult := DryRunResult{Resource: resourceName, Valid: true}
		if err := c.Create(ctx, depObj, client.DryRunAll); err != nil {
			depResult.Valid = false
			depResult.Error = err.Error()
		} else {
			depNames = append(depNames, depObj.GetName())
		}
		results = append(results, depResult)
	}

	// --- 2. Validate Job ---
	job := &burninv1alpha1.Job{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cre.nvidia.com/v1alpha1",
			Kind:       "Job",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dry-run-job",
			Namespace: namespace,
		},
		Spec: *specCopy,
	}

	jobResult := DryRunResult{Resource: "Job/dry-run-job", Valid: true}
	if err := c.Create(ctx, job, client.DryRunAll); err != nil {
		jobResult.Valid = false
		jobResult.Error = err.Error()
	}
	results = append(results, jobResult)

	// --- 3. Validate workload ---
	wlObj, err := adapter.Build("dry-run-workload", namespace, &specCopy.Workload)
	if err != nil {
		results = append(results, DryRunResult{
			Resource: fmt.Sprintf("%s/dry-run-workload", adapter.GVK().Kind),
			Valid:    false,
			Error:    err.Error(),
		})
	} else {
		wlResult := DryRunResult{
			Resource: fmt.Sprintf("%s/dry-run-workload", adapter.GVK().Kind),
			Valid:    true,
		}
		if err := c.Create(ctx, wlObj, client.DryRunAll); err != nil {
			errMsg := err.Error()
			if isDependencyNotFoundError(errMsg, depNames) {
				wlResult.Valid = true
				wlResult.Warning = "references dependency created at runtime: " + errMsg
			} else {
				wlResult.Valid = false
				wlResult.Error = errMsg
			}
		}
		results = append(results, wlResult)
	}

	return results, nil
}

// isDependencyNotFoundError returns true if the error message indicates a
// "not found" failure that references one of the known dependency names.
// This detects cases where a workload (e.g. TrainJob) fails because its
// dependency (e.g. TrainingRuntime) hasn't been created yet — which is
// expected in dry-run mode since dependencies would be created at runtime.
func isDependencyNotFoundError(errMsg string, depNames []string) bool {
	if !strings.Contains(strings.ToLower(errMsg), "not found") {
		return false
	}
	for _, name := range depNames {
		if strings.Contains(errMsg, name) {
			return true
		}
	}
	return false
}

// PrintDryRunSummary writes a human-readable summary table to stderr.
// Output goes to stderr so that YAML on stdout remains pipeable.
func PrintDryRunSummary(header string, results []DryRunResult) {
	w := tabwriter.NewWriter(os.Stderr, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "\n%s\n", header)
	_, _ = fmt.Fprintln(w, "RESOURCE\tSTATUS\tMESSAGE")
	for _, r := range results {
		switch {
		case !r.Valid:
			_, _ = fmt.Fprintf(w, "%s\tINVALID\t%s\n", r.Resource, r.Error)
		case r.Warning != "":
			_, _ = fmt.Fprintf(w, "%s\tWARN\t%s\n", r.Resource, r.Warning)
		default:
			_, _ = fmt.Fprintf(w, "%s\tOK\t\n", r.Resource)
		}
	}
	_ = w.Flush()
}

func resolveNodes(platform, gpuArch, nodesFile string) ([]corev1.Node, error) {
	// Priority 1: custom nodes file
	if nodesFile != "" {
		return loadNodesFromFile(nodesFile)
	}

	// Priority 2: embedded mock node from --platform + --gpu-arch
	if platform != "" && gpuArch != "" {
		return LoadEmbeddedNodes(platform, gpuArch)
	}

	// No flags: list available combos
	return nil, fmt.Errorf("specify --nodes-file <path> or --platform + --gpu-arch\navailable: %s",
		strings.Join(listAvailable(), ", "))
}

func loadNodesFromFile(path string) ([]corev1.Node, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is a user-provided CLI argument

	if err != nil {
		return nil, fmt.Errorf("read nodes file: %w", err)
	}
	var nodes []corev1.Node
	if err := yaml.Unmarshal(data, &nodes); err != nil {
		return nil, fmt.Errorf("parse nodes file: %w", err)
	}
	return nodes, nil
}

// LoadEmbeddedNodes loads a mock node from the embedded YAML templates for the
// given platform and GPU architecture combination.
func LoadEmbeddedNodes(platform, gpuArch string) ([]corev1.Node, error) {
	filename := fmt.Sprintf("nodes/%s-%s.yaml", platform, gpuArch)
	data, err := embeddedNodes.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("no mock nodes for %s-%s\navailable: %s",
			platform, gpuArch, strings.Join(listAvailable(), ", "))
	}
	var node corev1.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, fmt.Errorf("parse embedded node template: %w", err)
	}
	if node.Name == "" {
		node.Name = "mock-node-0"
	}
	return []corev1.Node{node}, nil
}

func listAvailable() []string {
	entries, _ := embeddedNodes.ReadDir("nodes")
	available := make([]string, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".yaml")
		available = append(available, name)
	}
	return available
}
