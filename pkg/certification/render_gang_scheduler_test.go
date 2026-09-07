// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	_ "github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// gangQueueLabelKey is the label KAI Scheduler reads to place a workload in a
// queue. It is spelled out here rather than imported so that a rename in
// pkg/platform shows up as a golden-file diff instead of silently following.
const gangQueueLabelKey = "kai.scheduler/queue"

// gangReplicatedJob records what gang scheduling did to one replicatedJob of
// one TrainingRuntime dependency.
type gangReplicatedJob struct {
	Dependency    string `json:"dependency"`
	ReplicatedJob string `json:"replicatedJob"`
	// SchedulerName is replicatedJobs[].template.spec.template.spec.schedulerName.
	// Empty means the catalog left it unset and nothing wrote it.
	SchedulerName string `json:"schedulerName"`
	// QueueLabel is the kai.scheduler/queue value on
	// replicatedJobs[].template.metadata.labels, empty when the label is absent.
	QueueLabel string `json:"queueLabel"`
	// TemplateLabels is every label on that same metadata, sorted. It is here so
	// a case can tell "queue label added" apart from "labels replaced by the
	// queue label", which is what would happen if the helper assigned a fresh
	// map over an existing one.
	TemplateLabels []string `json:"templateLabels"`
}

// gangWorkflow is the per-Workflow projection written to the golden file.
type gangWorkflow struct {
	Workflow string `json:"workflow"`
	// DependencyKinds lists every dependency in order, so a case covering a
	// category with a PVC or ConfigMap shows that the non-TrainingRuntime
	// dependencies are still there and still parse.
	DependencyKinds []string            `json:"dependencyKinds"`
	ReplicatedJobs  []gangReplicatedJob `json:"replicatedJobs"`
}

// TestCertificationRenderGangScheduler covers the ordering that makes
// spec.gangScheduler work: nvcrectl renders the catalog, resolves catalog and
// platform overrides, and only then opts the pods into the gang scheduler. Run
// the other way around, a platform override would put schedulerName back, and
// the training entries hardcode schedulerName: default-scheduler, so the field
// would silently do nothing for exactly the workloads that need it most.
//
// The cases drive resolveWorkflowsOffline, which is the same function
// runCertificationRender calls, so what they record is what
// "nvcrectl certification render --platform aws" prints and what the
// certification controller creates.
func TestCertificationRenderGangScheduler(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "certification-render-gang-scheduler",
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
		if err := os.WriteFile(certPath, []byte(tc.Inputs["input_certification.yaml"]), 0o644); err != nil {
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
		if err := resolveWorkflowsOffline(cert, workflows, cfg.Platform); err != nil {
			return err
		}

		result := make([]gangWorkflow, 0, len(workflows))
		for i := range workflows {
			projected, projectErr := projectGangScheduling(&workflows[i])
			if projectErr != nil {
				return projectErr
			}
			result = append(result, projected)
		}

		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// projectGangScheduling walks a resolved Workflow in declaration order:
// dependencies as the catalog lists them, then replicatedJobs as the runtime
// lists them. Nothing is sorted except the label lists, so the output is stable
// without hiding a reordering.
func projectGangScheduling(wf *nvcrev1alpha1.Workflow) (gangWorkflow, error) {
	out := gangWorkflow{
		Workflow:        wf.Name,
		DependencyKinds: []string{},
		ReplicatedJobs:  []gangReplicatedJob{},
	}

	for i := range wf.Spec.Dependencies {
		raw := wf.Spec.Dependencies[i].Raw
		if len(raw) == 0 {
			out.DependencyKinds = append(out.DependencyKinds, "")
			continue
		}

		var typeMeta struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &typeMeta); err != nil {
			return out, err
		}
		out.DependencyKinds = append(out.DependencyKinds, typeMeta.Kind)
		if typeMeta.Kind != "TrainingRuntime" {
			continue
		}

		var rt trainerv1alpha1.TrainingRuntime
		if err := json.Unmarshal(raw, &rt); err != nil {
			return out, err
		}
		for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
			out.ReplicatedJobs = append(out.ReplicatedJobs, gangReplicatedJob{
				Dependency:     rt.Name,
				ReplicatedJob:  rj.Name,
				SchedulerName:  rj.Template.Spec.Template.Spec.SchedulerName,
				QueueLabel:     rj.Template.Labels[gangQueueLabelKey],
				TemplateLabels: sortedLabels(rj.Template.Labels),
			})
		}
	}
	return out, nil
}

// sortedLabels renders a label map as sorted "key=value" strings.
func sortedLabels(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for k, v := range labels {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}
