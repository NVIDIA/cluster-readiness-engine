// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/discovery"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Keep this in step with github.com/kubeflow/trainer/v2 in go.mod. The
	// controller compiles against those API types, so installing an older
	// chart means the CRDs it creates can lag the types NVCRE writes.
	kubeflowTrainerVersion = "v2.2.1"

	defaultImageRegistry   = "ghcr.io"
	defaultImageRepository = "nvidia/cluster-readiness-engine/manager"

	nvcreNamespace = "nvcre"

	// Phase names (kubeadm-style).
	phaseCR   = "cr"
	phaseDeps = "deps"
	phaseHelm = "helm"

	nvcreAPIGroup = "nvcre.nvidia.com"

	trainerAPIGroup = "trainer.kubeflow.org"
	jobsetAPIGroup  = "jobset.x-k8s.io"

	kindCustomResourceDefinition = "CustomResourceDefinition"
	kindNamespace                = "Namespace"
	kindClusterRole              = "ClusterRole"
	kindDeployment               = "Deployment"
	kindService                  = "Service"
	kindServiceAccount           = "ServiceAccount"
	kindConfigMap                = "ConfigMap"
	kindTrainingRuntime          = "TrainingRuntime"
	kindJobSet                   = "JobSet"
	kindValidatingWebhook        = "ValidatingWebhookConfiguration"
	kindMutatingWebhook          = "MutatingWebhookConfiguration"
	jobSetControllerName         = "jobset-controller"
	jobSetWebhookServiceName     = "jobset-webhook-service"
	appsAPIGroup                 = "apps"
	rbacAPIGroup                 = "rbac.authorization.k8s.io"
	rbacV1APIVersion             = "rbac.authorization.k8s.io/v1"

	crGracefulTimeout = 10 * time.Minute
)

