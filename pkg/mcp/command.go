// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcp wires the read-only MCP server into the nvcrectl command tree.
package mcp

import (
	"github.com/spf13/cobra"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/mcpserver"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/render"
)

// NewCommand returns the "mcp" cobra command.
func NewCommand(version string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol (MCP) server exposing read-only certification state",
	}
	cmd.AddCommand(newServeCommand(version))
	return cmd
}

// newServeCommand serves the MCP server over stdio. The flags mirror every
// other cluster-connecting subcommand, and so do the credentials: client-go's
// standard loading rules (explicit flags, KUBECONFIG env, then ~/.kube/config,
// falling back to the in-cluster ServiceAccount when the process runs in a pod
// with no kubeconfig). The server holds no credentials of its own and adds no
// privilege — it is exactly as authorized as the identity it resolves.
func newServeCommand(version string) *cobra.Command {
	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil // the namespace is a per-tool argument

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the read-only NVCRE MCP server over stdio",
		Long: `Expose NVCRE certification state to MCP agents over stdio.

Four read-only tools are available:
  - list_categories          the certification catalog (domain/variant)
  - get_certification_status overall result, conditions, and per-category state
  - get_certification_report the full report nvcrectl certification report prints
  - list_failed_nodes        failed nodes with per-node reason and message

The server is strictly read-only: no tool creates, mutates, or deletes a
resource, and nothing triggers a run. It holds no credentials of its own and
grants no privilege of its own — it acts as whatever identity client-go
resolves, which is the launching user's kubeconfig, or the pod ServiceAccount
if it is run in-cluster without one.

Most MCP clients spawn this command themselves, e.g.:

  "nvcre": {
    "command": "nvcrectl",
    "args": ["mcp", "serve"]
  }`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := render.NewK8sClient(configFlags)
			if err != nil {
				return err
			}
			return mcpserver.Run(cmd.Context(), &mcpserver.Store{Client: c}, version)
		},
	}
	configFlags.AddFlags(cmd.Flags())
	return cmd
}
