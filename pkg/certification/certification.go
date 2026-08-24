// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
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
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/cluster"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/controller"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/naming"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/render"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/report"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/setup"
)

// NewCommand returns the "certification" cobra command.
// version is threaded through to init/setup operations.
func NewCommand(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certification",
		Short: "Certification management commands",
	}
	cmd.AddCommand(newCertificationRenderCommand())
	cmd.AddCommand(newListCategoriesCommand())
	cmd.AddCommand(newRunCommand(version))
	cmd.AddCommand(newReportCommand())
	return cmd
}

func newListCategoriesCommand() *cobra.Command {
	var outputFormat string

	cmd := &cobra.Command{
		Use:   "list-categories",
		Short: "List available catalog categories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runListCategories(outputFormat)
		},
	}

	cmd.Flags().StringVar(&outputFormat, "output", "table", "Output format: table, yaml, or json")
	return cmd
}

const outputJSON = "json"

func runListCategories(format string) error {
	categories := catalog.List()

	switch format {
	case outputJSON:
		data, err := json.MarshalIndent(categories, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Println(string(data))
	case "yaml":
		data, err := yaml.Marshal(categories)
		if err != nil {
			return fmt.Errorf("marshal yaml: %w", err)
		}
		fmt.Print(string(data))
	default: // table
		fmt.Printf("%-20s %s\n", "DOMAIN", "VARIANT")
		for _, c := range categories {
			fmt.Printf("%-20s %s\n", c.Domain, c.Variant)
		}
	}
	return nil
}

func newCertificationRenderCommand() *cobra.Command {
	var outputFormat string
	var dryRun bool
	var platform string

	configFlags := kubeconfig.NewConfigFlags(true)
	*configFlags.Namespace = defaultKubeNamespace
	cmd := &cobra.Command{
		Use:   "render [flags] <certification.yaml>",
		Short: "Render all Workflows from a Certification",
		Long: `Reads a Certification YAML, looks up each category in the catalog,
and renders the Workflows that the controller would create.

With --dry-run, connects to a cluster, applies overrides per Workflow,
and validates resolved resources via server-side dry-run.

Use --platform to simulate platform-specific overrides (e.g., EFA volumes
on AWS) without connecting to a cluster. Valid values: aws, gcp, azure, oci, onprem.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return fmt.Errorf(
					"requires a certification YAML file as argument\n\n" +
						"Usage: ncrectl certification render [flags] <certification.yaml>",
				)
			}
			return runCertificationRender(args[0], outputFormat, dryRun, configFlags, platform)
		},
	}

	cmd.Flags().StringVar(&outputFormat, "output", "yaml", "Output format: yaml or json")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Connect to cluster, discover real nodes, and validate via server-side dry-run")
	cmd.Flags().StringVar(&platform, "platform", "",
		"Simulate platform for override matching (aws, gcp, azure, oci, onprem, togetherai, mistral, forge)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

func runCertificationRender(certFile, outputFormat string, dryRun bool,
	configFlags *kubeconfig.ConfigFlags, platform string) error {
	namespace := *configFlags.Namespace

	if platform != "" {
		valid := map[string]bool{
			"aws":        true,
			"gcp":        true,
			"azure":      true,
			"oci":        true,
			"onprem":     true,
			"togetherai": true,
			"mistral":    true,
			"forge":      true,
		}
		if !valid[platform] {
			return fmt.Errorf(
				"invalid --platform %q: must be one of aws, gcp, azure, oci, onprem, togetherai, mistral, forge",
				platform)
		}
	}

	cert, err := readCertification(certFile)
	if err != nil {
		return err
	}

	// When --dry-run, connect early so we can auto-detect GPU architecture
	// from cluster nodes if not specified in the nodeSelector.
	var dryRunClient client.Client
	var dryRunNodes []corev1.Node
	if dryRun {
		var cErr error
		dryRunClient, cErr = render.NewK8sClient(configFlags)
		if cErr != nil {
			return fmt.Errorf("build kubernetes client: %w", cErr)
		}

		if catalog.GPUArchFromNodeSelector(cert.Spec.Target.NodeSelector) == "" {
			ctx := context.Background()
			_, gpuProduct, gpuErr := cluster.DiscoverGPUNodes(ctx, dryRunClient, &cert.Spec.Target)
			if gpuErr != nil {
				return gpuErr
			}
			if cert.Spec.Target.NodeSelector == nil {
				cert.Spec.Target.NodeSelector = make(map[string]string)
			}
			cert.Spec.Target.NodeSelector["nvidia.com/gpu.product"] = gpuProduct
		}
	}

	workflows, err := renderCertification(cert, platform)
	if err != nil {
		return err
	}

	// Collect dry-run results per workflow for summary output after YAML.
	type workflowResults struct {
		name    string
		results []render.DryRunResult
	}
	var allResults []workflowResults

	if dryRun {
		ctx := context.Background()
		nodes := dryRunNodes
		if len(nodes) == 0 {
			var nErr error
			nodes, nErr = controller.DiscoverTargetNodes(ctx, dryRunClient, workflows[0].Spec.Orchestration.Target)
			if nErr != nil {
				return fmt.Errorf("discover nodes: %w", nErr)
			}
		}

		for i := range workflows {
			meta, err := render.ResolveWorkflow(&workflows[i], nodes)
			if err != nil {
				return fmt.Errorf("resolve workflow %s: %w", workflows[i].Name, err)
			}
			render.SetRenderAnnotations(&workflows[i], meta)

			results, dryRunErr := render.DryRunCreate(ctx, dryRunClient, namespace, &workflows[i].Spec, nodes)
			if dryRunErr != nil {
				return fmt.Errorf("dry-run workflow %s: %w", workflows[i].Name, dryRunErr)
			}
			allResults = append(allResults, workflowResults{
				name:    workflows[i].Name,
				results: results,
			})
		}
	} else {
		// Apply overrides using a synthetic node derived from the Certification's
		// nodeSelector. This enables GPU-architecture-specific overrides (images,
		// env vars) to be applied even without connecting to a real cluster.
		syntheticNode := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Labels: cert.Spec.Target.NodeSelector},
		}
		if platform != "" {
			syntheticNode.Spec.ProviderID = platformToProviderID(platform)
			if platform == "togetherai" {
				if syntheticNode.Labels == nil {
					syntheticNode.Labels = map[string]string{}
				}
				syntheticNode.Labels["node-role.together.ai/worker"] = ""
			}
			if platform == "forge" {
				if syntheticNode.Labels == nil {
					syntheticNode.Labels = map[string]string{}
				}
				syntheticNode.Labels["kubernetes.io/hostname"] = "synthetic-forge-node"
			}
		}
		syntheticNodes := []corev1.Node{syntheticNode}
		for i := range workflows {
			if _, err := render.ResolveWorkflow(&workflows[i], syntheticNodes); err != nil {
				return fmt.Errorf("resolve overrides for %s: %w", workflows[i].Name, err)
			}
		}
	}

	switch outputFormat {
	case outputJSON:
		data, err := json.MarshalIndent(workflows, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal json: %w", err)
		}
		fmt.Println(string(data))
	default: // yaml
		for i, wf := range workflows {
			if i > 0 {
				fmt.Println("---")
			}
			data, err := yaml.Marshal(wf)
			if err != nil {
				return fmt.Errorf("marshal yaml: %w", err)
			}
			fmt.Print(string(data))
		}
	}

	for _, wr := range allResults {
		render.PrintDryRunSummary("Dry-run validation: "+wr.name, wr.results)
	}

	return nil
}

// renderCertification builds all Workflows that the controller would create
// from catalog entries for the given Certification. The platform argument
// (from --platform or detected from cluster nodes) is used to resolve
// platform-specific node defaults like OCI L40s {gpusPerNode: 4, mlnxPerNode: 2}
// at template-render time. Pass "" to use architecture defaults only.
func renderCertification(cert *crev1alpha1.Certification, platform string) ([]crev1alpha1.Workflow, error) {
	if len(cert.Spec.Categories) == 0 {
		return nil, fmt.Errorf("certification has no categories")
	}

	gpuArch := catalog.GPUArchFromNodeSelector(cert.Spec.Target.NodeSelector)
	if gpuArch == "" {
		return nil, fmt.Errorf(
			"cannot determine GPU architecture from target nodeSelector" +
				" (nvidia.com/gpu.product label is required)",
		)
	}

	var workflows []crev1alpha1.Workflow
	for _, cat := range cert.Spec.Categories {
		entry := catalog.Lookup(cat.Domain, cat.Variant)
		if entry == nil {
			return nil, fmt.Errorf("unknown certification category: %s/%s", cat.Domain, cat.Variant)
		}

		opts := controller.ResolveOptions(&cert.Spec.CategoryOptions, cat.Options)

		nd := catalog.GPUDefaults(gpuArch, platform)
		gpusPerNode := nd.GpusPerNode
		mlnxPerNode := nd.MlnxPerNode
		if opts.GpusPerNode != nil {
			gpusPerNode = *opts.GpusPerNode
		}
		if opts.MlnxPerNode != nil {
			mlnxPerNode = *opts.MlnxPerNode
		}

		enableMNNVL := controller.DefaultEnableMNNVL(gpuArch)
		if opts.EnableMNNVL != nil {
			enableMNNVL = *opts.EnableMNNVL
		}

		// Resolve nodesPerJob for offline render (no cluster access).
		nodesPerJob := int32(0)
		if opts.NodesPerJob != nil {
			nodesPerJob = *opts.NodesPerJob
		}
		if nodesPerJob == 0 {
			// Default to 1 node for offline render when nodesPerJob is not set.
			nodesPerJob = 1
		}

		workflowSpec, buildErr := entry.Build(cert.Spec.Target, catalog.BuildConfig{
			ImagePullSecrets:   opts.ImagePullSecrets,
			StorageClassName:   opts.StorageClassName,
			NodesPerJob:        nodesPerJob,
			GpusPerNode:        gpusPerNode,
			MlnxPerNode:        mlnxPerNode,
			EnableMNNVL:        enableMNNVL,
			EnableCheckpoint:   derefBoolPtr(opts.EnableCheckpoint),
			MaxSteps:           derefInt32Ptr(opts.MaxSteps),
			ExitDurationMins:   derefInt32Ptr(opts.ExitDurationMins),
			GPUArchitecture:    gpuArch,
			SaveInterval:       derefInt32Ptr(opts.SaveInterval),
			SaveRetainInterval: derefInt32Ptr(opts.SaveRetainInterval),
			SaveTopK:           derefInt32Ptr(opts.SaveTopK),
			StorageSize:        opts.StorageSize,
			TestScale:          opts.TestScale,
			MaxBytes:           opts.MaxBytes,
			NumIterations:      derefInt32Ptr(opts.NumIterations),
			NumCycles:          derefInt32Ptr(opts.NumCycles),
			Thresholds:         opts.Thresholds,
			MaxConcurrent:      derefInt32Ptr(opts.MaxConcurrent),
			// These five were missing, so render previewed catalog defaults
			// rather than the user's settings. certification_controller.go
			// passes all of them, which is why an applied run was correct while
			// the preview was not.
			MinGroupSize:       derefInt32Ptr(opts.MinGroupSize),
			RepeatCount:        derefInt32Ptr(opts.RepeatCount),
			MaxRestarts:        derefInt32Ptr(opts.MaxRestarts),
			TimeoutPerJob:      opts.TimeoutPerJob,
			MeasurementTimeout: opts.MeasurementTimeout,
		})
		if buildErr != nil {
			return nil, fmt.Errorf("building workflow for %s/%s: %w", cat.Domain, cat.Variant, buildErr)
		}

		workflowName := naming.Truncate(
			fmt.Sprintf("%s-%s-%s", cert.Name, cat.Domain, cat.Variant),
			naming.MaxWorkflowNameLen,
		)

		workflow := crev1alpha1.Workflow{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "cre.nvidia.com/v1alpha1",
				Kind:       "Workflow",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: workflowName,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by":    "cluster-readiness-engine",
					"cre.nvidia.com/certification":    cert.Name,
					"cre.nvidia.com/category-domain":  cat.Domain,
					"cre.nvidia.com/category-variant": cat.Variant,
				},
			},
			Spec: workflowSpec,
		}

		workflows = append(workflows, workflow)
	}

	return workflows, nil
}

// derefBoolPtr returns the value pointed to by p, or false if p is nil.
func derefBoolPtr(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// derefInt32Ptr returns the value pointed to by p, or 0 if p is nil.
func derefInt32Ptr(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

// readCertification parses a Certification YAML file from disk.
func readCertification(path string) (*crev1alpha1.Certification, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is a user-provided CLI argument

	if err != nil {
		return nil, fmt.Errorf("read certification: %w", err)
	}
	var cert crev1alpha1.Certification
	if err := yaml.Unmarshal(data, &cert); err != nil {
		return nil, fmt.Errorf("parse certification: %w", err)
	}
	return &cert, nil
}

// platformToProviderID is now syntheticProviderID in run_common.go.
func platformToProviderID(platform string) string {
	return render.SyntheticProviderID(platform)
}

// ---------------------------------------------------------------------------
// ncrectl certification run
// ---------------------------------------------------------------------------

// certRunConfig captures the fully-resolved intent for a certification run.
// Built from either CLI flags (--category) or a YAML file (--cert-file),
// then consumed by the single executeCertificationRun pipeline.
type certRunConfig struct {
	version                  string // CLI version, passed to setup.RunInit
	cert                     *crev1alpha1.Certification
	namespace                string
	controllerImage          string // --image: controller image for --setup
	controllerPullSecret     string // --controller-pull-secret: controller registry token forwarded to setup init
	workloadRegistry         string // --workload-registry: workload registry hostname
	workloadRegistryUsername string // --workload-registry-username: workload registry username
	workloadRegistryPassword string // --workload-registry-password: workload registry password/token
	doWait                   bool   // --wait: watch + report
	doSetup                  bool   // --setup: runInit before create
	doCleanup                bool   // --cleanup: runReset + delete cert/ns after
	resultsFile              string // --results-file: path to write JSON report
	timeout                  time.Duration
	configFlags              *kubeconfig.ConfigFlags
	out                      io.Writer
}

// categoryRunOpts holds the optional CategoryOptions flags for the --category path.
// Bool pointers are nil when the user did not pass the flag (use controller default).
type categoryRunOpts struct {
	enableCheckpoint *bool
	maxSteps         int32
	exitDurationMins int32
	gpusPerNode      int32
	enableMNNVL      *bool
	storageClass     string
	repeatCount      int32
	maxRestarts      int32
}

func newRunCommand(version string) *cobra.Command {
	var categories []string
	var name string
	var nodesPerJob, maxSteps, exitDurationMins, gpusPerNode, repeatCount, maxRestarts int32
	var enableCheckpoint, enableMNNVL bool
	var storageClass string
	var doWait, doSetup, doCleanup bool
	var certFile string
	var controllerImage string
	var controllerPullSecret string
	var workloadRegistry, workloadRegistryUsername, workloadRegistryPassword string
	var resultsFile string
	var timeout time.Duration

	configFlags := kubeconfig.NewConfigFlags(true)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run a certification on the target cluster",
		Long: `Creates a Certification resource in the cluster from the specified categories
or from a Certification YAML file.

Categories are selected from the catalog (see 'ncrectl certification list-categories').
The default node selector targets all GPU nodes (nvidia.com/gpu.present=true).

Use --cert-file to load a Certification from YAML (mutually exclusive with --category).
Use --setup to install CRDs, controller, and LogProfiles before creating the cert.
Use --wait to watch for completion and print a report.
Use --cleanup to teardown installed components after completion.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if certFile != "" && len(categories) > 0 {
				return fmt.Errorf("--cert-file and --category are mutually exclusive")
			}
			// All three workload-registry flags must be set together and non-empty.
			// Check values directly (not just Changed) so --workload-registry-password ""
			// with NGC_KEY unset is caught here rather than silently producing ImagePullBackOff.
			pullSet := 0
			for _, v := range []string{workloadRegistry, workloadRegistryUsername, workloadRegistryPassword} {
				if v != "" {
					pullSet++
				}
			}
			if pullSet > 0 && pullSet < 3 {
				return fmt.Errorf("--workload-registry, --workload-registry-username, and --workload-registry-password must all be set together and non-empty")
			}
			if certFile == "" && len(categories) == 0 {
				return fmt.Errorf("either --cert-file or at least one --category is required\n\n" +
					"Use 'ncrectl certification list-categories' to see available categories")
			}

			var cfg *certRunConfig
			var err error
			if certFile != "" {
				cfg, err = buildConfigFromFile(certFile, *configFlags.Namespace, controllerImage,
					controllerPullSecret, workloadRegistry, workloadRegistryUsername, workloadRegistryPassword,
					doWait, doSetup, doCleanup, timeout, configFlags, os.Stderr)
			} else {
				opts := categoryRunOpts{
					maxSteps:         maxSteps,
					exitDurationMins: exitDurationMins,
					gpusPerNode:      gpusPerNode,
					storageClass:     storageClass,
					repeatCount:      repeatCount,
					maxRestarts:      maxRestarts,
				}
				if cmd.Flags().Changed("enable-checkpoint") {
					opts.enableCheckpoint = &enableCheckpoint
				}
				if cmd.Flags().Changed("enable-mnnvl") {
					opts.enableMNNVL = &enableMNNVL
				}
				cfg, err = buildConfigFromFlags(categories, name, *configFlags.Namespace, nodesPerJob,
					opts, controllerImage, controllerPullSecret, workloadRegistry, workloadRegistryUsername, workloadRegistryPassword,
					doWait, doSetup, doCleanup, timeout, configFlags, os.Stderr)
			}
			if err != nil {
				return err
			}
			cfg.version = version
			cfg.resultsFile = resultsFile
			return executeCertificationRun(cfg)
		},
	}

	cmd.Flags().StringSliceVar(&categories, "category", nil,
		"Category to run in domain/variant format (repeatable)")
	cmd.Flags().StringVar(&certFile, "cert-file", "",
		"Certification YAML file (mutually exclusive with --category)")
	cmd.Flags().BoolVar(&doSetup, "setup", false,
		"Install CRDs, controller, and LogProfiles before creating the certification")
	cmd.Flags().BoolVar(&doWait, "wait", false,
		"Watch for completion and print a report")
	cmd.Flags().BoolVar(&doCleanup, "cleanup", false,
		"Delete certification, namespace, and installed components after completion")
	cmd.Flags().StringVar(&controllerPullSecret, "controller-pull-secret", "",
		"Token for controller registry authentication during --setup (e.g. GitHub PAT for ghcr.io) — separate from workload image credentials")
	cmd.Flags().StringVar(&workloadRegistry, "workload-registry", "",
		"Registry server for workload image pull (e.g. nvcr.io, ghcr.io) — required when --workload-registry-password is set")
	cmd.Flags().StringVar(&workloadRegistryUsername, "workload-registry-username", "",
		"Registry username for workload image pull (e.g. \\$oauthtoken for NGC) — required when --workload-registry-password is set")
	cmd.Flags().StringVar(&workloadRegistryPassword, "workload-registry-password", "",
		"Registry password or API key for workload image pull — creates an workloadRegistryPassword in the certification namespace")
	cmd.Flags().StringVar(&controllerImage, "image", "",
		"Controller image for --setup (default: ghcr.io/dsx-ai-factory/cluster-readiness-engine/manager:<version>)")
	cmd.Flags().StringVar(&name, "name", "",
		"Certification name (default: ncrectl-<timestamp>)")
	cmd.Flags().Int32Var(&nodesPerJob, "nodes-per-job", 0,
		"Number of nodes per job (0 = auto-select: all nodes for non-training, largest config for training)")
	cmd.Flags().BoolVar(&enableCheckpoint, "enable-checkpoint", false,
		"Enable checkpoint storage for training workloads")
	cmd.Flags().Int32Var(&maxSteps, "max-steps", 0,
		"Max training steps for NeMo 4 workloads (0 = use catalog default)")
	cmd.Flags().Int32Var(&exitDurationMins, "exit-duration-mins", 0,
		"Training duration in minutes for NeMo 6 workloads (0 = use catalog default)")
	cmd.Flags().Int32Var(&gpusPerNode, "gpus-per-node", 0,
		"GPUs per node (0 = auto-detect from GPU architecture)")
	cmd.Flags().BoolVar(&enableMNNVL, "enable-mnnvl", false,
		"Enable Multi-Node NVLink (NCCL_MNNVL_ENABLE=1)")
	cmd.Flags().Int32Var(&repeatCount, "repeat-count", 0,
		"Number of orchestration iterations to repeat tests (0 = use catalog default)")
	cmd.Flags().Int32Var(&maxRestarts, "max-restarts", 0,
		"Maximum checkpoint restarts for training workloads (0 = use catalog default)")
	cmd.Flags().StringVar(&storageClass, "storage-class", "",
		"StorageClass for PVC dependencies created by catalog entries")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute,
		"Timeout for --wait")
	cmd.Flags().StringVar(&resultsFile, "results-file", "",
		"Write certification report as JSON to this file path (requires --wait)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// ---------------------------------------------------------------------------
// Config builders — the only place the two input modes diverge
// ---------------------------------------------------------------------------

// buildConfigFromFlags builds a certRunConfig from --category CLI flags.
// Connects to the cluster to discover GPU product.
func buildConfigFromFlags(
	categoryStrs []string, name, namespace string, nodesPerJob int32,
	opts categoryRunOpts, controllerImage, controllerPullSecret, workloadRegistry, workloadRegistryUsername, workloadRegistryPassword string,
	doWait, doSetup, doCleanup bool, timeout time.Duration,
	configFlags *kubeconfig.ConfigFlags, out io.Writer,
) (*certRunConfig, error) {
	cats, err := parseCategories(categoryStrs)
	if err != nil {
		return nil, err
	}

	if name == "" {
		name = generateCertName()
	}
	if namespace == "" {
		namespace = generateCertNamespace()
	}

	// Connect to discover GPU product.
	c, err := render.NewK8sClient(configFlags)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	ctx := context.Background()

	_, gpuProduct, err := cluster.DiscoverGPUNodes(ctx, c, nil)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(out, "Discovered GPU nodes with product: %s\n", gpuProduct)

	cert := &crev1alpha1.Certification{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "cre.nvidia.com/v1alpha1",
			Kind:       "Certification",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "cluster-readiness-engine",
				"app.kubernetes.io/created-by": "ncrectl",
			},
		},
		Spec: crev1alpha1.CertificationSpec{
			Target: crev1alpha1.TargetSpec{
				NodeSelector: map[string]string{
					"nvidia.com/gpu.present": "true",
					"nvidia.com/gpu.product": gpuProduct,
				},
			},
			Categories: cats,
		},
	}

	// Apply CategoryOptions from CLI flags.
	if nodesPerJob > 0 {
		cert.Spec.NodesPerJob = &nodesPerJob
	}
	if opts.enableCheckpoint != nil {
		cert.Spec.EnableCheckpoint = opts.enableCheckpoint
	}
	if opts.maxSteps > 0 {
		cert.Spec.MaxSteps = &opts.maxSteps
	}
	if opts.exitDurationMins > 0 {
		cert.Spec.ExitDurationMins = &opts.exitDurationMins
	}
	if opts.gpusPerNode > 0 {
		cert.Spec.GpusPerNode = &opts.gpusPerNode
	}
	if opts.enableMNNVL != nil {
		cert.Spec.EnableMNNVL = opts.enableMNNVL
	}
	if opts.storageClass != "" {
		cert.Spec.StorageClassName = &opts.storageClass
	}
	if opts.repeatCount > 0 {
		cert.Spec.RepeatCount = &opts.repeatCount
	}
	if opts.maxRestarts > 0 {
		cert.Spec.MaxRestarts = &opts.maxRestarts
	}

	return &certRunConfig{
		cert:                     cert,
		namespace:                namespace,
		controllerImage:          controllerImage,
		controllerPullSecret:     controllerPullSecret,
		workloadRegistry:         workloadRegistry,
		workloadRegistryUsername: workloadRegistryUsername,
		workloadRegistryPassword: workloadRegistryPassword,
		doWait:                   doWait,
		doSetup:                  doSetup,
		doCleanup:                doCleanup,
		timeout:                  timeout,
		configFlags:              configFlags,
		out:                      out,
	}, nil
}

