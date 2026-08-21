// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/spf13/cobra"

	_ "github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/certification"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/cluster"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/render"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/setup"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/workloadrun"
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
	}
	root.AddCommand(
		certification.NewCommand(version),
		cluster.NewCommand(),
		render.NewWorkflowCommand(),
		setup.NewCommand(version),
		workloadrun.NewCommand(),
	)
	return root
}
