// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	kubeflowTrainerVersion = "v2.2.0"

	defaultImageRegistry   = "ghcr.io"
	defaultImageRepository = "nvidia/cluster-readiness-engine/manager"

	creNamespace = "cluster-readiness-engine"

	// Phase names (kubeadm-style).
	phaseCR   = "cr"
	phaseDeps = "deps"

	creAPIGroup = "cre.nvidia.com"

	trainerAPIGroup = "trainer.kubeflow.org"
	jobsetAPIGroup  = "jobset.x-k8s.io"

	crGracefulTimeout = 10 * time.Minute
)

// creResource describes an CRE CRD type for cleanup.
type creResource struct {
	resource   string // plural name (e.g. "certifications")
	kind       string // singular Kind (e.g. "Certification")
	apiVersion string // full apiVersion (e.g. "cre.nvidia.com/v1alpha1")
}

// NewCommand returns the "setup" cobra command. version is the running binary
// version and is threaded through to init/upgrade operations.
func NewCommand(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Cluster setup commands",
	}
	cmd.AddCommand(newInitCommand(version))
	cmd.AddCommand(newResetCommand())
	cmd.AddCommand(newSetupStatusCommand())
	return cmd
}

// ---------------------------------------------------------------------------
// ncrectl setup init
// ---------------------------------------------------------------------------

