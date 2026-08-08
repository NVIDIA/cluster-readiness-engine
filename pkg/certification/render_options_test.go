// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
)

// certification render is the check-before-you-apply tool, so what it shows has
// to match what the controller will create. Five CategoryOptions fields were
// missing from the CLI's catalog.BuildConfig while
// certification_controller.go passed all of them, so the preview quietly showed
// catalog defaults: timeoutPerJob 45m previewed as 1h, repeatCount 2 as
// iterations 1. A plausible wrong number is worse than a missing one.
func TestRenderKeepsOrchestrationOptions(t *testing.T) {
	cert := &burninv1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "opts-test"},
		Spec: burninv1alpha1.CertificationSpec{
			Target: burninv1alpha1.TargetSpec{
				NodeSelector: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
				},
			},
			CategoryOptions: burninv1alpha1.CategoryOptions{
				NodesPerJob:   new(int32(4)),
				MaxConcurrent: new(int32(3)),
				TimeoutPerJob: "45m",
				RepeatCount:   new(int32(2)),
			},
			Categories: []burninv1alpha1.CertificateCategory{
				{Domain: "communication", Variant: "nccl-all-reduce"},
			},
		},
	}

	workflows, err := renderCertification(cert, "aws")
	require.NoError(t, err)
	require.Len(t, workflows, 1)

	out, err := yaml.Marshal(workflows[0].Spec.Orchestration)
	require.NoError(t, err)
	got := string(out)

	require.Contains(t, got, "timeoutPerJob: 45m0s",
		"the user's timeout must survive, not the catalog default of 1h")
	require.NotContains(t, got, "1h0m0s")
	require.Contains(t, got, "maxConcurrent: 3")
	require.Equal(t, 2, workflows[0].Spec.Orchestration.Iterations,
		"repeatCount 2 must render as iterations 2, not the default 1")
}

// minGroupSize only reaches the rendered output in diagnose mode.
func TestRenderKeepsMinGroupSizeForDiagnose(t *testing.T) {
	cert := &burninv1alpha1.Certification{
		ObjectMeta: metav1.ObjectMeta{Name: "diag-test"},
		Spec: burninv1alpha1.CertificationSpec{
			Target: burninv1alpha1.TargetSpec{
				NodeSelector: map[string]string{
					"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
				},
			},
			CategoryOptions: burninv1alpha1.CategoryOptions{
				NodesPerJob:  new(int32(4)),
				TestScale:    "diagnose",
				MinGroupSize: new(int32(3)),
			},
			Categories: []burninv1alpha1.CertificateCategory{
				{Domain: "communication", Variant: "nccl-all-reduce"},
			},
		},
	}

	workflows, err := renderCertification(cert, "aws")
	require.NoError(t, err)
	require.Len(t, workflows, 1)

	out, err := yaml.Marshal(workflows[0].Spec)
	require.NoError(t, err)
	require.True(t, strings.Contains(string(out), "minGroupSize: 3"),
		"minGroupSize must reach the diagnose block, got:\n%s", string(out))
}
