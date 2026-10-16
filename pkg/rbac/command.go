// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rbac

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// NewCommand returns the `nvcrectl rbac` command group.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rbac",
		Short: "Inspect the controller's per-namespace access",
	}
	cmd.AddCommand(newPrintCommand())
	return cmd
}

func newPrintCommand() *cobra.Command {
	var namespace string
	sa := DefaultServiceAccount()

	cmd := &cobra.Command{
		Use:   "print",
		Short: "Print the RoleBinding that grants the controller access to a run namespace",
		Long: `Writes the RoleBinding that grants the controller its write access in one
run namespace, for applying with kubectl or committing next to a Certification
in GitOps. 'nvcrectl certification run' and 'nvcrectl workloadrun run' create it
themselves; this is for runs that do not go through nvcrectl.

The binding is only needed when the chart is installed with
rbac.scope: namespaced. Without it, a Workflow in that namespace waits with
reason NamespacePermissionsMissing until the binding exists.

Nothing is read from the cluster. Pass --controller-namespace and
--service-account when the chart was installed under a release other than
'nvcre' in namespace 'nvcre'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return printRunBinding(cmd.OutOrStdout(), namespace, sa)
		},
	}
	cmd.Flags().StringVarP(&namespace, "namespace", "n", "", "Run namespace to grant (required)")
	cmd.Flags().StringVar(&sa.Namespace, "controller-namespace", DefaultControllerNamespace,
		"Namespace the controller is installed in")
	cmd.Flags().StringVar(&sa.Name, "service-account", DefaultServiceAccountName,
		"Controller ServiceAccount name")
	_ = cmd.MarkFlagRequired("namespace")
	return cmd
}

func printRunBinding(out io.Writer, namespace string, sa ServiceAccount) error {
	if namespace == "" {
		return fmt.Errorf("--namespace must not be empty")
	}
	if sa.Namespace == "" || sa.Name == "" {
		return fmt.Errorf("--controller-namespace and --service-account must not be empty")
	}
	manifest, err := RunBindingYAML(namespace, sa)
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, manifest)
	return err
}
