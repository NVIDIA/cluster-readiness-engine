// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/kubeconfig"
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

// SetupStatus is the JSON output structure for ncrectl setup status.
type SetupStatus struct {
	// Installed is true when all required components are present and ready
	// and no managed Helm release is in a failed or pending state.
	Installed  bool                  `json:"installed"`
	Components SetupStatusComponents `json:"components"`
	// HelmReleases reports the state of the Helm releases setup init manages.
	HelmReleases []HelmReleaseStatus `json:"helmReleases"`

	// dcgmAbsent is true only when the API server answered that the service
	// does not exist. A denied or failed lookup leaves it false, so the
	// command does not tell the user to enable a service that may be there.
	dcgmAbsent bool
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
	CRECRDs         bool `json:"creCRDs"`
	CREController   bool `json:"creController"`
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
	return c.CRECRDs &&
		c.CREController &&
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
		Short: "Report the installation status of CRE and its dependencies",
		Long: `Check whether CRE and all required cluster components are installed and ready.

Components checked:
  creCRDs        CRE CustomResourceDefinitions (cre.nvidia.com)
  creController  CRE controller deployment (namespace: cluster-readiness-engine)
  kubeflowTrainer      Kubeflow Trainer TrainJob CRD (kubeflow.org)
  logProfiles          CRE LogProfile resources
  gpuOperator          NVIDIA GPU Operator (nodes with nvidia.com/gpu.present=true)
  dcgm                 NVIDIA DCGM service (optional; diagnostics/dcgm-level4 only)

The Helm releases managed by 'setup init' (cluster-readiness-engine and
kubeflow-trainer) are also checked via the helm CLI.

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
			status := collectSetupStatus(ctx, c, query)

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
func collectSetupStatus(ctx context.Context, c client.Client, query helmStateFunc) *SetupStatus {
	comp := SetupStatusComponents{}

	comp.CRECRDs = checkCRECRDs(ctx, c)
	comp.CREController = checkCREController(ctx, c)
	comp.KubeflowTrainer = checkKubeflowTrainer(ctx, c)
	comp.LogProfiles = checkLogProfiles(ctx, c)
	comp.GPUOperator = checkGPUOperator(ctx, c)

	dcgmErr := checkDCGM(ctx, c)
	comp.DCGM = dcgmErr == nil

	releases := checkHelmReleases(query)

	return &SetupStatus{
		Installed:    comp.allRequired() && len(unhealthyHelmReleases(releases)) == 0,
		Components:   comp,
		HelmReleases: releases,
		dcgmAbsent:   apierrors.IsNotFound(dcgmErr),
	}
}

// checkHelmReleases queries the state of every Helm release setup init manages.
func checkHelmReleases(query helmStateFunc) []HelmReleaseStatus {
	managed := []struct{ name, namespace string }{
		{helmReleaseName, creNamespace},
		{trainerReleaseName, trainerNamespace},
	}
	releases := make([]HelmReleaseStatus, 0, len(managed))
	for _, rel := range managed {
		releases = append(releases, HelmReleaseStatus{
			Name:      rel.name,
			Namespace: rel.namespace,
			State:     query(rel.name, rel.namespace),
		})
	}
	return releases
}

// helmStateBlocksReady reports whether a release state must block readiness.
// Failed and pending states block. Deployed does not. Absent and unknown
// states do not block either: CRE may have been installed without Helm, and
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

// checkCRECRDs returns true if CRE CRDs are installed.
func checkCRECRDs(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return false
	}
	for _, item := range list.Items {
		group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
		if group == creAPIGroup {
			return true
		}
	}
	return false
}

// checkCREController returns true if the CRE controller deployment
// has at least one available replica in the cluster-readiness-engine namespace.
func checkCREController(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apps/v1")
	list.SetKind("DeploymentList")
	if err := c.List(ctx, list, client.InNamespace(creNamespace)); err != nil {
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

// checkKubeflowTrainer returns true if the Kubeflow TrainJob CRD is installed.
func checkKubeflowTrainer(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("apiextensions.k8s.io/v1")
	list.SetKind("CustomResourceDefinitionList")
	if err := c.List(ctx, list); err != nil {
		return false
	}
	for _, item := range list.Items {
		group, _, _ := unstructured.NestedString(item.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(item.Object, "spec", "names", "kind")
		if group == trainerAPIGroup && kind == "TrainJob" {
			return true
		}
	}
	return false
}

// checkLogProfiles returns true if at least one LogProfile resource exists.
func checkLogProfiles(ctx context.Context, c client.Client) bool {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(creAPIGroup + "/v1alpha1")
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
	_, _ = fmt.Fprintf(w, "CRE CRDs\t%s %s\n", check(s.Components.CRECRDs), status(s.Components.CRECRDs))
	_, _ = fmt.Fprintf(w, "CRE Controller\t%s %s\n",
		check(s.Components.CREController), status(s.Components.CREController))
	_, _ = fmt.Fprintf(w, "Kubeflow Trainer\t%s %s\n",
		check(s.Components.KubeflowTrainer), status(s.Components.KubeflowTrainer))
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
		_, _ = fmt.Fprintln(out, "Status: not ready — run 'ncrectl setup init' to install missing components")
		if !s.Components.GPUOperator {
			_, _ = fmt.Fprintln(out, "  GPU Operator must be installed by your cluster administrator")
		}
	default:
		_, _ = fmt.Fprintln(out, "Status: not ready — a managed Helm release is unhealthy")
	}
	for _, rel := range unhealthy {
		_, _ = fmt.Fprintf(out,
			"  Helm release %s (namespace: %s) is %s — run 'ncrectl setup init' to repair it\n",
			rel.Name, rel.Namespace, rel.State)
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
