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

// TestNScaleB200NCCL tests the full certification lifecycle for Nscale NKS
// B200 NCCL all-reduce.
//
// Nscale runs no device plugin/GFD DaemonSet: nodes carry no
// nvidia.com/gpu.product label, and GPUs/RDMA are DRA resource claims
// (gpu.nvidia.com/rdma.nscale.com) rather than extended resources (ADR-085).
func TestNScaleB200NCCL(t *testing.T) {
	const (
		certName = "nscale-b200-nccl"
		nodesDir = "testdata/nscale/b200"
		dataDir  = "testdata/nscale/b200/nccl"
	)

	feature := features.New("nscale/b200/nccl-all-reduce").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			util.RestartController(ctx, t, c)

			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})

			util.RunNvcrectl(ctx, t,
				"--category", "communication/nccl-all-reduce",
				"--name", certName,
				"--namespace", "default",
				"--nodes-per-job", "2",
			)
			t.Cleanup(func() {
				util.DeleteCertification(context.Background(), c, certName, "default")
			})

			return context.WithValue(ctx, nsKey, "default")
		}).
		Assess("Pods match expected spec", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			util.WaitForCertification(ctx, t, c,
				util.CertificationKey(certName, ns),
				"InProgress", util.PollTimeout)

			pods := util.WaitForPods(ctx, t, c, ns, 3, util.PollTimeout)
			util.ComparePods(t, dataDir+"/expected_pods.yaml", pods)

			return ctx
		}).
		Assess("Certification succeeds", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			util.WaitForCertification(ctx, t, c,
				util.CertificationKey(certName, ns),
				"Succeeded", util.PollTimeout)

			util.CompareCertification(ctx, t, c,
				dataDir+"/expected_certification.yaml",
				util.CertificationKey(certName, ns))

			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}

// TestNScaleGB300NCCL tests the full certification lifecycle for Nscale NKS
// GB300 NCCL all-reduce, which additionally matches the arch-only
// GB200/GB300 ComputeDomain block on top of the nscale-specific one.
func TestNScaleGB300NCCL(t *testing.T) {
	const (
		certName = "nscale-gb300-nccl"
		nodesDir = "testdata/nscale/gb300"
		dataDir  = "testdata/nscale/gb300/nccl"
	)

	feature := features.New("nscale/gb300/nccl-all-reduce").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			util.RestartController(ctx, t, c)

			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})

			util.RunNvcrectl(ctx, t,
				"--category", "communication/nccl-all-reduce",
				"--name", certName,
				"--namespace", "default",
				"--nodes-per-job", "2",
			)
			t.Cleanup(func() {
				util.DeleteCertification(context.Background(), c, certName, "default")
			})

			return context.WithValue(ctx, nsKey, "default")
		}).
		Assess("Pods match expected spec", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			util.WaitForCertification(ctx, t, c,
				util.CertificationKey(certName, ns),
				"InProgress", util.PollTimeout)

			pods := util.WaitForPods(ctx, t, c, ns, 3, util.PollTimeout)
			util.ComparePods(t, dataDir+"/expected_pods.yaml", pods)

			return ctx
		}).
		Assess("Certification succeeds", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			util.WaitForCertification(ctx, t, c,
				util.CertificationKey(certName, ns),
				"Succeeded", util.PollTimeout)

			util.CompareCertification(ctx, t, c,
				dataDir+"/expected_certification.yaml",
				util.CertificationKey(certName, ns))

			return ctx
		}).
		Feature()

	testenv.Test(t, feature)
}
