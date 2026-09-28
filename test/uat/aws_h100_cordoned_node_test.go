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

// This test ensures that a Certification can succeed against a cordoned node
// if there exists a taintSelectors entry for the node.kubernetes.io/unschedulable taint.
func TestAWSH100CordonedNode(t *testing.T) {
	const (
		certName = "aws-h100-cordoned-node"
		nodesDir = "testdata/aws/h100/cordoned-node"
		dataDir  = "testdata/aws/h100/cordoned-node"
	)

	feature := features.New("aws/h100/cordoned-node").
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)

			util.RestartController(ctx, t, c)

			util.ApplyYAML(ctx, t, c, nodesDir+"/nodes.yaml", "")
			t.Cleanup(func() {
				util.CleanupYAML(context.Background(), c, nodesDir+"/nodes.yaml", "")
			})

			util.RunNvcrectl(ctx, t,
				"--cert-file", "test/uat/"+nodesDir+"/certification.yaml",
				"--namespace", "default",
			)
			t.Cleanup(func() {
				util.DeleteCertification(context.Background(), c, certName, "default")
			})

			return context.WithValue(ctx, nsKey, "default")
		}).
		Assess("Pod is scheduled onto the cordoned node", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c, err := util.NewClient(cfg)
			require.NoError(t, err)
			ns := ctx.Value(nsKey).(string)

			util.WaitForCertification(ctx, t, c,
				util.CertificationKey(certName, ns),
				"InProgress", util.PollTimeout)

			pods := util.WaitForPods(ctx, t, c, ns, 1, util.PollTimeout)
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
