// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const outputJSON = "json"

// SetupStatus is the JSON output structure for ncrectl setup status.
type SetupStatus struct {
	// Installed is true when all required components are present and ready.
	Installed  bool                  `json:"installed"`
	Components SetupStatusComponents `json:"components"`
}

// SetupStatusComponents reports the status of each individual component.
type SetupStatusComponents struct {
	CRECRDs         bool `json:"creCRDs"`
	CREController   bool `json:"creController"`
	KubeflowTrainer bool `json:"kubeflowTrainer"`
	LogProfiles     bool `json:"logProfiles"`
	GPUOperator     bool `json:"gpuOperator"`
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

'installed' is true only when all components are present.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			c, err := newSetupClient(configFlags)
			if err != nil {
				return fmt.Errorf("connect to cluster: %w", err)
			}

			status := collectSetupStatus(ctx, c)

			switch output {
			case outputJSON:
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(status)
			default:
				printSetupStatus(status)
				return nil
			}
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "table", "Output format: table, json")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// collectSetupStatus queries the cluster and returns the current component status.
func collectSetupStatus(ctx context.Context, c client.Client) *SetupStatus {
	comp := SetupStatusComponents{}

	comp.CRECRDs = checkCRECRDs(ctx, c)
	comp.CREController = checkCREController(ctx, c)
	comp.KubeflowTrainer = checkKubeflowTrainer(ctx, c)
	comp.LogProfiles = checkLogProfiles(ctx, c)
	comp.GPUOperator = checkGPUOperator(ctx, c)

	return &SetupStatus{
		Installed: comp.CRECRDs &&
			comp.CREController &&
			comp.KubeflowTrainer &&
			comp.LogProfiles &&
			comp.GPUOperator,
		Components: comp,
	}
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

// printSetupStatus renders a human-readable table.
func printSetupStatus(s *SetupStatus) {
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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "Component\tStatus")
	_, _ = fmt.Fprintln(w, "─────────────────────────\t──────────")
	_, _ = fmt.Fprintf(w, "CRE CRDs\t%s %s\n", check(s.Components.CRECRDs), status(s.Components.CRECRDs))
	_, _ = fmt.Fprintf(w, "CRE Controller\t%s %s\n",
		check(s.Components.CREController), status(s.Components.CREController))
	_, _ = fmt.Fprintf(w, "Kubeflow Trainer\t%s %s\n",
		check(s.Components.KubeflowTrainer), status(s.Components.KubeflowTrainer))
	_, _ = fmt.Fprintf(w, "Log Profiles\t%s %s\n", check(s.Components.LogProfiles), status(s.Components.LogProfiles))
	_, _ = fmt.Fprintf(w, "GPU Operator\t%s %s\n", check(s.Components.GPUOperator), status(s.Components.GPUOperator))
	_ = w.Flush()

	fmt.Println()
	if s.Installed {
		fmt.Println("Status: ready")
	} else {
		fmt.Println("Status: not ready — run 'ncrectl setup init' to install missing components")
		if !s.Components.GPUOperator {
			fmt.Println("  GPU Operator must be installed by your cluster administrator")
		}
	}
}
