// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const outputJSON = "json"

// The diagnostics/dcgm-level4 category runs dcgmi against this service. The
// GPU Operator creates it only when spec.dcgm.enabled is true.
const (
	dcgmServiceName      = "nvidia-dcgm"
	dcgmServiceNamespace = "gpu-operator"
)

const (
	// trainerDeploymentName is the Trainer controller Deployment the
	// kubeflow-trainer Helm chart creates in trainerNamespace.
	trainerDeploymentName = "kubeflow-trainer-controller-manager"
	// trainerContainerName is the manager container inside that Deployment.
	// The image tag is read from this container by name, so a sidecar with a
	// semver tag can never be mistaken for the Trainer version.
	trainerContainerName = "manager"
	// trainerVersionLabel is the standard chart version label Helm stamps on
	// the CRDs it installs.
	trainerVersionLabel = "app.kubernetes.io/version"
)

// Sources a detected Trainer version can come from, in detection order.
const (
	trainerVersionSourceHelm       = "helm release"
	trainerVersionSourceDeployment = "deployment image"
	trainerVersionSourceCRDLabel   = "crd label"
)

// SetupStatus is the JSON output structure for nvcrectl setup status.
type SetupStatus struct {
	// Installed is true when all required components are present and ready
	// and no managed Helm release is in a failed or pending state.
	Installed  bool                  `json:"installed"`
	Components SetupStatusComponents `json:"components"`
	// KubeflowTrainerVersion reports the detected Kubeflow Trainer version
	// against the version this build supports. Nil when the TrainJob CRD is
	// not installed.
	KubeflowTrainerVersion *TrainerVersionStatus `json:"kubeflowTrainerVersion,omitempty"`
	// HelmReleases reports the state of the Helm releases setup init manages.
	HelmReleases []HelmReleaseStatus `json:"helmReleases"`

	// dcgmAbsent is true only when the API server answered that the service
	// does not exist. A denied or failed lookup leaves it false, so the
	// command does not tell the user to enable a service that may be there.
	dcgmAbsent bool
}

// TrainerVersionStatus reports the installed Kubeflow Trainer version
// against the version this NVCRE build is pinned to.
type TrainerVersionStatus struct {
	// Supported is the Trainer version this NVCRE build supports.
	Supported string `json:"supported"`
	// Detected is the installed Trainer version, empty when it could not be
	// determined.
	Detected string `json:"detected,omitempty"`
	// Source names where Detected came from: "helm release", "deployment
	// image", or "crd label".
	Source string `json:"source,omitempty"`
}

// HelmReleaseStatus reports the Helm state of one managed release.
type HelmReleaseStatus struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// State is the release state helm reports ("deployed", "failed",
	// "pending-upgrade", ...), "not installed" when helm has no record of
	// the release, or "unknown" when the query could not be completed.
	State string `json:"state"`
}

// SetupStatusComponents reports the status of each individual component.
type SetupStatusComponents struct {
	NVCRECRDs       bool `json:"nvcreCRDs"`
	NVCREController bool `json:"nvcreController"`
	KubeflowTrainer bool `json:"kubeflowTrainer"`
	LogProfiles     bool `json:"logProfiles"`
	GPUOperator     bool `json:"gpuOperator"`
	// DCGM is optional. Only the diagnostics/dcgm-level4 category needs it,
	// so it does not count toward Installed.
	DCGM bool `json:"dcgm"`
}

// allRequired reports whether every required component is present, ignoring
// Helm release health. DCGM is optional and does not count.
func (c SetupStatusComponents) allRequired() bool {
	return c.NVCRECRDs &&
		c.NVCREController &&
		c.KubeflowTrainer &&
		c.LogProfiles &&
		c.GPUOperator
}