// buildConfigFromFile builds a certRunConfig from a --cert-file YAML.
func buildConfigFromFile(
	certFile, namespace, controllerImage, controllerPullSecret, workloadRegistry, workloadRegistryUsername, workloadRegistryPassword string,
	doWait, doSetup, doCleanup bool, timeout time.Duration,
	configFlags *kubeconfig.ConfigFlags, out io.Writer,
) (*certRunConfig, error) {
	cert, err := readCertification(certFile)
	if err != nil {
		return nil, err
	}

	if cert.Namespace == "" {
		if namespace != "" {
			cert.Namespace = namespace
		} else {
			cert.Namespace = generateCertNamespace()
		}
	}

	return &certRunConfig{
		cert:                     cert,
		namespace:                cert.Namespace,
		controllerImage:          controllerImage,
		controllerPullSecret:     controllerPullSecret,
		workloadRegistry:         workloadRegistry,
		workloadRegistryUsername: workloadRegistryUsername,
		workloadRegistryPassword: workloadRegistryPassword,
		doWait:                   doWait,
		doSetup:                  doSetup,
		doCleanup:                doCleanup,
		timeout:                  timeout,
		configFlags:              configFlags,
		out:                      out,
	}, nil
}

// ---------------------------------------------------------------------------
// Single execution pipeline (ADR-050)
// ---------------------------------------------------------------------------