// nvcreResource describes an NVCRE CRD type for cleanup.
type nvcreResource struct {
	resource   string // plural name (e.g. "certifications")
	kind       string // singular Kind (e.g. "Certification")
	apiVersion string // full apiVersion (e.g. "nvcre.nvidia.com/v1alpha1")
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
// nvcrectl setup init
// ---------------------------------------------------------------------------

func newInitCommand(version string) *cobra.Command {
	var image, skipPhases, imagePullSecret string
	var chartRef, trainerChartRef string
	var autoApprove bool
	var versionOverride string

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize NVCRE on the target cluster",
		Long: `Installs NVCRE via Helm and its dependencies.

Phases:
  [deps]  Kubeflow Trainer ` + kubeflowTrainerVersion + `
  [helm]  NVCRE Helm chart (` + helmChartOCI + `)

By default the Helm chart is pulled from GHCR at the CLI version. Dev builds require --version.
Pass --image-pull-secret to authenticate against a private GHCR registry.
Pass --chart-ref and --trainer-chart-ref to pull the charts from a mirror
registry instead of GHCR (for restricted-egress clusters).

Use --skip-phases=deps to skip Kubeflow Trainer installation.
Use --auto-approve to skip the confirmation prompt (for CI/automation).

Re-running init is safe: a Kubeflow Trainer release already deployed at the
pinned version is skipped. A release wedged by webhook Secret field-ownership
conflicts (issue #180) is recovered automatically only when no Trainer
workloads exist, JobSet ownership is unambiguous, and the protected-resource
inventory in kubeflow-system passes recovery safety checks.
Otherwise the blocking evidence and the operator-managed procedure are
printed. These conditions are rechecked at each destructive boundary: a
refusal before the Helm uninstall leaves the cluster untouched, while a
refusal at a later boundary reports what it already completed and stops.
Earlier cleanup is not rolled back.

The NVCRE CRDs are server-side-applied from the chart before every Helm
upgrade, so re-running init after a version bump also updates the CRD
schemas (Helm alone only installs CRDs on the first install).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return RunInit(version, image, imagePullSecret, skipPhases, autoApprove,
				configFlags, versionOverride, chartRef, trainerChartRef,
				os.Stdin, os.Stderr)
		},
	}

	cmd.Flags().StringVar(&skipPhases, "skip-phases", "",
		"Comma-separated phases to skip (e.g., deps)")
	cmd.Flags().StringVar(&image, "image", "",
		"Override controller image (default: "+
			defaultImageRegistry+"/"+defaultImageRepository+":<version>)")
	cmd.Flags().StringVar(&imagePullSecret, "image-pull-secret", "",
		"GitHub token — creates ghcr.io pull secret and authenticates chart pulls from GHCR (mirrors need helm registry login)")
	cmd.Flags().StringVar(&chartRef, "chart-ref", helmChartOCI,
		"Override NVCRE Helm chart location (e.g., a mirror registry)")
	cmd.Flags().StringVar(&trainerChartRef, "trainer-chart-ref", trainerHelmChartOCI,
		"Override Kubeflow Trainer Helm chart location (e.g., a mirror registry)")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false,
		"Skip interactive confirmation prompt (for CI/automation)")
	cmd.Flags().StringVar(&versionOverride, "version", "",
		"Helm chart version to install (required for dev builds)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// RunInit executes the init phases sequentially. chartRef and trainerChartRef
// override the chart locations the NVCRE and Kubeflow Trainer releases are
// installed from; empty means the published GHCR charts.
func RunInit(
	version string,
	image, imagePullSecret, skipPhases string, autoApprove bool,
	configFlags *kubeconfig.ConfigFlags, versionOverride string,
	chartRef, trainerChartRef string,
	in io.Reader, out io.Writer,
) error {
	skip, err := parseSkipPhases(skipPhases, initSkipPhases...)
	if err != nil {
		return err
	}
	kubeconfigPath, kubeContext := *configFlags.KubeConfig, *configFlags.Context
	if chartRef == "" {
		chartRef = helmChartOCI
	}
	if trainerChartRef == "" {
		trainerChartRef = trainerHelmChartOCI
	}

	// A half-mirrored chart-ref configuration still pulls one chart from
	// GHCR (issue #321); warn but continue, because a reachable GHCR makes
	// it a valid setup.
	if warning := asymmetricChartRefsWarning(chartRef, trainerChartRef, skip[phaseDeps], skip[phaseHelm]); warning != "" {
		_, _ = fmt.Fprintln(out, warning)
	}

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
	discoverNamespaced, err := newSetupDiscovery(configFlags)
	if err != nil {
		return fmt.Errorf("[preflight] build kubernetes discovery client: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Show summary and prompt.
	if !autoApprove {
		_, _ = fmt.Fprintf(out, "\n  Target cluster\n")
		_, _ = fmt.Fprintf(out, "  Context:  %s\n", ctxName)
		_, _ = fmt.Fprintf(out, "  Server:   %s\n", serverURL)
		_, _ = fmt.Fprintf(out, "\n  Phases:\n")
		printPhaseList(out, skip, initPhaseSummary(trainerChartRef, chartRef, image))
		_, _ = fmt.Fprintf(out,
			"\nDo you want to proceed? Only 'yes' will be accepted to confirm.\n")
		if !promptForConfirmation(in, out) {
			_, _ = fmt.Fprintln(out, "\nInit cancelled.")
			return nil
		}
		_, _ = fmt.Fprintln(out)
	}

	sp := setupPhaseParams{
		ctx:                ctx,
		c:                  c,
		kubeconfig:         kubeconfigPath,
		kubeContext:        kubeContext,
		skip:               skip,
		in:                 in,
		autoApprove:        autoApprove,
		trainer:            newTrainerHelm(kubeconfigPath, kubeContext, trainerChartRef, imagePullSecret),
		discoverNamespaced: discoverNamespaced,
		out:                out,
	}

	if err := installDepsPhase(sp); err != nil {
		return err
	}
	if skip[phaseHelm] {
		_, _ = fmt.Fprintln(out, "[helm] Skipped.")
		_, _ = fmt.Fprintln(out, "\nNVCRE dependency setup completed successfully.")
		return nil
	}

	pullSecret, err := setupControllerSecret(sp, imagePullSecret)
	if err != nil {
		return fmt.Errorf("[helm] %w", err)
	}
	if nvcreOutput, err := installHelmRelease(helmInstallParams{
		ctx:             ctx,
		c:               c,
		version:         version,
		kubeconfig:      kubeconfigPath,
		kubeContext:     kubeContext,
		versionOverride: versionOverride,
		registryToken:   imagePullSecret,
		image:           image,
		chartRef:        chartRef,
		pullSecretName:  pullSecret,
		out:             out,
	}); err != nil {
		// Same attempt-then-classify wrapper as the [deps] phase for
		// symmetric error reporting, but the NVCRE chart renders no
		// per-render certificate material, so there is no recovery arm
		// (ADR-073 decision 4).
		if classifyHelmInstallFailure(nvcreOutput, nvcreNamespace) == failureClassSSAConflict {
			_, _ = fmt.Fprintf(out,
				"[helm] The failure matches the Secret field-ownership conflict signature. "+
					"Automatic recovery covers only the %s release; inspect the %s release with "+
					"'helm status %s --namespace %s' and resolve the conflict manually.\n",
				trainerReleaseName, helmReleaseName, helmReleaseName, nvcreNamespace)
		}
		return fmt.Errorf("[helm] %w", err)
	}

	displayVersion := version
	if versionOverride != "" {
		displayVersion = versionOverride
	}
	_, _ = fmt.Fprintf(out, "\nNVCRE %s initialized successfully.\n", displayVersion)
	return nil
}

// setupPhaseParams bundles common parameters for setup phase functions.
type setupPhaseParams struct {
	ctx         context.Context
	c           client.Client
	kubeconfig  string
	kubeContext string
	skip        map[string]bool
	// in is the confirmation input stream; the [deps] recovery prompt reads
	// from it when autoApprove is false.
	in          io.Reader
	autoApprove bool
	// trainer bundles the Helm subprocess operations the [deps] phase needs.
	trainer trainerHelm
	// jobSetMode is selected before Trainer mutation and preserved across
	// destructive recovery so reinstall uses the same ownership decision.
	jobSetMode             jobSetMode
	jobSetOwnershipToken   string
	releaseManifestObjects map[string]struct{}
	// discoverNamespaced returns preferred namespaced API resources. A partial
	// discovery result is returned with its error and is rejected by recovery.
	discoverNamespaced func() ([]*metav1.APIResourceList, error)
	out                io.Writer
}

// trainerHelm bundles the Helm subprocess operations the [deps] phase uses,
// so tests can substitute stubs — the same substitution shape status_test.go
// uses for helmStateFunc. Production code uses newTrainerHelm.
type trainerHelm struct {
	// state returns the trainer release state and installed chart version.
	state trainerStateFunc
	// install runs helm upgrade --install and returns the captured transcript.
	install func(mode jobSetMode, out io.Writer) (string, error)
	// uninstall removes the trainer release.
	uninstall func(out io.Writer) error
	// manifest returns the stored manifest for the exact trainer release.
	manifest func() (string, error)
}

// newTrainerHelm returns the CLI-backed trainerHelm implementation. chartRef
// is the Kubeflow Trainer chart location; empty means the published GHCR
// chart. registryToken authenticates the chart pull when chartRef is
// GHCR-hosted; for a mirror-hosted chart it is ignored (issue #321).
func newTrainerHelm(kubeconfigPath, kubeContext, chartRef, registryToken string) trainerHelm {
	return trainerHelm{
		state: newTrainerStateQuery(kubeconfigPath, kubeContext),
		install: func(mode jobSetMode, out io.Writer) (string, error) {
			return installTrainerHelmRelease(
				kubeconfigPath, kubeContext, chartRef, registryToken, mode != jobSetModeExternal, out)
		},
		uninstall: func(out io.Writer) error {
			return uninstallTrainerHelmRelease(kubeconfigPath, kubeContext, out)
		},
		manifest: func() (string, error) {
			return trainerReleaseManifest(kubeconfigPath, kubeContext)
		},
	}
}

// trainerAction is the [deps] phase action planTrainerPhase selects.
type trainerAction int

const (
	// trainerActionInstall runs helm upgrade --install for a confirmed fresh
	// installation; a failure is reported raw with no recovery arm.
	trainerActionInstall trainerAction = iota
	// trainerActionSkip prints "already deployed" and does nothing: the
	// release is deployed at the pinned chart version, and never
	// re-rendering the chart means never re-rolling the webhook certs.
	trainerActionSkip
	// trainerActionAttemptRecover runs the install once and classifies a
	// failure per ADR-073 decision 2; the conflict class arms the gated
	// automatic recovery.
	trainerActionAttemptRecover
	// trainerActionRefuse stops because the release state could not be read.
	trainerActionRefuse
)

// planTrainerPhase maps the observed trainer release state to a [deps]
// action (ADR-073 decision 4). chartVersion is compared without the leading
// "v", the way Helm stores chart versions.
func planTrainerPhase(state, chartVersion string) trainerAction {
	switch state {
	case helmStateDeployed:
		if chartVersion == strings.TrimPrefix(kubeflowTrainerVersion, "v") {
			return trainerActionSkip
		}
		return trainerActionAttemptRecover
	case helmStateUnknown:
		return trainerActionRefuse
	case helmStateNotInstalled, helmStateUninstalled:
		return trainerActionInstall
	default: // failed, pending-install, pending-upgrade, pending-rollback, ...
		return trainerActionAttemptRecover
	}
}

// helmStateFailedOrPending reports whether a release state is failed or any
// pending-* state — the release-state half of the two-signal conflict
// classification (ADR-073 decision 2).
func helmStateFailedOrPending(state string) bool {
	return state == "failed" || strings.HasPrefix(state, "pending-")
}

// installDepsPhase installs Kubeflow Trainer when release and JobSet ownership
// evidence select a safe action. It skips a pinned healthy release, refuses
// unknown or incomplete evidence, and classifies a failed install attempt. A
// failure carrying the issue #180 field-ownership conflict signature arms the
// gated automatic recovery; anything else fails with the raw helm output.
func installDepsPhase(sp setupPhaseParams) error {
	if sp.skip[phaseDeps] {
		_, _ = fmt.Fprintln(sp.out, "[deps] Skipped.")
		return nil
	}

	stateResult := sp.trainer.state()
	state, chartVersion := stateResult.state, stateResult.chartVersion
	action := planTrainerPhase(state, chartVersion)
	if action == trainerActionRefuse {
		if stateResult.err == nil {
			stateResult.err = fmt.Errorf("helm returned an unknown release state without a diagnostic")
		}
		printUnknownTrainerStateFallback(sp.out, stateResult.err)
		return fmt.Errorf("[deps] cannot determine Kubeflow Trainer release state: %w", stateResult.err)
	}

	crdPresent, err := jobSetCRDExists(sp.ctx, sp.c)
	if err != nil {
		printJobSetOwnershipFallback(sp.out, unknownJobSet("read JobSet CRD: "+err.Error()))
		return fmt.Errorf("[deps] read JobSet CRD: %w", err)
	}
	if action == trainerActionSkip {
		if !crdPresent {
			return missingJobSetCRDError(sp.out, state)
		}
		_, _ = fmt.Fprintf(sp.out,
			"[deps] Kubeflow Trainer release %q already deployed at chart version %s; skipping.\n",
			trainerReleaseName, chartVersion)
		return nil
	}
	if !crdPresent && state != helmStateNotInstalled && state != helmStateUninstalled {
		return missingJobSetCRDError(sp.out, state)
	}

	observation := observeJobSetOwnership(sp.ctx, sp.c, crdPresent, state, sp.trainer.manifest)
	if observation.mode == jobSetModeUnknown {
		printJobSetOwnershipFallback(sp.out, observation)
		return fmt.Errorf("[deps] JobSet ownership is unknown: %s", strings.Join(observation.evidence, "; "))
	}
	sp.jobSetMode = observation.mode
	sp.jobSetOwnershipToken = observation.ownershipToken
	sp.releaseManifestObjects = observation.manifestObjects
	if observation.mode == jobSetModeAbsent || observation.mode == jobSetModeCRDOnly {
		_, _ = fmt.Fprintln(sp.out,
			"[deps] JobSet detection covers the verified chart fingerprint only; operators with customized or uncertain prior installations must use operator-managed installation.")
	}
	if observation.mode == jobSetModeCRDOnly {
		_, _ = fmt.Fprintln(sp.out,
			"[deps] Installing the bundled JobSet controller using the retained JobSet CRD; its schema is not refreshed.")
	}

	switch action {
	case trainerActionSkip, trainerActionRefuse:
		// Both were resolved above; reaching here is a programming error and
		// must not mutate the cluster.
		return fmt.Errorf("[deps] internal error: action %d reached the install path", action)
	case trainerActionInstall:
		_, _ = fmt.Fprintf(sp.out, "[deps] Installing Kubeflow Trainer %s...\n", kubeflowTrainerVersion)
		if output, err := sp.trainer.install(observation.mode, sp.out); err != nil {
			printTrainerFailureGuidance(sp, output)
			return fmt.Errorf("[deps] %w", err)
		}
		return nil
	}

	// trainerActionAttemptRecover: the release exists but is not cleanly
	// deployed at the pinned version. Attempt once, then classify.
	_, _ = fmt.Fprintf(sp.out,
		"[deps] Kubeflow Trainer release is in state %q; attempting install of %s...\n",
		state, kubeflowTrainerVersion)
	output, err := sp.trainer.install(observation.mode, sp.out)
	if err == nil {
		return nil
	}
	return handleTrainerInstallFailure(sp, output, err)
}

// handleTrainerInstallFailure classifies a failed trainer install attempt
// and either performs the gated automatic recovery or fails fast with the
// manual procedure (ADR-073 decisions 2, 3, and 5).
func handleTrainerInstallFailure(sp setupPhaseParams, output string, installErr error) error {
	installErr = fmt.Errorf("[deps] %w", installErr)

	failure := classifyTrainerInstallFailure(output)
	if failure == failureClassJobSetOwnership {
		printTrainerFailureGuidance(sp, output)
		return installErr
	}
	if failure != failureClassSSAConflict {
		// Not this failure class (e.g. a registry timeout): fail with the
		// raw helm output, which the install attempt already printed.
		return installErr
	}

	// Second signal: the release state must agree before anything
	// destructive happens. A stray "conflict" substring in an unrelated
	// error must not trigger recovery.
	postState := sp.trainer.state().state
	if !helmStateFailedOrPending(postState) {
		_, _ = fmt.Fprintf(sp.out,
			"[deps] The install output matches the webhook Secret field-ownership conflict signature, "+
				"but the release state is %q — not recovering automatically.\n", postState)
		printManualTrainerRecovery(sp.out)
		return installErr
	}

	_, _ = fmt.Fprintf(sp.out,
		"[deps] Install failed with webhook Secret field-ownership conflicts and the release is %s (issue #180).\n",
		postState)
	printSecretOwnershipDiagnostics(sp.ctx, sp.c, sp.out)
	if err := refreshRecoveryReleaseEvidence(&sp); err != nil {
		_, _ = fmt.Fprintf(sp.out, "[deps] Automatic recovery refused while refreshing exact release evidence: %v\n", err)
		printManualTrainerRecovery(sp.out)
		return installErr
	}

	printRecoveryQuiescencePrerequisite(sp.out)
	baseline, blockers := recoveryGate(sp, nil, false)
	safe := len(blockers) == 0
	if !safe {
		_, _ = fmt.Fprintln(sp.out, "[deps] Automatic recovery refused:")
		for _, b := range blockers {
			_, _ = fmt.Fprintf(sp.out, "  - %s\n", b)
		}
		printManualTrainerRecovery(sp.out)
		return installErr
	}

	if !sp.autoApprove {
		printTrainerRecoveryPlan(sp.out)
		_, _ = fmt.Fprintf(sp.out,
			"\nDo you want to proceed with automatic recovery? Only 'yes' will be accepted to confirm.\n")
		if !promptForConfirmation(sp.in, sp.out) {
			_, _ = fmt.Fprintln(sp.out, "\n[deps] Recovery declined.")
			printManualTrainerRecovery(sp.out)
			return installErr
		}
		_, _ = fmt.Fprintln(sp.out)
	}

	preUninstall, blockers := recoveryGate(sp, &baseline, false)
	blockers = append(blockers, compareRecoveryEvidence(baseline, preUninstall, false)...)
	if len(blockers) != 0 {
		_, _ = fmt.Fprintln(sp.out, "[deps] Automatic recovery refused after confirmation:")
		for _, blocker := range blockers {
			_, _ = fmt.Fprintf(sp.out, "  - %s\n", blocker)
		}
		// This gate runs after the operator confirmed the destructive plan,
		// so the refusal leaves a still-broken release and no procedure
		// unless the manual one is printed here too.
		printManualTrainerRecovery(sp.out)
		return installErr
	}

	// Exactly one recovery attempt per run; a second failure falls through
	// to the manual procedure instead of looping (ADR-073 decision 5).
	if err := recoverTrainerRelease(sp, baseline); err != nil {
		printManualTrainerRecovery(sp.out)
		return fmt.Errorf("[deps] automatic recovery failed: %w", err)
	}
	return nil
}

func refreshRecoveryReleaseEvidence(sp *setupPhaseParams) error {
	present, err := jobSetCRDExists(sp.ctx, sp.c)
	if err != nil {
		return err
	}
	observed := observeJobSetOwnership(sp.ctx, sp.c, present, "failed", sp.trainer.manifest)
	if observed.mode == jobSetModeUnknown {
		return fmt.Errorf("JobSet ownership became ambiguous: %s", strings.Join(observed.evidence, "; "))
	}
	if sp.jobSetMode == jobSetModeExternal &&
		(observed.mode != jobSetModeExternal || observed.ownershipToken != sp.jobSetOwnershipToken) {
		return fmt.Errorf("external JobSet ownership changed: selected %s, observed %s", sp.jobSetMode, observed.mode)
	}
	if sp.jobSetMode != jobSetModeExternal && observed.mode == jobSetModeExternal {
		return fmt.Errorf("JobSet ownership changed from %s to external", sp.jobSetMode)
	}
	sp.releaseManifestObjects = observed.manifestObjects
	if observed.ownershipToken != "" {
		sp.jobSetOwnershipToken = observed.ownershipToken
	}
	return nil
}

// trainerRecoveryCRDs are the Trainer-owned CRDs recovery deletes. The shared
// JobSet CRD is intentionally absent and is retained in every ownership mode.
var trainerRecoveryCRDs = []string{
	"trainjobs." + trainerAPIGroup,
	"trainingruntimes." + trainerAPIGroup,
	"clustertrainingruntimes." + trainerAPIGroup,
}

// trainerWorkloadGate enforces the cluster-wide custom-resource blockers.
func trainerWorkloadGate(sp setupPhaseParams) (bool, []string) {
	var blockers []string
	for _, group := range []string{trainerAPIGroup, jobsetAPIGroup} {
		resources, err := discoverServedResourcesByGroup(sp.ctx, sp.c, group)
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("cannot list CRDs in group %s: %v", group, err))
			continue
		}
		for _, res := range resources {
			items, err := listNVCRECRs(sp.ctx, sp.c, res)
			if err != nil {
				blockers = append(blockers, fmt.Sprintf("cannot list %s instances: %v", res.kind, err))
				continue
			}
			for _, item := range items {
				name := item.GetName()
				if ns := item.GetNamespace(); ns != "" {
					name = ns + "/" + name
				}
				switch res.kind {
				case "TrainingRuntime", "ClusterTrainingRuntime":
					// Only runtimes proven to belong to this exact release and
					// its stored manifest are recreated by the reinstall.
					_, inManifest := sp.releaseManifestObjects[manifestIdentity(
						item.GetAPIVersion(), item.GetKind(), item.GetNamespace(), item.GetName())]
					if completeHelmOwner(&item).bundled() && inManifest {
						continue
					}
					blockers = append(blockers, fmt.Sprintf(
						"%s %s is not proven to belong to the exact Trainer release manifest; deleting the CRDs would destroy it",
						res.kind, name))
				case "JobSet":
					if sp.jobSetMode == jobSetModeExternal && item.GetNamespace() != trainerNamespace {
						continue
					}
					blockers = append(blockers, fmt.Sprintf(
						"%s %s exists; recovery would remove its controller or namespace", res.kind, name))
				default:
					blockers = append(blockers, fmt.Sprintf(
						"%s %s exists; recovery deletes its CRD", res.kind, name))
				}
			}
		}
	}
	sort.Strings(blockers)
	return len(blockers) == 0, blockers
}

func recoveryGate(
	sp setupPhaseParams, prior *recoveryEvidence, trainerAPIsRemoved bool,
) (recoveryEvidence, []string) {
	_, workloadBlockers := trainerWorkloadGate(sp)
	evidence, inventoryBlockers := collectRecoveryEvidence(sp, prior, trainerAPIsRemoved)
	blockers := append(workloadBlockers, inventoryBlockers...)
	if ownershipBlocker := revalidateRecoveryJobSetOwnership(sp, trainerAPIsRemoved); ownershipBlocker != "" {
		blockers = append(blockers, ownershipBlocker)
	}
	sort.Strings(blockers)
	return evidence, blockers
}

func revalidateRecoveryJobSetOwnership(sp setupPhaseParams, trainerRemoved bool) string {
	present, err := jobSetCRDExists(sp.ctx, sp.c)
	if err != nil {
		return "cannot revalidate JobSet CRD ownership: " + err.Error()
	}
	state := "failed"
	manifest := sp.trainer.manifest
	if trainerRemoved {
		state = helmStateNotInstalled
		manifest = nil
	}
	observed := observeJobSetOwnership(sp.ctx, sp.c, present, state, manifest)
	// The mode can agree while the controller behind it does not. Every token
	// part embeds a UID, so a replaced external controller shows up here and
	// reporting it as a mode change would name the same mode on both sides.
	if sp.jobSetMode == jobSetModeExternal && observed.mode == jobSetModeExternal &&
		observed.ownershipToken != sp.jobSetOwnershipToken {
		return "external JobSet controller identity changed since it was verified; recovery would proceed against " +
			"a controller it has not inspected (" + strings.Join(observed.evidence, "; ") + ")"
	}
	if observed.mode == jobSetModeUnknown || observed.mode == jobSetModeExternal && sp.jobSetMode != jobSetModeExternal ||
		sp.jobSetMode == jobSetModeExternal && observed.mode != jobSetModeExternal {
		return fmt.Sprintf("JobSet ownership changed or became ambiguous: selected %s, observed %s (%s)",
			sp.jobSetMode, observed.mode, strings.Join(observed.evidence, "; "))
	}
	return ""
}

// recoverTrainerRelease performs the gated recovery exactly once. It repeats
// workload checks at every CRD boundary, inventories the namespace again, and
// deletes only the inspected namespace UID.
func recoverTrainerRelease(sp setupPhaseParams, baseline recoveryEvidence) error {
	out := sp.out
	_, _ = fmt.Fprintf(out, "[deps][recover] Uninstalling Helm release %q from namespace %s...\n",
		trainerReleaseName, trainerNamespace)
	if err := sp.trainer.uninstall(out); err != nil {
		return fmt.Errorf("uninstall %s failed; outcome may be partial and no further cleanup was attempted: %w",
			trainerReleaseName, err)
	}

	_, _ = fmt.Fprintln(out, "[deps][recover] Removing Kubeflow Trainer CRDs (JobSet CRD is retained)...")
	for _, name := range trainerRecoveryCRDs {
		if err := deleteTrainerCRDWithRecheck(sp, name); err != nil {
			return fmt.Errorf("partial recovery after Trainer uninstall: %w", err)
		}
	}

	finalEvidence, blockers := recoveryGate(sp, &baseline, true)
	blockers = append(blockers, compareRecoveryEvidence(baseline, finalEvidence, true)...)
	if len(blockers) != 0 {
		return fmt.Errorf("partial recovery stopped before namespace deletion: %s", strings.Join(blockers, "; "))
	}
	// deleteNamespaceWithUID issues no Delete without an inspected UID, since
	// there is no precondition to attach. Say so rather than reporting a
	// deletion that never happened: collectRecoveryEvidence leaves the UID
	// unset when the namespace was already absent at the baseline gate.
	namespaceOutcome := fmt.Sprintf("deleted namespace %s", trainerNamespace)
	if baseline.namespaceUID == "" {
		namespaceOutcome = fmt.Sprintf("namespace %s was already absent", trainerNamespace)
		_, _ = fmt.Fprintf(out, "[deps][recover] Namespace %s is already absent; no deletion was required.\n",
			trainerNamespace)
	} else {
		_, _ = fmt.Fprintf(out, "[deps][recover] Deleting namespace %s...\n", trainerNamespace)
	}
	if err := deleteNamespaceWithUID(sp, baseline.namespaceUID); err != nil {
		return fmt.Errorf("partial recovery stopped after Trainer CRD deletion: %w", err)
	}

	_, _ = fmt.Fprintf(out, "[deps][recover] Reinstalling Kubeflow Trainer %s...\n", kubeflowTrainerVersion)
	if output, err := sp.trainer.install(sp.jobSetMode, out); err != nil {
		printTrainerFailureGuidance(sp, output)
		return fmt.Errorf("reinstall: %w", err)
	}

	_, _ = fmt.Fprintf(out,
		"[deps][recover] Recovery complete: uninstalled release %q, deleted Trainer CRDs (%s), retained JobSet CRD %s, %s, reinstalled Kubeflow Trainer %s.\n",
		trainerReleaseName, strings.Join(trainerRecoveryCRDs, ", "), jobSetCRDName, namespaceOutcome, kubeflowTrainerVersion)
	return nil
}

// printTrainerRecoveryPlan prints the recovery plan for interactive
// re-confirmation before anything is deleted (ADR-073 decision 5).
func printTrainerRecoveryPlan(out io.Writer) {
	_, _ = fmt.Fprintln(out, "\n  Recovery plan (issue #180):")
	_, _ = fmt.Fprintf(out, "    1. Uninstall Helm release %q (namespace: %s)\n", trainerReleaseName, trainerNamespace)
	_, _ = fmt.Fprintf(out, "    2. Delete Trainer CRDs: %s (retain %s)\n", strings.Join(trainerRecoveryCRDs, ", "), jobSetCRDName)
	_, _ = fmt.Fprintf(out, "    3. Delete namespace %s and wait for termination\n", trainerNamespace)
	_, _ = fmt.Fprintf(out, "    4. Reinstall Kubeflow Trainer %s\n", kubeflowTrainerVersion)
}

func printRecoveryQuiescencePrerequisite(out io.Writer) {
	_, _ = fmt.Fprintln(out,
		"[deps] Recovery prerequisite: keep kubeflow-system protected-resource writes and Trainer custom-resource writes quiescent until recovery stops or completes.")
	_, _ = fmt.Fprintln(out,
		"[deps] If recovery removes the bundled JobSet controller, JobSet creation must also remain quiescent. --auto-approve does not establish this condition.")
}

// printManualTrainerRecovery prints the field-validated manual recovery
// procedure from issue #180. It is printed on every fail-fast path.
func printManualTrainerRecovery(out io.Writer) {
	_, _ = fmt.Fprintln(out, "\n[deps] Operator-managed recovery guidance (issue #180):")
	_, _ = fmt.Fprintf(out, "  1. helm uninstall %s --namespace %s\n", trainerReleaseName, trainerNamespace)
	_, _ = fmt.Fprintf(out, "  2. kubectl delete crd %s  # retain %s\n", strings.Join(trainerRecoveryCRDs, " "), jobSetCRDName)
	_, _ = fmt.Fprintf(out, "  3. Inventory %s and delete it only after applying the same protected-resource and quiescence checks.\n", trainerNamespace)
	_, _ = fmt.Fprintf(out, "  4. Re-run 'nvcrectl setup init' to reinstall Kubeflow Trainer %s. Earlier cleanup is not rolled back.\n", kubeflowTrainerVersion)
}

// printSecretOwnershipDiagnostics prints the field managers of every Secret
// in the trainer namespace. The managedFields probe is diagnostic output
// only; it is never used as the detector (ADR-073 decision 2).
func printSecretOwnershipDiagnostics(ctx context.Context, c client.Client, out io.Writer) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("v1")
	list.SetKind("SecretList")
	if err := c.List(ctx, list, client.InNamespace(trainerNamespace)); err != nil {
		_, _ = fmt.Fprintf(out, "[deps] (diagnostics unavailable: list Secrets in %s: %v)\n",
			trainerNamespace, err)
		return
	}
	_, _ = fmt.Fprintf(out, "[deps] Secret field ownership in namespace %s:\n", trainerNamespace)
	for _, item := range list.Items {
		managers := make([]string, 0, len(item.GetManagedFields()))
		for _, mf := range item.GetManagedFields() {
			managers = append(managers, mf.Manager)
		}
		sort.Strings(managers)
		ownership := "(no field managers recorded)"
		if len(managers) > 0 {
			ownership = "managed by [" + strings.Join(managers, ", ") + "]"
		}
		_, _ = fmt.Fprintf(out, "  %s: %s\n", item.GetName(), ownership)
	}
}

func setupControllerSecret(
	sp setupPhaseParams, imagePullSecret string,
) (string, error) {
	if imagePullSecret == "" {
		return "", nil
	}
	if _, err := EnsureNamespace(sp.ctx, sp.c, nvcreNamespace, sp.out); err != nil {
		return "", fmt.Errorf("[helm] %w", err)
	}
	name, _, err := CreateImagePullSecret(sp.ctx, sp.c,
		nvcreNamespace, pullSecretName, defaultImageRegistry, "token", imagePullSecret)
	if err != nil {
		return "", fmt.Errorf("[helm] create pull secret: %w", err)
	}
	_, _ = fmt.Fprintf(sp.out,
		"[helm] Created image pull secret %q in namespace %s.\n",
		name, nvcreNamespace)
	return name, nil
}

// ---------------------------------------------------------------------------
// nvcrectl setup reset
// ---------------------------------------------------------------------------

func newResetCommand() *cobra.Command {
	var skipPhases string
	var autoApprove bool

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Remove NVCRE from the target cluster",
		Long: `Removes NVCRE and its dependencies via Helm.

Phases:
  [cr]    NVCRE custom resource instances (Certifications, Workflows, etc.)
  [helm]  NVCRE Helm release (CRDs, controller, LogProfiles)
  [deps]  Kubeflow Trainer ` + kubeflowTrainerVersion + `

Shared namespaces and the controller pull secret are intentionally retained,
as is the shared JobSet CRD. reset prints a "Retained resources" list; entries
that are safe to remove carry a cleanup command, while the JobSet CRD is
reported as a warning only, because deleting it destroys JobSets in every
namespace.

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
	skip, err := parseSkipPhases(skipPhases, resetSkipPhases...)
	if err != nil {
		return err
	}
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
		printPhaseList(out, skip, resetPhaseSummary())
		_, _ = fmt.Fprintf(out,
			"\nDo you want to proceed? Only 'yes' will be accepted to confirm.\n")
		if !promptForConfirmation(in, out) {
			_, _ = fmt.Fprintln(out, "\nReset cancelled.")
			return nil
		}
		_, _ = fmt.Fprintln(out)
	}

	// [cr] — Delete all NVCRE custom resource instances while the
	// controller is still alive to process finalizer removal.
	if skip[phaseCR] {
		_, _ = fmt.Fprintln(out, "[cr] Skipped.")
	} else {
		if err := deleteNVCRECRs(ctx, c, out); err != nil {
			return fmt.Errorf("[cr] %w", err)
		}
	}

	if skip[phaseHelm] {
		_, _ = fmt.Fprintln(out, "[helm] Skipped.")
	} else {
		if err := uninstallHelmRelease(helmUninstallParams{
			kubeconfig:  kubeconfigPath,
			kubeContext: kubeContext,
			out:         out,
		}); err != nil {
			return fmt.Errorf("[helm] %w", err)
		}

		// Helm intentionally never deletes CRDs that live in a chart's crds/
		// directory. Delete NVCRE's CRDs explicitly when this phase runs.
		if err := deleteCRDsByGroup(ctx, c, nvcreAPIGroup, "[helm]", "NVCRE", out); err != nil {
			return fmt.Errorf("[helm] %w", err)
		}
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
	printRetainedResources(ctx, c, skip, out)
	_, _ = fmt.Fprintln(out, "\nNVCRE reset successfully.")
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

	// Helm leaves chart CRDs behind. Remove only Trainer-owned CRDs; the
	// shared JobSet CRD is retained for cluster-wide consumers.
	if err := deleteCRDsByGroup(sp.ctx, sp.c, trainerAPIGroup, "[deps]", "Kubeflow Trainer", sp.out); err != nil {
		return fmt.Errorf("[deps] %w", err)
	}
	return nil
}

// retainedResource is a resource reset intentionally leaves behind, paired
// with the kubectl command that removes it.
type retainedResource struct {
	description string
	cleanup     string
	// lookupError preserves a failed existence check (e.g. RBAC denied),
	// so the resource is reported as "may remain" with the underlying cause.
	lookupError error
}

// printRetainedResources reports what reset intentionally keeps — the shared
// namespaces (`helm uninstall` never deletes the release namespace, and other
// tenants may use it) and the controller pull secret created by setup init
// outside the Helm release — together with the kubectl command to remove
// each. Existence is checked with the client already in hand; resources
// confirmed absent are omitted.
func printRetainedResources(
	ctx context.Context, c client.Client, skip map[string]bool, out io.Writer,
) {
	var retained []retainedResource
	record := func(exists bool, err error, r retainedResource) {
		if err != nil {
			r.lookupError = err
			retained = append(retained, r)
			return
		}
		if exists {
			retained = append(retained, r)
		}
	}

	var exists bool
	var err error
	if !skip[phaseHelm] {
		exists, err = namespaceExists(ctx, c, nvcreNamespace)
		record(exists, err, retainedResource{
			description: "Namespace " + nvcreNamespace,
			cleanup:     "kubectl delete namespace " + nvcreNamespace,
		})

		exists, err = secretExists(ctx, c, nvcreNamespace, pullSecretName)
		record(exists, err, retainedResource{
			description: fmt.Sprintf("Secret %s/%s", nvcreNamespace, pullSecretName),
			cleanup: fmt.Sprintf("kubectl delete secret %s -n %s",
				pullSecretName, nvcreNamespace),
		})
	}

	// Only mention the Trainer namespace when the deps phase actually ran;
	// with --skip-phases=deps the kubeflow-trainer release still lives there.
	if !skip[phaseDeps] {
		exists, err = namespaceExists(ctx, c, trainerNamespace)
		record(exists, err, retainedResource{
			description: "Namespace " + trainerNamespace,
			cleanup:     "kubectl delete namespace " + trainerNamespace,
		})

		exists, err = jobSetCRDExists(ctx, c)
		record(exists, err, retainedResource{
			description: "JobSet CRD jobsets." + jobsetAPIGroup +
				" (deleting it destroys JobSets across all namespaces)",
			cleanup: "",
		})
	}

	if len(retained) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "\nRetained resources (not removed by reset):")
	for _, r := range retained {
		if r.lookupError != nil {
			_, _ = fmt.Fprintf(out, "  - %s (may remain; existence check failed: %v)\n", r.description, r.lookupError)
		} else {
			_, _ = fmt.Fprintf(out, "  - %s\n", r.description)
		}
		if r.cleanup != "" {
			_, _ = fmt.Fprintf(out, "      %s\n", r.cleanup)
		}
	}
}

