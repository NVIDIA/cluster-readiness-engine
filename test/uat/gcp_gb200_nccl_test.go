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

// TestGCPGB200NCCL tests the full certification lifecycle for GCP GB200 NCCL all-reduce.
//
// GCP GB200 uses RoCE (rdma-* networks) + ComputeDomain + DRA. Key differences from GCP H100:
//   - a4x-highgpu-4g (not a3-highgpu-8g), arm64
//   - 4 GPUs per node (not 8)
//   - NCCL_MNNVL_ENABLE=1, NCCL_NET_GDR_LEVEL=5
//   - RoCE runtime patch (rdma NICs, not TCPXO/FastRak)
//   - ComputeDomain dependency for topology-aware networking
func TestGCPGB200NCCL(t *testing.T) {
	const (
		certName = "gcp-gb200-nccl"
		nodesDir = "testdata/gcp/gb200"
		dataDir  = "testdata/gcp/gb200/nccl"
	)

	feature := features.New("gcp/gb200/nccl-all-reduce").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			// Restart controller to clear informer cache from any prior test.
			util.RestartController(ctx, t, c)

			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})

			util.RunNcrectl(ctx, t,
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