// executeCertificationRun is the unified pipeline for all certification run paths.
// Setup, wait, and cleanup are composable phases controlled by the config flags.
func executeCertificationRun(cfg *certRunConfig) (pipelineErr error) {
	ctx := context.Background()
	out := cfg.out

	// --- Signal handling ---
	if cfg.doSetup || cfg.doCleanup {
		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
	}

	// --- Create client early so cleanup defer can use it ---
	wc, err := render.NewK8sWatchClient(cfg.configFlags)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	// --- Cleanup defer (registered BEFORE setup so it runs on setup failures) ---
	certCreated := false
	createdNamespace := ""

	if cfg.doCleanup {
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cleanupCancel()

			_, _ = fmt.Fprintln(out, "[cleanup] Cleaning up...")
			warnings := false

			// Delete certification resource and wait for the controller to
			// cascade-delete all Workflows, Jobs, and workloads. The controller
			// needs its RBAC and Deployment to process this cascade, so we MUST
			// wait for the Certification to be fully gone before running reset.
			if certCreated {
				_, _ = fmt.Fprintln(out, "[cleanup] Deleting certification...")
				if err := wc.Delete(cleanupCtx, cfg.cert); err != nil && !apierrors.IsNotFound(err) {
					_, _ = fmt.Fprintf(out, "[cleanup] Warning: failed to delete certification: %v\n", err)
					warnings = true
				} else {
					waitForDeletion(cleanupCtx, wc, cfg.cert.Name, cfg.cert.Namespace, out)
				}
			}

			// Note: the workload pull secret is owned by the Certification and
			// will be garbage-collected automatically when the Certification is
			// deleted above. No explicit deletion needed.

			// Delete namespace if we created it, and wait for it to fully
			// terminate so the next run doesn't fail with "namespace is being
			// terminated".
			if createdNamespace != "" {
				_, _ = fmt.Fprintf(out, "[cleanup] Deleting namespace %s...\n", createdNamespace)
				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: createdNamespace}}
				if err := wc.Delete(cleanupCtx, ns); err != nil && !apierrors.IsNotFound(err) {
					_, _ = fmt.Fprintf(out, "[cleanup] Warning: failed to delete namespace %s: %v\n", createdNamespace, err)
					warnings = true
				} else {
					setup.WaitForNamespaceDeletion(cleanupCtx, wc, createdNamespace, out)
				}
			}

			// Reset all installed phases via runReset. SSA makes setup
			// idempotent, so we reset everything unconditionally.
			if cfg.doSetup {
				if err := setup.RunReset("", true, cfg.configFlags, nil, out); err != nil {
					_, _ = fmt.Fprintf(out, "[cleanup] Warning: reset failed: %v\n", err)
					warnings = true
				}
			}

			if warnings {
				_, _ = fmt.Fprintln(out, "[cleanup] Done (with warnings).")
			} else {
				_, _ = fmt.Fprintln(out, "[cleanup] Done.")
			}
		}()
	}

	// --- Setup phase: install via runInit (SSA is idempotent) ---
	if cfg.doSetup {
		_, _ = fmt.Fprintln(out, "[setup] Installing dependencies...")
		initErr := setup.RunInit(cfg.version, cfg.controllerImage, cfg.controllerPullSecret, "", true,
			cfg.configFlags, "", nil, out)
		if initErr != nil {
			return fmt.Errorf("[setup] %w", initErr)
		}
	}

	// --- Ensure namespace ---
	wasCreated, err := setup.EnsureNamespace(ctx, wc, cfg.namespace, out)
	if err != nil {
		return err
	}
	if wasCreated {
		createdNamespace = cfg.namespace
	}

	// --- Create image pull secret before the Certification so pods can pull
	// immediately. The secret is created without an owner first; after the
	// Certification is created we set an OwnerReference so Kubernetes GC deletes
	// it automatically whenever the Certification is deleted by any means.
	// wasCreated is true only when a new secret was made (false = pre-existing
	// secret was updated). Only delete on rollback when wasCreated is true.
	wasCreatedByUs := false
	if cfg.workloadRegistryPassword != "" {
		secretName, created, secretErr := setup.CreateImagePullSecret(ctx, wc,
			cfg.namespace, setup.WorkloadPullSecretName(cfg.cert.Name),
			cfg.workloadRegistry, cfg.workloadRegistryUsername, cfg.workloadRegistryPassword)
		if secretErr != nil {
			return fmt.Errorf("create image pull secret: %w", secretErr)
		}
		wasCreatedByUs = created
		cfg.cert.Spec.ImagePullSecrets = append(cfg.cert.Spec.ImagePullSecrets,
			corev1.LocalObjectReference{Name: secretName})
		_, _ = fmt.Fprintf(out, "Created image pull secret %q in namespace %s.\n", secretName, cfg.namespace)
	}

	// --- Create Certification ---
	if err := wc.Create(ctx, cfg.cert); err != nil {
		// Clean up only if we actually created the secret (not if we updated one
		// belonging to a concurrent run). Attempt delete unconditionally — if
		// namespace deletion will cascade later, the delete will be a no-op NotFound.
		if wasCreatedByUs {
			pullSec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: setup.WorkloadPullSecretName(cfg.cert.Name), Namespace: cfg.namespace,
			}}
			_ = wc.Delete(ctx, pullSec)
		}
		return fmt.Errorf("create certification: %w", err)
	}
	certCreated = true

	// --- Set OwnerReference on pull secret now that we have the Certification UID ---
	// Only when wasCreatedByUs: if we only updated an existing secret we don't own
	// it and must not manage its lifecycle. Uses Update (optimistic concurrency) to
	// avoid JSON Merge Patch replacing the OwnerReferences array.
	if wasCreatedByUs {
		sec := &corev1.Secret{}
		if getErr := wc.Get(ctx, client.ObjectKey{Name: setup.WorkloadPullSecretName(cfg.cert.Name), Namespace: cfg.namespace}, sec); getErr != nil {
			_, _ = fmt.Fprintf(out, "Warning: could not retrieve pull secret %q to set OwnerReference: %v\n",
				setup.WorkloadPullSecretName(cfg.cert.Name), getErr)
		} else {
			sec.OwnerReferences = append(sec.OwnerReferences, metav1.OwnerReference{
				APIVersion: "cre.nvidia.com/v1alpha1",
				Kind:       "Certification",
				Name:       cfg.cert.Name,
				UID:        cfg.cert.UID,
			})
			if updateErr := wc.Update(ctx, sec); updateErr != nil {
				_, _ = fmt.Fprintf(out, "Warning: could not set OwnerReference on pull secret %q — it will not be GC'd automatically: %v\n",
					setup.WorkloadPullSecretName(cfg.cert.Name), updateErr)
			}
		}
	}

	_, _ = fmt.Fprintf(out, "Certification %s created in namespace %s.\n",
		cfg.cert.Name, cfg.namespace)
	_, _ = fmt.Fprintln(out, "Categories:")
	for _, cat := range cfg.cert.Spec.Categories {
		_, _ = fmt.Fprintf(out, "  - %s/%s\n", cat.Domain, cat.Variant)
	}

	// --- Wait phase ---
	if !cfg.doWait {
		_, _ = fmt.Fprintf(out, "\nTo check status:\n")
		_, _ = fmt.Fprintf(out,
			"  kubectl get certification %s -n %s\n", cfg.cert.Name, cfg.namespace)
		return nil
	}

	_, _ = fmt.Fprintln(out)
	finalCert, waitErr := watchCertification(ctx, wc, cfg.cert.Name, cfg.namespace, cfg.timeout, out)

	// --- Report phase (only when we have a cert; nil on timeout or watch failure) ---
	if finalCert != nil {
		handleReport(ctx, wc, finalCert, cfg.resultsFile, waitErr, out)
	}

	pipelineErr = waitErr
	return pipelineErr
}