func newSetupStatusCommand() *cobra.Command {
	var output string

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report the installation status of NVCRE and its dependencies",
		Long: `Check whether NVCRE and all required cluster components are installed and ready.

Components checked:
  nvcreCRDs        NVCRE CustomResourceDefinitions (nvcre.nvidia.com)
  nvcreController  NVCRE controller deployment (namespace: nvcre)
  kubeflowTrainer      Kubeflow Trainer TrainJob CRD and version (supported: ` + kubeflowTrainerVersion + `)
  logProfiles          NVCRE LogProfile resources
  gpuOperator          NVIDIA GPU Operator (nodes with nvidia.com/gpu.present=true)
  dcgm                 NVIDIA DCGM service (optional; diagnostics/dcgm-level4 only)

The Helm releases managed by 'setup init' (nvcre and
kubeflow-trainer) are also checked via the helm CLI.

The installed Kubeflow Trainer version is detected from the managed Helm
release, the Trainer controller Deployment image tag, or the CRD
app.kubernetes.io/version label — whichever answers first. A version other
than the supported one fails the kubeflowTrainer check; a Trainer install
whose version cannot be determined passes with a warning.

'installed' is true only when all required components are present and no
managed Helm release is in a failed or pending state. DCGM is optional, so it
does not affect 'installed'. A release helm has no record of, or that cannot
be queried (helm not in PATH), does not affect 'installed' either.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c, err := newSetupClient(configFlags)
			if err != nil {
				return fmt.Errorf("connect to cluster: %w", err)
			}

			query := newHelmStateQuery(*configFlags.KubeConfig, *configFlags.Context)
			trainerState := newTrainerStateQuery(*configFlags.KubeConfig, *configFlags.Context)
			status := collectSetupStatus(ctx, c, query, trainerState)

			switch output {
			case outputJSON:
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(status)
			default:
				printSetupStatus(os.Stdout, status)
				return nil
			}
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table, json")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// collectSetupStatus queries the cluster and the helm CLI and returns the
// current component and release status.
func collectSetupStatus(
	ctx context.Context, c client.Client, query helmStateFunc, trainerState trainerStateFunc,
) *SetupStatus {
	comp := SetupStatusComponents{}

	comp.NVCRECRDs = checkNVCRECRDs(ctx, c)
	comp.NVCREController = checkNVCREController(ctx, c)
	comp.LogProfiles = checkLogProfiles(ctx, c)
	comp.GPUOperator = checkGPUOperator(ctx, c)

	// One helm query answers both the trainer release row and the version
	// detection, so status does not run the same subprocess twice.
	trainerHelmState, trainerChartVersion := trainerState()

	// The TrainJob CRD must exist, and when the installed Trainer version can
	// be determined it must be the supported one. A Trainer whose version
	// cannot be determined passes with a warning, not a failure: NVCRE may
	// have been installed out-of-band and a version probe must not brick the
	// status command.
	var trainerVersion *TrainerVersionStatus
	if trainJobCRD := findTrainJobCRD(ctx, c); trainJobCRD != nil {
		tv := detectTrainerVersion(ctx, c, trainerHelmState, trainerChartVersion, trainJobCRD)
		trainerVersion = &tv
		comp.KubeflowTrainer = tv.Detected == "" || trainerVersionMatches(tv.Detected)
	}

	dcgmErr := checkDCGM(ctx, c)
	comp.DCGM = dcgmErr == nil

	releases := checkHelmReleases(query, trainerHelmState)

	return &SetupStatus{
		Installed:              comp.allRequired() && len(unhealthyHelmReleases(releases)) == 0,
		Components:             comp,
		KubeflowTrainerVersion: trainerVersion,
		HelmReleases:           releases,
		dcgmAbsent:             apierrors.IsNotFound(dcgmErr),
	}
}

// checkHelmReleases reports the state of every Helm release setup init
// manages. The trainer release state comes from the caller's version query,
// which already asked helm about that release.
func checkHelmReleases(query helmStateFunc, trainerHelmState string) []HelmReleaseStatus {
	return []HelmReleaseStatus{
		{Name: helmReleaseName, Namespace: nvcreNamespace, State: query(helmReleaseName, nvcreNamespace)},
		{Name: trainerReleaseName, Namespace: trainerNamespace, State: trainerHelmState},
	}
}

// helmStateBlocksReady reports whether a release state must block readiness.
// Failed and pending states block. Deployed does not. Absent and unknown
// states do not block either: NVCRE may have been installed without Helm, and
// a missing helm binary must not fail the status command.
func helmStateBlocksReady(state string) bool {
	switch state {
	case helmStateDeployed, helmStateUninstalled, helmStateNotInstalled, helmStateUnknown:
		return false
	}
	return true
}

// unhealthyHelmReleases returns the releases whose state blocks readiness.
func unhealthyHelmReleases(releases []HelmReleaseStatus) []HelmReleaseStatus {
	var unhealthy []HelmReleaseStatus
	for _, rel := range releases {
		if helmStateBlocksReady(rel.State) {
			unhealthy = append(unhealthy, rel)
		}
	}
	return unhealthy
}

// checkNVCRECRDs returns true if NVCRE CRDs are installed.
func checkNVCRECRDs(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return false
	}
	for _, item := range list.Items {
		group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
		if group == nvcreAPIGroup {
			return true
		}
	}
	return false
}

// checkNVCREController returns true if the NVCRE controller deployment
// has at least one available replica in the nvcre namespace.
func checkNVCREController(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apps/v1")
	list.SetKind("DeploymentList")
	if err := c.List(ctx, list, client.InNamespace(nvcreNamespace)); err != nil {
		return false
	}
	for _, item := range list.Items {
		available, _, _ := unstructured.NestedInt64(item.Object, "status", "availableReplicas")
		if available > 0 {
			return true
		}
	}
	return false
}

// findTrainJobCRD returns the Kubeflow TrainJob CRD, or nil when it is not
// installed (or the CRD list could not be read).
func findTrainJobCRD(ctx context.Context, c client.Client) *unstructured.Unstructured {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return nil
	}
	for i := range list.Items {
		item := &list.Items[i]
		group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(item.Object, "spec", "names", "kind")
		if group == trainerAPIGroup && kind == "TrainJob" {
			return item
		}
	}
	return nil
}

// detectTrainerVersion determines the installed Kubeflow Trainer version.
// Detection order: the managed Helm release chart version, the Trainer
// controller Deployment image tag, then the app.kubernetes.io/version label
// on the TrainJob CRD. A source that yields nothing falls through to the
// next; when none answer, Detected stays empty.
func detectTrainerVersion(
	ctx context.Context, c client.Client, trainerHelmState, trainerChartVersion string,
	trainJobCRD *unstructured.Unstructured,
) TrainerVersionStatus {
	tv := TrainerVersionStatus{Supported: kubeflowTrainerVersion}

	if trainerHelmState == helmStateDeployed && trainerChartVersion != "" {
		tv.Detected, tv.Source = trainerChartVersion, trainerVersionSourceHelm
		return tv
	}
	if tag := trainerDeploymentImageTag(ctx, c); tag != "" {
		tv.Detected, tv.Source = tag, trainerVersionSourceDeployment
		return tv
	}
	if v := trainJobCRD.GetLabels()[trainerVersionLabel]; v != "" {
		tv.Detected, tv.Source = v, trainerVersionSourceCRDLabel
		return tv
	}
	return tv
}

// trainerDeploymentImageTag returns the image tag of the manager container in
// the Trainer controller Deployment, or "" when the Deployment or container is
// absent or the tag does not look like a version (a "latest" tag or a digest
// says nothing about the release). The container is matched by name so a
// sidecar with a semver tag is never read as the Trainer version.
func trainerDeploymentImageTag(ctx context.Context, c client.Client) string {
	dep := &unstructured.Unstructured{}
	dep.SetAPIVersion("apps/v1")
	dep.SetKind("Deployment")
	key := client.ObjectKey{Name: trainerDeploymentName, Namespace: trainerNamespace}
	if err := c.Get(ctx, key, dep); err != nil {
		return ""
	}
	containers, found, err := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
	if err != nil || !found {
		return ""
	}
	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := container["name"].(string); name != trainerContainerName {
			continue
		}
		image, _ := container["image"].(string)
		_, tag := parseImage(image)
		if !isReleaseBuild(tag) {
			return ""
		}
		return tag
	}
	return ""
}

// trainerVersionMatches reports whether a detected Trainer version equals the
// supported pin, ignoring the leading "v" the way Helm stores chart versions.
func trainerVersionMatches(detected string) bool {
	return strings.TrimPrefix(detected, "v") == strings.TrimPrefix(kubeflowTrainerVersion, "v")
}

// checkLogProfiles returns true if at least one LogProfile resource exists.
func checkLogProfiles(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(nvcreAPIGroup + "/v1alpha1")
	list.SetKind("LogProfileList")
	if err := c.List(ctx, list); err != nil {
		return false
	}
	return len(list.Items) > 0
}

// checkGPUOperator returns true if any node has the nvidia.com/gpu.present=true
// label, which is set by NVIDIA GPU Feature Discovery (part of GPU Operator).
func checkGPUOperator(ctx context.Context, c client.Client) bool {
	nodeList := &unstructured.UnstructuredList{}
	nodeList.SetAPIVersion("v1")
	nodeList.SetKind("NodeList")
	if err := c.List(ctx, nodeList, client.MatchingLabels{
		"nvidia.com/gpu.present": "true",
	}); err != nil {
		return false
	}
	return len(nodeList.Items) > 0
}

// checkDCGM returns nil if the GPU Operator created the standalone DCGM
// service. The operator omits it when spec.dcgm.enabled is false, which is the
// default when dcgmExporter runs its own embedded DCGM. The caller reads the
// error to tell a missing service apart from a lookup it could not complete.
func checkDCGM(ctx context.Context, c client.Client) error {
	svc := &unstructured.Unstructured{}
	svc.SetAPIVersion("v1")
	svc.SetKind("Service")
	key := client.ObjectKey{Name: dcgmServiceName, Namespace: dcgmServiceNamespace}
	return c.Get(ctx, key, svc)
}

// trainerVersionMismatch reports whether a Trainer version was detected and
// does not match the supported pin.
func (s *SetupStatus) trainerVersionMismatch() bool {
	return s.KubeflowTrainerVersion != nil &&
		s.KubeflowTrainerVersion.Detected != "" &&
		!trainerVersionMatches(s.KubeflowTrainerVersion.Detected)
}

// trainerStatusCell renders the Kubeflow Trainer table cell, folding the
// version verdict into the installed/not-found wording of the other rows.
func trainerStatusCell(s *SetupStatus) string {
	switch {
	case s.KubeflowTrainerVersion == nil:
		return "✗ not found"
	case s.KubeflowTrainerVersion.Detected == "":
		return "✓ installed (version unknown)"
	case s.trainerVersionMismatch():
		return fmt.Sprintf("✗ version %s (supported %s)",
			s.KubeflowTrainerVersion.Detected, s.KubeflowTrainerVersion.Supported)
	default:
		return "✓ installed (" + s.KubeflowTrainerVersion.Supported + ")"
	}
}

// printSetupStatus renders a human-readable table.
func printSetupStatus(out io.Writer, s *SetupStatus) {
	check := func(ok bool) string {
		if ok {
			return "✓"
		}
		return "✗"
	}
	status := func(ok bool) string {
		if ok {
			return "installed"
		}
		return "not found"
	}

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "Component\tStatus")
	_, _ = fmt.Fprintln(w, "─────────────────────────\t──────────")
	_, _ = fmt.Fprintf(w, "NVCRE CRDs\t%s %s\n", check(s.Components.NVCRECRDs), status(s.Components.NVCRECRDs))
	_, _ = fmt.Fprintf(w, "NVCRE Controller\t%s %s\n",
		check(s.Components.NVCREController), status(s.Components.NVCREController))
	_, _ = fmt.Fprintf(w, "Kubeflow Trainer\t%s\n", trainerStatusCell(s))
	_, _ = fmt.Fprintf(w, "Log Profiles\t%s %s\n", check(s.Components.LogProfiles), status(s.Components.LogProfiles))
	_, _ = fmt.Fprintf(w, "GPU Operator\t%s %s\n", check(s.Components.GPUOperator), status(s.Components.GPUOperator))
	_, _ = fmt.Fprintf(w, "DCGM (optional)\t%s %s\n", check(s.Components.DCGM), status(s.Components.DCGM))
	for _, rel := range s.HelmReleases {
		_, _ = fmt.Fprintf(w, "Helm release %s\t%s %s\n",
			rel.Name, check(!helmStateBlocksReady(rel.State)), rel.State)
	}
	_ = w.Flush()

	_, _ = fmt.Fprintln(out)
	unhealthy := unhealthyHelmReleases(s.HelmReleases)
	switch {
	case s.Installed:
		_, _ = fmt.Fprintln(out, "Status: ready")
	case !s.Components.allRequired():
		_, _ = fmt.Fprintln(out, "Status: not ready — run 'nvcrectl setup init' to install missing components")
		if !s.Components.GPUOperator {
			_, _ = fmt.Fprintln(out, "  GPU Operator must be installed by your cluster administrator")
		}
		if s.trainerVersionMismatch() {
			_, _ = fmt.Fprintf(out,
				"  Kubeflow Trainer %s is installed but this NVCRE build supports %s — "+
					"run 'nvcrectl setup init' to install the supported version\n",
				s.KubeflowTrainerVersion.Detected, s.KubeflowTrainerVersion.Supported)
		}
	default:
		_, _ = fmt.Fprintln(out, "Status: not ready — a managed Helm release is unhealthy")
	}
	for _, rel := range unhealthy {
		_, _ = fmt.Fprintf(out,
			"  Helm release %s (namespace: %s) is %s — run 'nvcrectl setup init' to repair it\n",
			rel.Name, rel.Namespace, rel.State)
	}

	if s.KubeflowTrainerVersion != nil && s.KubeflowTrainerVersion.Detected == "" {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Warning: could not determine the installed Kubeflow Trainer version.")
		_, _ = fmt.Fprintf(out, "         This NVCRE build supports Kubeflow Trainer %s.\n",
			s.KubeflowTrainerVersion.Supported)
	}

	if s.dcgmAbsent {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintf(out, "Note: service %s/%s is missing. Only the diagnostics/dcgm-level4\n",
			dcgmServiceNamespace, dcgmServiceName)
		_, _ = fmt.Fprintln(out, "      category needs it. Your cluster administrator can add it with:")
		_, _ = fmt.Fprintln(out, `      kubectl patch clusterpolicy cluster-policy --type=merge \`)
		_, _ = fmt.Fprintln(out, `        -p '{"spec":{"dcgm":{"enabled":true}}}'`)
	}
}