// namespaceExists reports whether the named namespace exists. NotFound is not
// an error; any other lookup failure is returned so the caller can report the
// resource as unverified instead of silently dropping it.
func namespaceExists(ctx context.Context, c client.Client, name string) (bool, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// secretExists reports whether the named Secret exists, with the same error
// semantics as namespaceExists.
func secretExists(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Phase helpers
// ---------------------------------------------------------------------------

// phaseInfo describes a phase for display in the confirmation summary.
type phaseInfo struct {
	name        string
	description string
}

// initPhaseSummary and resetPhaseSummary are the phase lists the confirmation
// prompt prints, kept beside the accepted --skip-phases sets below so the
// summary cannot advertise a phase the command does not gate. That divergence
// is what made `--skip-phases=helm` a silent no-op that only changed the
// printed summary.
func initPhaseSummary(trainerChartRef, chartRef, image string) []phaseInfo {
	return []phaseInfo{
		{phaseDeps, fmt.Sprintf("Kubeflow Trainer %s (%s)", kubeflowTrainerVersion, trainerChartRef)},
		{phaseHelm, fmt.Sprintf("NVCRE Helm chart (%s), image %s", chartRef, image)},
	}
}

func resetPhaseSummary() []phaseInfo {
	return []phaseInfo{
		{phaseCR, "NVCRE custom resources"},
		{phaseHelm, "NVCRE Helm release (CRDs, controller, LogProfiles)"},
		{phaseDeps, fmt.Sprintf("Kubeflow Trainer %s", kubeflowTrainerVersion)},
	}
}

// initSkipPhases and resetSkipPhases are the phase names each command accepts
// in --skip-phases.
var (
	initSkipPhases  = []string{phaseDeps, phaseHelm}
	resetSkipPhases = []string{phaseCR, phaseHelm, phaseDeps}
)

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
func parseSkipPhases(s string, allowed ...string) (map[string]bool, error) {
	skip := make(map[string]bool)
	if s == "" {
		return skip, nil
	}
	valid := make(map[string]struct{}, len(allowed))
	for _, phase := range allowed {
		valid[phase] = struct{}{}
	}
	for phase := range strings.SplitSeq(s, ",") {
		phase = strings.TrimSpace(phase)
		if phase != "" {
			if _, ok := valid[phase]; !ok {
				return nil, fmt.Errorf("unknown phase %q; accepted phases: %s", phase, strings.Join(allowed, ", "))
			}
			skip[phase] = true
		}
	}
	return skip, nil
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

func newSetupDiscovery(cf *kubeconfig.ConfigFlags) (func() ([]*metav1.APIResourceList, error), error) {
	restConfig, err := cf.ToRESTConfig()
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig: %w", err)
	}
	d, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build discovery client: %w", err)
	}
	return d.ServerPreferredNamespacedResources, nil
}

// ---------------------------------------------------------------------------
// [cr] phase — NVCRE custom resource cleanup
// ---------------------------------------------------------------------------

// deleteNVCRECRs implements the [cr] reset phase. It performs graceful,
// controller-driven cleanup only and fails if resources remain.
func deleteNVCRECRs(ctx context.Context, c client.Client, out io.Writer) error {
	_, _ = fmt.Fprintln(out, "[cr] Deleting NVCRE custom resources...")

	resources, err := discoverNVCREResources(ctx, c)
	if err != nil {
		return fmt.Errorf("discover NVCRE resources: %w", err)
	}
	if len(resources) == 0 {
		_, _ = fmt.Fprintln(out, "[cr] No NVCRE custom resources found.")
		return nil
	}

	// Check if any NVCRE CRs exist at all.
	if !anyNVCRECRsExist(ctx, c, resources) {
		_, _ = fmt.Fprintln(out, "[cr] No NVCRE custom resources found.")
		return nil
	}

	// Stage 1: Graceful deletion — delete all NVCRE resources and let
	// controllers reconcile finalizers/ownership cleanup.
	gracefulCtx, gracefulCancel := context.WithTimeout(ctx, crGracefulTimeout)
	defer gracefulCancel()

	gracefulCascadeDelete(gracefulCtx, c, out, resources)

	// Wait for remaining resources to terminate naturally.
	if err := waitForAllNVCRECRsGone(gracefulCtx, c, resources); err != nil {
		return fmt.Errorf("graceful cleanup did not complete within %s: %w",
			crGracefulTimeout, err)
	}

	_, _ = fmt.Fprintln(out, "  All NVCRE custom resources removed.")
	return nil
}

// gracefulCascadeDelete deletes all known NVCRE resources.
func gracefulCascadeDelete(
	ctx context.Context, c client.Client, out io.Writer, resources []nvcreResource,
) {
	for _, res := range resources {
		items, err := listNVCRECRs(ctx, c, res)
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

// waitForAllNVCRECRsGone polls until no NVCRE CR instances remain.
func waitForAllNVCRECRsGone(
	ctx context.Context, c client.Client, resources []nvcreResource,
) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		if !anyNVCRECRsExist(ctx, c, resources) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// listNVCRECRs lists all instances of an NVCRE resource type across
// all namespaces using an unstructured client.
func listNVCRECRs(
	ctx context.Context, c client.Client, res nvcreResource,
) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(res.apiVersion)
	list.SetKind(res.kind + "List")
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// anyNVCRECRsExist returns true if any NVCRE CR instances exist
// across all resource types.
func anyNVCRECRsExist(
	ctx context.Context, c client.Client, resources []nvcreResource,
) bool {
	for _, res := range resources {
		items, err := listNVCRECRs(ctx, c, res)
		if err != nil {
			continue
		}
		if len(items) > 0 {
			return true
		}
	}
	return false
}

// discoverNVCREResources returns all CRD-backed NVCRE resources for the
// configured API group, including their served apiVersion and Kind metadata.
func discoverNVCREResources(ctx context.Context, c client.Client) ([]nvcreResource, error) {
	resources, err := discoverResourcesByGroup(ctx, c, nvcreAPIGroup)
	if err != nil {
		return nil, err
	}
	filtered := resources[:0]
	for _, res := range resources {
		if res.kind == "LogProfile" {
			continue // LogProfiles are handled by the [logprofiles] phase, not [cr]
		}
		filtered = append(filtered, res)
	}
	return filtered, nil
}

// discoverResourcesByGroup returns all CRD-backed resources for an API group
// that expose a served version, skipping any CRD that does not. Reset's [cr]
// phase uses this: an inspection gap must not stop an explicit cleanup the
// operator asked for (ADR-078 decision 3), and the later [helm] phase still
// removes the CRD.
func discoverResourcesByGroup(ctx context.Context, c client.Client, apiGroup string) ([]nvcreResource, error) {
	return discoverGroupResources(ctx, c, apiGroup, false)
}

// discoverServedResourcesByGroup is the fail-closed variant: a CRD in the
// group with no served version is an error. The recovery gate uses this,
// because a CRD it cannot enumerate is a CRD whose stored objects it cannot
// see, and it is about to delete CRDs and the namespace (ADR-078 decision 4).
// Kubernetes requires exactly one storage version but no served one, so this
// state is reachable through a half-applied or deliberately disabled CRD.
func discoverServedResourcesByGroup(ctx context.Context, c client.Client, apiGroup string) ([]nvcreResource, error) {
	return discoverGroupResources(ctx, c, apiGroup, true)
}

func discoverGroupResources(
	ctx context.Context, c client.Client, apiGroup string, requireServed bool,
) ([]nvcreResource, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return nil, err
	}

	resources := make([]nvcreResource, 0, len(list.Items))
	for _, item := range list.Items {
		group, found, err := unstructured.NestedString(item.Object, "spec", "group")
		if err != nil || !found || group != apiGroup {
			continue
		}
		kind, found, err := unstructured.NestedString(item.Object, "spec", "names", "kind")
		if err != nil || !found || kind == "" {
			continue
		}
		resource, found, err := unstructured.NestedString(item.Object, "spec", "names", "plural")
		if err != nil || !found || resource == "" {
			continue
		}

		apiVersion := ""
		versions, found, err := unstructured.NestedSlice(item.Object, "spec", "versions")
		if err != nil {
			return nil, fmt.Errorf("inspect CRD %s versions: %w", item.GetName(), err)
		}
		if found {
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
			if requireServed {
				return nil, fmt.Errorf("CRD %s in group %s has no served version", item.GetName(), apiGroup)
			}
			continue
		}

		resources = append(resources, nvcreResource{
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

// deleteCRDsByGroup deletes all CustomResourceDefinitions belonging to the
// given API group and waits for them to be fully removed. Helm intentionally
// never deletes CRDs on `helm uninstall` (to avoid accidental data loss), so
// every phase that installs CRDs via Helm needs this explicit cleanup.
// phaseTag (e.g. "[helm]", "[deps]") and label (e.g. "NVCRE") are used
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
			Name: name,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "nvcrectl",
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

const (
	// pullSecretName is the controller image pull secret created by setup init.
	pullSecretName = "nvcrectl-pull-secret" // #nosec G101 -- Kubernetes Secret name, not a credential
)

// WorkloadPullSecretName returns the name of the workload image pull secret for
// a given Certification or WorkloadRun. Deriving from the resource name prevents
// concurrent runs in the same namespace from fighting over a single fixed name.
func WorkloadPullSecretName(resourceName string) string {
	return "nvcrectl-pull-" + resourceName // #nosec G101 -- Kubernetes Secret name, not a credential
}

// CreateImagePullSecret creates a dockerconfigjson Secret for the given registry.
// server is the registry hostname (e.g. "nvcr.io", "ghcr.io").
// username is the registry username (e.g. "$oauthtoken" for NGC, "token" for GHCR).
// password is the registry password or API key.
// secretName is the name to give the created Secret.
// Returns (secretName, wasCreated, error) where wasCreated is true only when a
// new Secret was created — false means an existing Secret was updated. Callers
// must only delete the secret on rollback when wasCreated is true.
func CreateImagePullSecret(ctx context.Context, c client.Client, namespace, secretName, server, username, password string) (string, bool, error) {
	if server == "" || username == "" || password == "" {
		return "", false, fmt.Errorf("workload-registry, workload-registry-username, and workload-registry-password must all be non-empty")
	}
	authStr := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	type authEntry struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Auth     string `json:"auth"`
	}
	dockerCfg := struct {
		Auths map[string]authEntry `json:"auths"`
	}{
		Auths: map[string]authEntry{
			server: {Username: username, Password: password, Auth: authStr},
		},
	}
	dockerConfigBytes, err := json.Marshal(dockerCfg)
	if err != nil {
		return "", false, fmt.Errorf("marshal dockerconfigjson: %w", err)
	}
	dockerConfig := string(dockerConfigBytes)

	secret := &corev1.Secret{
		Name:      secretName,
		Namespace: namespace,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "nvcrectl",
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerConfig),
		},
	}

	if err := c.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			existing := &corev1.Secret{}
			if getErr := c.Get(ctx, client.ObjectKey{Name: secretName, Namespace: namespace}, existing); getErr != nil {
				return "", false, fmt.Errorf("get existing secret: %w", getErr)
			}
			// Secret.type is immutable and we must not delete a secret we may not own.
			// Return an actionable error so the user can resolve it explicitly.
			if existing.Type != corev1.SecretTypeDockerConfigJson {
				return "", false, fmt.Errorf(
					"secret %q already exists with type %q (want %q): delete it manually and retry",
					secretName, existing.Type, corev1.SecretTypeDockerConfigJson)
			}
			existing.Data = secret.Data
			if updateErr := c.Update(ctx, existing); updateErr != nil {
				return "", false, fmt.Errorf("update secret: %w", updateErr)
			}
			return secretName, false, nil // updated existing, caller does not own it
		}
		return "", false, fmt.Errorf("create image pull secret: %w", err)
	}

	return secretName, true, nil
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