// handleReport builds and prints the certification report, optionally writing
// it to a JSON file. Caller must pass a non-nil cert.
func handleReport(
	ctx context.Context, wc client.WithWatch,
	cert *crev1alpha1.Certification, resultsFile string,
	waitErr error, out io.Writer,
) {
	r := report.Build(ctx, wc, cert)
	dest := os.Stdout
	if waitErr != nil {
		dest = os.Stderr
	}
	report.Print(dest, r)

	if resultsFile != "" {
		if err := report.WriteJSON(resultsFile, []*report.CertReport{r}); err != nil {
			_, _ = fmt.Fprintf(out, "Warning: failed to write results file %s: %v\n", resultsFile, err)
		} else {
			_, _ = fmt.Fprintf(out, "Results written to %s\n", resultsFile)
		}
	}
}

// discoverGPUProduct is replaced by discoverGPUNodes in run_common.go.

// parseCategories splits "domain/variant" strings, validates each against the catalog.
func parseCategories(strs []string) ([]crev1alpha1.CertificateCategory, error) {
	var cats []crev1alpha1.CertificateCategory
	for _, s := range strs {
		parts := strings.SplitN(s, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf(
				"invalid category format %q: expected domain/variant", s)
		}
		domain, variant := parts[0], parts[1]
		if catalog.Lookup(domain, variant) == nil {
			available := catalog.List()
			var names []string
			for _, c := range available {
				names = append(names, c.Domain+"/"+c.Variant)
			}
			return nil, fmt.Errorf(
				"unknown category %q\n\nAvailable categories:\n  %s",
				s, strings.Join(names, "\n  "))
		}
		cats = append(cats, crev1alpha1.CertificateCategory{
			Domain:  domain,
			Variant: variant,
		})
	}
	return cats, nil
}

