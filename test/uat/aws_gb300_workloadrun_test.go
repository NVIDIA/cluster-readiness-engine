// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat

package uat

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"github.com/NVIDIA/cluster-readiness-engine/test/uat/util"
)

// TestAWSGB300WorkloadRunMPI runs an MPI (nccl-tests style) WorkloadRun on
// simulated AWS GB300 nodes. AWS GB300 uses RoCE (no EFA), so the platform
// override table must pin OpenMPI's own transport to TCP on eth0, disable
// UCC/HCOLL, and forward the RoCE NCCL env via mpirun -x (issue #175,
// ADR-070).
func TestAWSGB300WorkloadRunMPI(t *testing.T) {
	const (
		runName  = "aws-gb300-workloadrun"
		nodesDir = "testdata/aws/gb300"
		dataDir  = "testdata/aws/gb300/workloadrun"
	)

	feature := features.New("aws/gb300/workloadrun-mpi").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			util.RestartController(ctx, t, c)

			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})

			util.RunNvcrectlWorkloadRun(ctx, t, dataDir+"/input_workloadrun.yaml")
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
		Assess("Launcher pins MPI transport to TCP with RoCE env", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			// 1 launcher + 2 workers for a 2-node MPI run.
			pods := util.WaitForPods(ctx, t, c, ns, 3, util.PollTimeout)

			var launcherArgs string
			for i := range pods {
				if !strings.Contains(pods[i].Name, "launcher") {
					continue
				}
				launcherArgs = joinContainerArgs(&pods[i])
			}
			require.NotEmpty(t, launcherArgs, "expected a launcher pod with args")

			// Spot checks for the AWS GB300 RoCE transport block: MPI
			// point-to-point pinned to TCP, and the NCCL RoCE GID index
			// forwarded to the workers via mpirun -x.
			require.Contains(t, launcherArgs, "--mca pml ob1",
				"launcher must pin OpenMPI transport to TCP (ob1)")
			require.Contains(t, launcherArgs, "-x NCCL_IB_GID_INDEX=3",
				"launcher must forward the RoCE GID index via mpirun -x")

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
			require.Equal(t, "gb300", run.Status.DetectedGPUArchitecture)

			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}

// joinContainerArgs flattens every container's command and args into one
// string so assertions can match mpirun flags regardless of how the
// controller wraps them. The WorkloadRun controller bakes the mpirun
// invocation into a bash -c script whose tokens are double-quoted
// (`"--mca" "pml" "ob1"`); stripping the quotes lets the same assertion
// also match unwrapped args.
func joinContainerArgs(pod *corev1.Pod) string {
	var parts []string
	for _, c := range pod.Spec.Containers {
		parts = append(parts, strings.Join(c.Command, " "), strings.Join(c.Args, " "))
	}
	return strings.ReplaceAll(strings.Join(parts, " "), `"`, "")
}