func newInitCommand(version string) *cobra.Command {
	var image, skipPhases, imagePullSecret string
	var autoApprove bool
	var versionOverride string

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize CRE on the target cluster",
		Long: `Installs CRE via Helm and its dependencies.

Phases:
  [deps]  Kubeflow Trainer ` + kubeflowTrainerVersion + `
  [helm]  CRE Helm chart (oci://ghcr.io/nvidia/cluster-readiness-engine)

The Helm chart is pulled from GHCR at the CLI version. Dev builds require --version.
Pass --image-pull-secret to authenticate against a private GHCR registry.

Use --skip-phases=deps to skip Kubeflow Trainer installation.
Use --auto-approve to skip the confirmation prompt (for CI/automation).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunInit(version, image, imagePullSecret, skipPhases, autoApprove,
				configFlags, versionOverride, os.Stdin, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&skipPhases, "skip-phases", "",
		"Comma-separated phases to skip (e.g., deps)")
	cmd.Flags().StringVar(&image, "image", "",
		"Override controller image (default: "+
			defaultImageRegistry+"/"+defaultImageRepository+":<version>)")
	cmd.Flags().StringVar(&imagePullSecret, "image-pull-secret", "",
		"GitHub token — creates ghcr.io pull secret and authenticates Helm chart pull")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false,
		"Skip interactive confirmation prompt (for CI/automation)")
	cmd.Flags().StringVar(&versionOverride, "version", "",
		"Helm chart version to install (required for dev builds)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// runInit executes the init phases sequentially.
func RunInit(
	version string,
	image, imagePullSecret, skipPhases string, autoApprove bool,
	configFlags *kubeconfig.ConfigFlags, versionOverride string,
	in io.Reader, out io.Writer,
) error {
	skip := parseSkipPhases(skipPhases)
	kubeconfigPath, kubeContext := *configFlags.KubeConfig, *configFlags.Context

	// [preflight]
	_, _ = fmt.Fprintln(out, "[preflight] Checking prerequisites...")

	ctxName, serverURL, err := getClusterInfo(configFlags)
	if err != nil {
		return fmt.Errorf("[preflight] %w", err)
	}

	if image == "" {
		if versionOverride != "" {
			image = defaultImageRegistry + "/" + defaultImageRepository + ":" + versionOverride
		} else {
			image = defaultImage(version)
		}
	}

	c, err := newSetupClient(configFlags)
	if err != nil {
		return fmt.Errorf("[preflight] build kubernetes client: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Show summary and prompt.
	if !autoApprove {
		_, _ = fmt.Fprintf(out, "\n  Target cluster\n")
		_, _ = fmt.Fprintf(out, "  Context:  %s\n", ctxName)
		_, _ = fmt.Fprintf(out, "  Server:   %s\n", serverURL)
		_, _ = fmt.Fprintf(out, "\n  Phases:\n")
		printPhaseList(out, skip, []phaseInfo{
			{phaseDeps, fmt.Sprintf("Kubeflow Trainer %s", kubeflowTrainerVersion)},
			{"helm", fmt.Sprintf("CRE Helm chart (%s)", image)},
		})
		_, _ = fmt.Fprintf(out,
			"\nDo you want to proceed? Only 'yes' will be accepted to confirm.\n")
		if !promptForConfirmation(in, out) {
			_, _ = fmt.Fprintln(out, "\nInit cancelled.")
			return nil
		}
		_, _ = fmt.Fprintln(out)
	}

	sp := setupPhaseParams{
		ctx:         ctx,
		c:           c,
		kubeconfig:  kubeconfigPath,
		kubeContext: kubeContext,
		skip:        skip,
		out:         out,
	}

	if err := installDepsPhase(sp); err != nil {
		return err
	}
	pullSecret, err := setupControllerSecret(sp, imagePullSecret)
	if err != nil {
		return fmt.Errorf("[helm] %w", err)
	}
	if err := installHelmRelease(helmInstallParams{
		version:         version,
		kubeconfig:      kubeconfigPath,
		kubeContext:     kubeContext,
		versionOverride: versionOverride,
		registryToken:   imagePullSecret,
		image:           image,
		pullSecretName:  pullSecret,
		out:             out,
	}); err != nil {
		return fmt.Errorf("[helm] %w", err)
	}

	displayVersion := version
	if versionOverride != "" {
		displayVersion = versionOverride
	}
	_, _ = fmt.Fprintf(out, "\nCRE %s initialized successfully.\n", displayVersion)
	return nil
}

// setupPhaseParams bundles common parameters for setup phase functions.
type setupPhaseParams struct {
	ctx         context.Context
	c           client.Client
	kubeconfig  string
	kubeContext string
	skip        map[string]bool
	out         io.Writer
}

func installDepsPhase(sp setupPhaseParams) error {
	if sp.skip[phaseDeps] {
		_, _ = fmt.Fprintln(sp.out, "[deps] Skipped.")
		return nil
	}
	_, _ = fmt.Fprintf(sp.out, "[deps] Installing Kubeflow Trainer %s...\n", kubeflowTrainerVersion)
	if err := installTrainerHelmRelease(sp.kubeconfig, sp.kubeContext, sp.out); err != nil {
		return fmt.Errorf("[deps] %w", err)
	}
	return nil
}

func setupControllerSecret(
	sp setupPhaseParams, imagePullSecret string,
) (string, error) {
	if imagePullSecret == "" {
		return "", nil
	}
	if _, err := EnsureNamespace(sp.ctx, sp.c, creNamespace, sp.out); err != nil {
		return "", fmt.Errorf("[helm] %w", err)
	}
	name, err := CreateImagePullSecret(sp.ctx, sp.c,
		creNamespace, imagePullSecret)
	if err != nil {
		return "", fmt.Errorf("[helm] create pull secret: %w", err)
	}
	_, _ = fmt.Fprintf(sp.out,
		"[helm] Created image pull secret %q in namespace %s.\n",
		name, creNamespace)
	return name, nil
}

// ---------------------------------------------------------------------------
// ncrectl setup reset
// ---------------------------------------------------------------------------

func newResetCommand() *cobra.Command {
	var skipPhases string
	var autoApprove bool

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Remove CRE from the target cluster",
		Long: `Removes CRE and its dependencies via Helm.

Phases:
  [cr]    CRE custom resource instances (Certifications, Workflows, etc.)
  [helm]  CRE Helm release (CRDs, controller, LogProfiles)
  [deps]  Kubeflow Trainer ` + kubeflowTrainerVersion + `

Use --skip-phases=deps to keep Kubeflow Trainer.
Use --auto-approve to skip the confirmation prompt (for CI/automation).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunReset(skipPhases, autoApprove, configFlags, os.Stdin, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&skipPhases, "skip-phases", "",
		"Comma-separated phases to skip (e.g., deps)")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false,
		"Skip interactive confirmation prompt (for CI/automation)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// runReset executes the reset phases in reverse order.
func RunReset(
	skipPhases string, autoApprove bool,
	configFlags *kubeconfig.ConfigFlags,
	in io.Reader, out io.Writer,
) error {
	skip := parseSkipPhases(skipPhases)
	kubeconfigPath, kubeContext := *configFlags.KubeConfig, *configFlags.Context

	// [preflight]
	_, _ = fmt.Fprintln(out, "[preflight] Checking prerequisites...")

	ctxName, serverURL, err := getClusterInfo(configFlags)
	if err != nil {
		return fmt.Errorf("[preflight] %w", err)
	}

	c, err := newSetupClient(configFlags)
	if err != nil {
		return fmt.Errorf("[preflight] build kubernetes client: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Show summary and prompt.
	if !autoApprove {
		_, _ = fmt.Fprintf(out, "\n  Target cluster\n")
		_, _ = fmt.Fprintf(out, "  Context:  %s\n", ctxName)
		_, _ = fmt.Fprintf(out, "  Server:   %s\n", serverURL)
		_, _ = fmt.Fprintf(out, "\n  Phases:\n")
		printPhaseList(out, skip, []phaseInfo{
			{phaseCR, "CRE custom resources"},
			{"helm", "CRE Helm release (CRDs, controller, LogProfiles)"},
			{phaseDeps, fmt.Sprintf("Kubeflow Trainer %s", kubeflowTrainerVersion)},
		})
		_, _ = fmt.Fprintf(out,
			"\nDo you want to proceed? Only 'yes' will be accepted to confirm.\n")
		if !promptForConfirmation(in, out) {
			_, _ = fmt.Fprintln(out, "\nReset cancelled.")
			return nil
		}
		_, _ = fmt.Fprintln(out)
	}

	// [cr] — Delete all CRE custom resource instances while the
	// controller is still alive to process finalizer removal.
	if skip[phaseCR] {
		_, _ = fmt.Fprintln(out, "[cr] Skipped.")
	} else {
		if err := deleteCRECRs(ctx, c, out); err != nil {
			return fmt.Errorf("[cr] %w", err)
		}
	}

	if err := uninstallHelmRelease(helmUninstallParams{
		kubeconfig:  kubeconfigPath,
		kubeContext: kubeContext,
		out:         out,
	}); err != nil {
		return fmt.Errorf("[helm] %w", err)
	}

	// Helm intentionally never deletes CRDs that live in a chart's crds/
	// directory (to avoid accidental data loss on uninstall), so `helm
	// uninstall` leaves the CRE CRDs behind. Delete them explicitly to
	// leave the cluster clean and let a subsequent init start fresh.
	if err := deleteCRDsByGroup(ctx, c, creAPIGroup, "[helm]", "CRE", out); err != nil {
		return fmt.Errorf("[helm] %w", err)
	}

	// Cluster-scoped RBAC resources (ClusterRole, ClusterRoleBinding) are
	// not subject to Helm's CRD-preservation rule, but a partially failed
	// `helm upgrade --install` may have applied them before rolling back,
	// leaving no completed release for `helm uninstall` to track. Delete
	// them explicitly so a subsequent init can start from a clean state.
	if err := deleteClusterScopedRBAC(ctx, c, "[helm]", out); err != nil {
		return fmt.Errorf("[helm] %w", err)
	}

	sp := setupPhaseParams{
		ctx:         ctx,
		c:           c,
		kubeconfig:  kubeconfigPath,
		kubeContext: kubeContext,
		skip:        skip,
		out:         out,
	}
	if err := uninstallDepsPhase(sp); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "\nCRE reset successfully.")
	return nil
}

func uninstallDepsPhase(sp setupPhaseParams) error {
	if sp.skip[phaseDeps] {
		_, _ = fmt.Fprintln(sp.out, "[deps] Skipped.")
		return nil
	}
	_, _ = fmt.Fprintf(sp.out, "[deps] Removing Kubeflow Trainer %s...\n", kubeflowTrainerVersion)
	if err := uninstallTrainerHelmRelease(sp.kubeconfig, sp.kubeContext, sp.out); err != nil {
		return fmt.Errorf("[deps] %w", err)
	}

	// Same Helm CRD-preservation behavior as the [helm] phase: uninstalling
	// the kubeflow-trainer release (and its JobSet sub-chart dependency)
	// leaves their CRDs behind. Clean them up explicitly.
	if err := deleteCRDsByGroup(sp.ctx, sp.c, trainerAPIGroup, "[deps]", "Kubeflow Trainer", sp.out); err != nil {
		return fmt.Errorf("[deps] %w", err)
	}
	if err := deleteCRDsByGroup(sp.ctx, sp.c, jobsetAPIGroup, "[deps]", "JobSet", sp.out); err != nil {
		return fmt.Errorf("[deps] %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase helpers
// ---------------------------------------------------------------------------

// phaseInfo describes a phase for display in the confirmation summary.
type phaseInfo struct {
	name        string
	description string
}

// printPhaseList prints the phase summary with skip indicators.
func printPhaseList(out io.Writer, skip map[string]bool, phases []phaseInfo) {
	for _, p := range phases {
		if skip[p.name] {
			_, _ = fmt.Fprintf(out, "    - [%-12s] %s (skipped)\n", p.name, p.description)
		} else {
			_, _ = fmt.Fprintf(out, "    - [%-12s] %s\n", p.name, p.description)
		}
	}
}

// parseSkipPhases converts a comma-separated string into a set of phase names.
func parseSkipPhases(s string) map[string]bool {
	skip := make(map[string]bool)
	if s == "" {
		return skip
	}
	for phase := range strings.SplitSeq(s, ",") {
		phase = strings.TrimSpace(phase)
		if phase != "" {
			skip[phase] = true
		}
	}
	return skip
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// getClusterInfo resolves the current context name and server URL from kubeconfig.
func getClusterInfo(cf *kubeconfig.ConfigFlags) (string, string, error) {
	rawConfig, err := cf.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return "", "", fmt.Errorf("load kubeconfig: %w", err)
	}

	kubeContext := *cf.Context
	ctxName := rawConfig.CurrentContext
	if kubeContext != "" {
		ctxName = kubeContext
	}

	ctx, ok := rawConfig.Contexts[ctxName]
	if !ok {
		return "", "", fmt.Errorf("context %q not found in kubeconfig", ctxName)
	}

	cluster, ok := rawConfig.Clusters[ctx.Cluster]
	if !ok {
		return "", "", fmt.Errorf(
			"cluster %q (referenced by context %q) not found in kubeconfig",
			ctx.Cluster, ctxName)
	}

	return ctxName, cluster.Server, nil
}

// promptForConfirmation reads from in and returns true only if the user types "yes".
func promptForConfirmation(in io.Reader, out io.Writer) bool {
	_, _ = fmt.Fprint(out, "  Enter a value: ")
	scanner := bufio.NewScanner(in)
	if scanner.Scan() {
		return strings.TrimSpace(scanner.Text()) == "yes"
	}
	return false
}

// waitForNamespaceDeletion polls until a namespace is fully removed.
func WaitForNamespaceDeletion(ctx context.Context, c client.Client, name string, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	_, _ = fmt.Fprintf(out, "  Waiting for namespace %s to terminate...\n", name)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintf(out, "  Warning: timed out waiting for namespace %s deletion.\n", name)
			return
		case <-ticker.C:
			ns := &unstructured.Unstructured{}
			ns.SetAPIVersion("v1")
			ns.SetKind("Namespace")
			if err := c.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
				if apierrors.IsNotFound(err) {
					_, _ = fmt.Fprintf(out, "  Namespace %s deleted.\n", name)
					return
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Go K8s client helpers
// ---------------------------------------------------------------------------

// newSetupClient builds a controller-runtime client for setup operations.
// Uses a dynamic REST mapper that auto-discovers new resource types (e.g.,
// after CRDs are installed).
func newSetupClient(cf *kubeconfig.ConfigFlags) (client.Client, error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}

	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)

	return client.New(restConfig, client.Options{Scheme: s})
}

// ---------------------------------------------------------------------------
// [cr] phase — CRE custom resource cleanup
// ---------------------------------------------------------------------------

// deleteCRECRs implements the [cr] reset phase. It performs graceful,
// controller-driven cleanup only and fails if resources remain.
func deleteCRECRs(ctx context.Context, c client.Client, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "[cr] Deleting CRE custom resources...")

	resources, err := discoverCREResources(ctx, c)
	if err != nil {
		return fmt.Errorf("discover CRE resources: %w", err)
	}
	if len(resources) == 0 {
		_, _ = fmt.Fprintln(out, "[cr] No CRE custom resources found.")
		return nil
	}

	// Check if any CRE CRs exist at all.
	if !anyCRECRsExist(ctx, c, resources) {
		_, _ = fmt.Fprintln(out, "[cr] No CRE custom resources found.")
		return nil
	}

	// Stage 1: Graceful deletion — delete all CRE resources and let
	// controllers reconcile finalizers/ownership cleanup.
	gracefulCtx, gracefulCancel := context.WithTimeout(ctx, crGracefulTimeout)
	defer gracefulCancel()

	gracefulCascadeDelete(gracefulCtx, c, out, resources)

	// Wait for remaining resources to terminate naturally.
	if err := waitForAllCRECRsGone(gracefulCtx, c, resources); err != nil {
		return fmt.Errorf("graceful cleanup did not complete within %s: %w",
			crGracefulTimeout, err)
	}

	_, _ = fmt.Fprintln(out, "  All CRE custom resources removed.")
	return nil
}

// gracefulCascadeDelete deletes all known CRE resources.
func gracefulCascadeDelete(
	ctx context.Context, c client.Client, out io.Writer, resources []creResource,
) {
	for _, res := range resources {
		items, err := listCRECRs(ctx, c, res)
		if err != nil {
			continue // CRD may not exist
		}
		for _, item := range items {
			name := item.GetName()
			ns := item.GetNamespace()
			label := name
			if ns != "" {
				label = fmt.Sprintf("%s (namespace: %s)", name, ns)
			}

			if item.GetDeletionTimestamp() != nil {
				_, _ = fmt.Fprintf(out, "  %s/%s already terminating\n", res.kind, label)
				continue
			}

			if err := c.Delete(ctx, &item); client.IgnoreNotFound(err) != nil {
				_, _ = fmt.Fprintf(out, "  Warning: failed to delete %s/%s: %v\n",
					res.kind, label, err)
				continue
			}
			_, _ = fmt.Fprintf(out, "  %s/%s deleted\n", res.kind, label)
		}
	}
}

// waitForAllCRECRsGone polls until no CRE CR instances remain.
func waitForAllCRECRsGone(
	ctx context.Context, c client.Client, resources []creResource,
) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if !anyCRECRsExist(ctx, c, resources) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// listCRECRs lists all instances of an CRE resource type across
// all namespaces using an unstructured client.
func listCRECRs(
	ctx context.Context, c client.Client, res creResource,
) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(res.apiVersion)
	list.SetKind(res.kind + "List")
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// anyCRECRsExist returns true if any CRE CR instances exist
// across all resource types.
func anyCRECRsExist(
	ctx context.Context, c client.Client, resources []creResource,
) bool {
	for _, res := range resources {
		items, err := listCRECRs(ctx, c, res)
		if err != nil {
			continue
		}
		if len(items) > 0 {
			return true
		}
	}
	return false
}

// discoverCREResources returns all CRD-backed CRE resources for the
// configured API group, including their served apiVersion and Kind metadata.
func discoverCREResources(ctx context.Context, c client.Client) ([]creResource, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}

	resources := make([]creResource, 0, len(list.Items))
	for _, item := range list.Items {
		group, found, err := unstructured.NestedString(item.Object, "spec", "group")
		if err != nil || !found || group != creAPIGroup {
			continue
		}
		kind, found, err := unstructured.NestedString(item.Object, "spec", "names", "kind")
		if err != nil || !found || kind == "" {
			continue
		}
		if kind == "LogProfile" {
			continue // LogProfiles are handled by the [logprofiles] phase, not [cr]
		}
		resource, found, err := unstructured.NestedString(item.Object, "spec", "names", "plural")
		if err != nil || !found || resource == "" {
			continue
		}

		apiVersion := ""
		versions, found, err := unstructured.NestedSlice(item.Object, "spec", "versions")
		if err == nil && found {
			for _, v := range versions {
				vm, ok := v.(map[string]any)
				if !ok {
					continue
				}
				name, _ := vm["name"].(string)
				served, _ := vm["served"].(bool)
				if name != "" && served {
					apiVersion = group + "/" + name
					break
				}
			}
		}
		if apiVersion == "" {
			continue
		}

		resources = append(resources, creResource{
			resource:   resource,
			kind:       kind,
			apiVersion: apiVersion,
		})
	}

	sort.Slice(resources, func(i, j int) bool {
		return resources[i].resource < resources[j].resource
	})
	return resources, nil
}

// deleteClusterScopedRBAC deletes all ClusterRoles and ClusterRoleBindings
// that carry the CRE app label. A partially-failed `helm upgrade --install`
// can apply cluster-scoped RBAC before rolling back, leaving no completed
// release for `helm uninstall` to track. Explicit deletion lets a subsequent
// init succeed without a "already exists" conflict.
func deleteClusterScopedRBAC(ctx context.Context, c client.Client, phaseTag string, out io.Writer) error {
	selector := client.MatchingLabels{"app.kubernetes.io/name": "cluster-readiness-engine"}

	crList := &rbacv1.ClusterRoleList{}
	if err := c.List(ctx, crList, selector); err != nil {
		return fmt.Errorf("list ClusterRoles: %w", err)
	}
	crbList := &rbacv1.ClusterRoleBindingList{}
	if err := c.List(ctx, crbList, selector); err != nil {
		return fmt.Errorf("list ClusterRoleBindings: %w", err)
	}

	if len(crList.Items)+len(crbList.Items) == 0 {
		return nil
	}

	_, _ = fmt.Fprintf(out, "%s Removing cluster-scoped RBAC resources...\n", phaseTag)
	for i := range crbList.Items {
		if err := c.Delete(ctx, &crbList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ClusterRoleBinding %s: %w", crbList.Items[i].Name, err)
		}
	}
	for i := range crList.Items {
		if err := c.Delete(ctx, &crList.Items[i]); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete ClusterRole %s: %w", crList.Items[i].Name, err)
		}
	}
	return nil
}

// deleteCRDsByGroup deletes all CustomResourceDefinitions belonging to the
// given API group and waits for them to be fully removed. Helm intentionally
// never deletes CRDs on `helm uninstall` (to avoid accidental data loss), so
// every phase that installs CRDs via Helm needs this explicit cleanup.
// phaseTag (e.g. "[helm]", "[deps]") and label (e.g. "CRE") are used
// purely for log message prefixes/wording.
func deleteCRDsByGroup(ctx context.Context, c client.Client, group, phaseTag, label string, out io.Writer) error {
	names, err := listCRDNamesByGroup(ctx, c, group)
	if err != nil {
		return fmt.Errorf("list CRDs for group %s: %w", group, err)
	}
	if len(names) == 0 {
		return nil
	}

	_, _ = fmt.Fprintf(out, "%s Removing %s CRDs...\n", phaseTag, label)
	for _, name := range names {
		crd := &unstructured.Unstructured{}
		crd.SetAPIVersion("apiextensions.k8s.io/v1")
		crd.SetKind("CustomResourceDefinition")
		crd.SetName(name)
		if err := c.Delete(ctx, crd); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete CRD %s: %w", name, err)
		}
	}

	waitForCRDsDeletion(ctx, c, group, label, out)
	return nil
}

func waitForCRDsDeletion(ctx context.Context, c client.Client, group, label string, out io.Writer) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	_, _ = fmt.Fprintln(out, "  Waiting for CRDs to be fully removed...")

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		names, err := listCRDNamesByGroup(ctx, c, group)
		if err == nil && len(names) == 0 {
			_, _ = fmt.Fprintf(out, "  All %s CRDs removed.\n", label)
			return
		}
		if err != nil {
			_, _ = fmt.Fprintf(out,
				"  Warning: failed to list CRDs for group %s: %v\n", group, err)
		}
		select {
		case <-ctx.Done():
			_, _ = fmt.Fprintln(out, "  Warning: timed out waiting for CRD deletion.")
			return
		case <-ticker.C:
		}
	}
}