// generateCertName creates a unique certification name with a timestamp.
func generateCertName() string {
	return fmt.Sprintf("ncrectl-%s", time.Now().Format("20060102-150405"))
}

// generateCertNamespace creates a unique namespace for the certification run.
func generateCertNamespace() string {
	return fmt.Sprintf("ncrectl-%s", time.Now().Format("20060102-150405"))
}

// ---------------------------------------------------------------------------
// Lifecycle helpers
// ---------------------------------------------------------------------------

// watchCertification watches the Certification via K8s watch mechanism
// and returns the final Certification object on terminal condition.
// The watch is automatically reconnected when the API server closes it
// (server-side watch timeout, typically 5-10 minutes).
func watchCertification(
	ctx context.Context, wc client.WithWatch,
	name, namespace string, timeout time.Duration, out io.Writer,
) (*crev1alpha1.Certification, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	_, _ = fmt.Fprintln(out, "[watch] Watching certification progress...")

	start := time.Now()
	lastStatuses := map[string]string{}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		// (Re)establish watch. The API server closes watches after a
		// server-side timeout (5-10 min). We reconnect until the client
		// timeout (--timeout) expires or a terminal condition is seen.
		certList := &crev1alpha1.CertificationList{}
		watcher, err := wc.Watch(ctx, certList, client.InNamespace(namespace),
			client.MatchingFields{"metadata.name": name})
		if err != nil {
			if ctx.Err() != nil {
				elapsed := time.Since(start).Truncate(time.Second)
				return nil, fmt.Errorf("certification did not complete within %s (ran for %s)", timeout, elapsed)
			}
			return nil, fmt.Errorf("start watch: %w", err)
		}

		cert, done, watchErr := processWatchEvents(ctx, wc, watcher, start, lastStatuses, heartbeat, out)
		watcher.Stop()
		if done {
			return cert, watchErr
		}
		// Watch channel closed by API server — reconnect unless timed out.
		if ctx.Err() != nil {
			elapsed := time.Since(start).Truncate(time.Second)
			return nil, fmt.Errorf("certification did not complete within %s (ran for %s)", timeout, elapsed)
		}
	}
}

