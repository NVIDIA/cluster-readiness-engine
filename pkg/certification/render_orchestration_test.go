// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// certification render is the check-before-you-apply tool, so what it prints
// has to match what the controller creates. Five CategoryOptions fields were
// missing from the CLI's catalog.BuildConfig while the controller passed all of
// them, and two were replaced by catalog defaults rather than omitted, so the
// operator read a plausible number that was not theirs.
func TestRenderOrchestrationOptions(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "render-orchestration-options",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var opts crev1alpha1.CategoryOptions
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &opts); err != nil {
			return err
		}
		var sel struct {
			Category *struct {
				Domain  string `yaml:"domain"`
				Variant string `yaml:"variant"`
			} `yaml:"category"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &sel); err != nil {
			return err
		}
		cat := crev1alpha1.CertificateCategory{Domain: "communication", Variant: "nccl-all-reduce"}
		if sel.Category != nil {
			cat = crev1alpha1.CertificateCategory{Domain: sel.Category.Domain, Variant: sel.Category.Variant}
		}

		cert := &crev1alpha1.Certification{
			ObjectMeta: metav1.ObjectMeta{Name: "render-test"},
			Spec: crev1alpha1.CertificationSpec{
				Target: crev1alpha1.TargetSpec{
					NodeSelector: map[string]string{
						"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
					},
				},
				CategoryOptions: opts,
				Categories:      []crev1alpha1.CertificateCategory{cat},
			},
		}

		workflows, err := renderCertification(cert, "aws")
		if err != nil {
			return err
		}

		wf := workflows[0]
		out := struct {
			Orchestration crev1alpha1.OrchestrationSpec `json:"orchestration"`
			NumNodes      *int32                        `json:"numNodes,omitempty"`
			MaxRestarts   int32                         `json:"maxRestarts,omitempty"`
			Measurement   string                        `json:"measurementTimeout,omitempty"`
		}{Orchestration: wf.Spec.Orchestration}

		if tj := wf.Spec.JobTemplate.Spec.Workload.TrainJob; tj != nil && tj.Trainer != nil {
			out.NumNodes = tj.Trainer.NumNodes
		}
		if ck := wf.Spec.JobTemplate.Spec.Checkpoint; ck != nil {
			if ck.MaxRestarts != nil {
				out.MaxRestarts = *ck.MaxRestarts
			}
		}
		if v := wf.Spec.Validation; v != nil && v.Performance != nil && v.Performance.MeasurementTimeout != nil {
			out.Measurement = v.Performance.MeasurementTimeout.Duration.String()
		}

		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
