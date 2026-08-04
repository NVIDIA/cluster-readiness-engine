// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/spf13/cobra"

	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/certification"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/cluster"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/render"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/setup"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/upgrade"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workloadrun"
)

var version = "dev"

func main() {
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:          "ncrectl",
		Short:        "CLI for GPU cluster readiness and certification",
		Version:      version,
		SilenceUsage: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Name() == "upgrade" {
				return nil // skip the upgrade check for the upgrade command itself
			}
			return upgrade.EnforceUpgrade(version, os.Stderr)
		},
	}
	root.AddCommand(
		certification.NewCommand(version),
		cluster.NewCommand(),
		render.NewWorkflowCommand(),
		setup.NewCommand(version),
		upgrade.NewCommand(version),
		workloadrun.NewCommand(),
	)
	return root
}
