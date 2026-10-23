// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestApplyWRNICDetection drives the NIC detection runWorkloadRunRenderDryRun
// runs against the discovered nodes before the job template is built. The
// cases pin both sides of the networkAttachments skip on the same on-prem
// HGX B300 node set, which advertises a single candidate: without a list the
// candidate is injected as nicResourceName, and with a list nothing is, since
// the list owns the NIC wiring (issue #400).
func TestApplyWRNICDetection(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "apply-wr-nic-detection",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Platform        string                    `json:"platform"`
			GPUArchitecture string                    `json:"gpuArchitecture"`
			MlnxPerNode     int32                     `json:"mlnxPerNode"`
			Nodes           []corev1.Node             `json:"nodes"`
			Run             nvcrev1alpha1.WorkloadRun `json:"run"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		applyWRNICDetection(&in.Run, in.Platform, in.GPUArchitecture, in.Nodes, in.MlnxPerNode)
		b, err := json.MarshalIndent(struct {
			NicResourceName *string `json:"nicResourceName"`
		}{in.Run.Spec.NicResourceName}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