func listCRDNamesByGroup(ctx context.Context, c client.Client, group string) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		specGroup, found, err := unstructured.NestedString(item.Object, "spec", "group")
		if err != nil || !found || specGroup != group {
			continue
		}
		names = append(names, item.GetName())
	}
	return names, nil
}

// EnsureNamespace creates a namespace if it doesn't exist.
// Returns true if the namespace was created by this call (false if it already existed).
func EnsureNamespace(ctx context.Context, c client.Client, name string, out io.Writer) (bool, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("check namespace %s: %w", name, err)
		}
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "ncrectl",
				},
			},
		}
		_, _ = fmt.Fprintf(out, "[namespace] Creating namespace %s...\n", name)
		if err := c.Create(ctx, ns); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, fmt.Errorf("create namespace %s: %w", name, err)
		}
		return true, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// YAML helpers (split, decode, patch)
// ---------------------------------------------------------------------------

// defaultImage returns the controller image derived from the CLI version.
func defaultImage(version string) string {
	return defaultImageRegistry + "/" + defaultImageRepository + ":" + version
}

// ---------------------------------------------------------------------------
// Image pull secret helpers
// ---------------------------------------------------------------------------

const pullSecretName = "ncrectl-pull-secret"

// CreateImagePullSecret creates a dockerconfigjson Secret for ghcr.io.
// token is a GitHub Personal Access Token with read:packages scope.
func CreateImagePullSecret(ctx context.Context, c client.Client, namespace, token string) (string, error) {
	authStr := base64.StdEncoding.EncodeToString([]byte("token:" + token))
	dockerConfig := fmt.Sprintf(`{"auths":{"ghcr.io":{"username":"token","password":"%s","auth":"%s"}}}`,
		token, authStr)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pullSecretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "ncrectl",
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerConfig),
		},
	}

	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Update existing secret.
			existing := &corev1.Secret{}
			if getErr := c.Get(ctx, client.ObjectKey{Name: pullSecretName, Namespace: namespace}, existing); getErr != nil {
				return "", fmt.Errorf("get existing secret: %w", getErr)
			}
			existing.Data = secret.Data
			if updateErr := c.Update(ctx, existing); updateErr != nil {
				return "", fmt.Errorf("update secret: %w", updateErr)
			}
			return pullSecretName, nil
		}
		return "", fmt.Errorf("create image pull secret: %w", err)
	}

	return pullSecretName, nil
}

// parseImage splits a container image reference into name and tag components.
// Handles registry ports (localhost:5000/repo:tag), digests (@sha256:...),
// and missing tags (defaults to "latest").
func parseImage(image string) (name, tag string) {
	// Handle digest references: registry/repo@sha256:abc123
	if i := strings.LastIndex(image, "@"); i != -1 {
		return image[:i], image[i+1:]
	}
	// Handle tag references: registry/repo:tag
	// Must distinguish registry port (localhost:5000) from tag separator.
	lastColon := strings.LastIndex(image, ":")
	lastSlash := strings.LastIndex(image, "/")
	if lastColon != -1 && lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:]
	}
	return image, "latest"
}
