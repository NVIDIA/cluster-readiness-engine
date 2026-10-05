// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/spf13/cobra"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/certification"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/cluster"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/mcp"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/render"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/setup"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workloadrun"
)

var version = "dev"

func main() {
	// Shared controller code logs through controller-runtime, which discards
	// output and dumps a stack trace after 30s unless a logger is set.
	// zap writes to stderr, so stdout stays clean for rendered manifests.
	logf.SetLogger(zap.New(zap.ConsoleEncoder()))
	if err := newRootCommand().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:          "nvcrectl",
		Short:        "CLI for GPU cluster readiness and certification",
		Version:      version,
		SilenceUsage: true,
	}
	root.AddCommand(
		certification.NewCommand(version),
		cluster.NewCommand(),
		mcp.NewCommand(version),
		render.NewWorkflowCommand(),
		setup.NewCommand(version),
		workloadrun.NewCommand(),
	)
	return root
}
