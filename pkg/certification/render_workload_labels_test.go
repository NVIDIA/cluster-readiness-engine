// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// workloadLabelWorkflow is the per-Workflow projection. Labels is the resolved
// spec.jobTemplate.spec.workloadMetadata.labels, the map that reaches the
// generated workload object. GangScheduler is the intent persisted on the
// Workflow, which is what lets a rendered Workflow submitted on its own
// enforce the same contract without its Certification.
type workloadLabelWorkflow struct {
	Workflow      string                           `json:"workflow"`
	Labels        map[string]string                `json:"labels"`
	GangScheduler *nvcrev1alpha1.GangSchedulerSpec `json:"gangScheduler"`
}

// TestCertificationRenderWorkloadLabels covers workload-object labels through
// the real catalog and the real render path. The cases drive
// resolveWorkflowsOffline, the same function runCertificationRender calls, so
// what they record is what "nvcrectl certification render" prints and — because
// both go through the one named transform stage — what the certification
// controller creates.
//
// Two properties matter here that the pkg/platform and pkg/workload cases
// cannot show, because both use hand-written specs. First, that the resolution
// is per category against real catalog entries, including a Certification
// whose categories want different queues. Second, that a Certification setting
// none of this renders with no workloadMetadata at all, which is the
// compatibility guarantee for every manifest written before ADR-079.
func TestCertificationRenderWorkloadLabels(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-workload-labels",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var cfg struct {
			Platform string `json:"platform"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &cfg); err != nil {
			return err
		}

		certPath := filepath.Join(tc.T.TempDir(), "certification.yaml")
		if err := os.WriteFile(certPath,
			[]byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
			return err
		}

		cert, err := readCertification(certPath)
		if err != nil {
			return err
		}
		workflows, err := renderCertification(cert, cfg.Platform)
		if err != nil {
			return err
		}

		// A conflict case fails here rather than producing Workflows, so the
		// error is the result and is recorded as such.
		if resolveErr := resolveWorkflowsOffline(cert, workflows, cfg.Platform); resolveErr != nil {
			tc.Actual = resolveErr.Error() + "\n"
			return nil
		}

		result := make([]workloadLabelWorkflow, 0, len(workflows))
		for i := range workflows {
			result = append(result, workloadLabelWorkflow{
				Workflow:      workflows[i].Name,
				Labels:        projectWorkloadMetadataLabels(&workflows[i]),
				GangScheduler: workflows[i].Spec.GangScheduler,
			})
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// projectWorkloadMetadataLabels reads the resolved workload-object labels as a
// plain map, returning nil when the field is absent so a golden distinguishes
// "no workloadMetadata" from "workloadMetadata with an empty map".
func projectWorkloadMetadataLabels(wf *nvcrev1alpha1.Workflow) map[string]string {
	md := wf.Spec.JobTemplate.Spec.WorkloadMetadata
	if md == nil || len(md.Labels) == 0 {
		return nil
	}
	labels := make(map[string]string, len(md.Labels))
	for key, value := range md.Labels {
		labels[key] = string(value)
	}
	return labels
}