// categoryWatchLabels returns one display label per entry in
// cert.Status.CategoryStatuses. Labels are "domain/variant"; when the
// certification contains two or more categories with the same domain/variant,
// an "(MNNVL Enabled)"/"(MNNVL Disabled)" suffix — matching the report's
// MNNVL label — disambiguates the duplicates.
func categoryWatchLabels(cert *crev1alpha1.Certification) []string {
	counts := map[string]int{}
	for _, cs := range cert.Status.CategoryStatuses {
		counts[cs.Domain+"/"+cs.Variant]++
	}
	labels := make([]string, len(cert.Status.CategoryStatuses))
	for i, cs := range cert.Status.CategoryStatuses {
		key := cs.Domain + "/" + cs.Variant
		if counts[key] > 1 {
			if mnnvl := report.CategoryMNNVL(cert, i); mnnvl != "" {
				key = fmt.Sprintf("%s (MNNVL %s)", key, mnnvl)
			}
		}
		labels[i] = key
	}
	return labels
}

// processWatchEvents handles events from a single watch session.
// Returns (cert, true, err) on terminal condition, or (nil, false, nil)
// when the watch channel closes and should be reconnected.
func processWatchEvents(
	ctx context.Context, c client.Client, watcher watch.Interface, start time.Time,
	lastStatuses map[string]string, heartbeat *time.Ticker, out io.Writer,
) (*crev1alpha1.Certification, bool, error) {
	events := watcher.ResultChan()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return nil, false, nil // watch closed by server, reconnect
			}
			if event.Type == watch.Error {
				if ctx.Err() != nil {
					return nil, true, nil
				}
				return nil, false, nil // transient error, reconnect
			}

			cert, isCert := event.Object.(*crev1alpha1.Certification)
			if !isCert {
				continue
			}
			elapsed := time.Since(start).Truncate(time.Second)

			// Print category status changes.
			labels := categoryWatchLabels(cert)
			for i, cs := range cert.Status.CategoryStatuses {
				key := labels[i]
				if cs.Status != lastStatuses[key] {
					_, _ = fmt.Fprintf(out, "[watch] %s: %s (%s)\n",
						key, cs.Status, elapsed)
					lastStatuses[key] = cs.Status
				}
			}

			// Check terminal conditions.
			if apimeta.IsStatusConditionTrue(
				cert.Status.Conditions, crev1alpha1.CertificationSucceeded) {
				_, _ = fmt.Fprintf(out, "[watch] Certification succeeded. (%s)\n", elapsed)
				return cert, true, nil
			}
			if apimeta.IsStatusConditionTrue(
				cert.Status.Conditions, crev1alpha1.CertificationFailed) {
				_, _ = fmt.Fprintf(out, "[watch] Certification failed. (%s)\n", elapsed)
				if nodes := certFailedNodes(ctx, c, cert); len(nodes) > 0 {
					_, _ = fmt.Fprintf(out, "Failed nodes: %s\n",
						strings.Join(nodes, ", "))
				}
				return cert, true, fmt.Errorf("certification failed")
			}

		case <-heartbeat.C:
			elapsed := time.Since(start).Truncate(time.Second)
			for _, cs := range lastStatuses {
				if cs == "InProgress" {
					for key, status := range lastStatuses {
						if status == "InProgress" {
							_, _ = fmt.Fprintf(out, "[watch] %s: %s (%s)\n",
								key, status, elapsed)
						}
					}
					break
				}
			}
			if len(lastStatuses) == 0 {
				_, _ = fmt.Fprintf(out, "[watch] Waiting for status... (%s)\n", elapsed)
			}
		}
	}
}

