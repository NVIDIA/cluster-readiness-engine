// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat

package uat

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/NVIDIA/cluster-readiness-engine/test/uat/util"
)

// TestAWSGB200WorkloadRunNemotron5 runs a Nemotron 5 (56B) training WorkloadRun
// on simulated AWS GB200 nodes using the exec framework with Megatron-LM.
func TestAWSGB200WorkloadRunNemotron5(t *testing.T) {
	const (
		runName  = "aws-gb200-workloadrun"
		nodesDir = "testdata/aws/gb200"
		dataDir  = "testdata/aws/gb200/workloadrun"
	)

	feature := features.New("aws/gb200/workloadrun-nemotron5").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			util.RestartController(ctx, t, c)

			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})

			util.RunNcrectlWorkloadRun(ctx, t, dataDir+"/input_workloadrun.yaml")
			t.Cleanup(func() {
				util.DeleteWorkloadRun(context.Background(), c, runName, "default")
			})

			return context.WithValue(ctx, nsKey, "default")
		}).
		Assess("WorkloadRun creates Workflow", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			run := util.WaitForWorkloadRun(ctx, t, c,
				util.CertificationKey(runName, ns),
				"InProgress", util.PollTimeout)

			require.NotNil(t, run.Status.WorkflowRef,
				"WorkloadRun should have a workflowRef")
			require.Equal(t, runName, run.Status.WorkflowRef.Name,
				"Workflow name should match WorkloadRun name")

			return ctx
		}).
		Assess("WorkloadRun succeeds", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			run := util.WaitForWorkloadRun(ctx, t, c,
				util.CertificationKey(runName, ns),
				"Succeeded", util.PollTimeout)

			require.Equal(t, "aws", run.Status.DetectedPlatform)
			require.Equal(t, "gb200", run.Status.DetectedGPUArchitecture)

			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}