// waitForDeletion waits for a Certification to be fully deleted.
// The timeout is generous because the controller must cascade-delete all
// Workflows, Jobs, workloads, and dependencies before the Certification
// finalizer is removed. The controller needs its RBAC to process this.
func waitForDeletion(ctx context.Context, c client.Client, name, namespace string, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(out, "[cleanup] Timed out waiting for deletion.")
			return
		case <-ticker.C:
			cert := &crev1alpha1.Certification{}
			if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cert); err != nil {
				if apierrors.IsNotFound(err) {
					return // deleted
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// certification report — regenerate report from a completed certification
// ---------------------------------------------------------------------------

func newReportCommand() *cobra.Command {
	var resultsFile string

	configFlags := kubeconfig.NewConfigFlags(true)
	*configFlags.Namespace = defaultKubeNamespace

	cmd := &cobra.Command{
		Use:   "report <certification-name> [<certification-name>...]",
		Short: "Generate a report for one or more certifications",
		Long: `Connects to the cluster, fetches the named Certification(s), and generates
the same report that 'ncrectl certification run --wait' prints on completion.

Multiple certification names can be provided to combine them into a single report.
Each certification is shown in its own section with categories and summary.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReport(args, configFlags, resultsFile)
		},
	}

	cmd.Flags().StringVar(&resultsFile, "results-file", "",
		"Write certification report as JSON to this file path")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

func runReport(names []string, configFlags *kubeconfig.ConfigFlags, resultsFile string) error {
	ctx := context.Background()
	namespace := *configFlags.Namespace

	c, err := render.NewK8sClient(configFlags)
	if err != nil {
		return fmt.Errorf("build kubernetes client: %w", err)
	}

	var reports []*report.CertReport
	var errs []string
	for _, name := range names {
		cert := &crev1alpha1.Certification{}
		if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, cert); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("certification %q not found in namespace %q", name, namespace)
			}
			return fmt.Errorf("get certification %q: %w", name, err)
		}
		reports = append(reports, report.Build(ctx, c, cert))

		succeeded := apimeta.IsStatusConditionTrue(cert.Status.Conditions, crev1alpha1.CertificationSucceeded)
		failed := apimeta.IsStatusConditionTrue(cert.Status.Conditions, crev1alpha1.CertificationFailed)
		if !succeeded && !failed {
			errs = append(errs, fmt.Sprintf("certification %q is still running", name))
		} else if failed {
			errs = append(errs, fmt.Sprintf("certification %q failed", name))
		}
	}

	report.PrintMulti(os.Stdout, reports)

	if resultsFile != "" {
		if err := report.WriteJSON(resultsFile, reports); err != nil {
			return fmt.Errorf("write results file: %w", err)
		}
		_, _ = fmt.Fprintf(os.Stderr, "Results written to %s\n", resultsFile)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
