// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
)

// BuildWorkflowSpec renders the Workflow that "nvcrectl workloadrun render" and
// "--dry-run" print. The controller does not call it. The controller has its
// own copy in pkg/controller/workloadrun_controller.go, and the two copies have
// already drifted apart, so these cases cover the preview only. A passing run
// here tells you nothing about what happens on a cluster.
//
// Each case records only the fields it is about, instead of the whole spec. The
// whole spec is about 1040 lines, and about 85% of it is the platform override
// block, which cmd/integration/testdata/reconcile/workloadrun-torch already
// records line for line. If these cases recorded it again, then every edit to
// pkg/platform/overrides/workloadrun.yaml or to the catalog _lib files it reads
// would fail all of them, even though none of those edits touch this package.
func TestBuildWorkflowSpec(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "build-workflow-spec",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// The tags below are json, not yaml. sigs.k8s.io/yaml converts the YAML
		// to JSON and then calls encoding/json, which ignores a yaml tag and
		// leaves an unmatched field at its zero value without reporting an
		// error. With yaml tags, a typo in a key would go unnoticed and would
		// write gpusPerNode: 0 into the expected file.
		var input struct {
			Run           crev1alpha1.WorkloadRun `json:"run"`
			GpusPerNode   int32                   `json:"gpusPerNode"`
			MlnxPerNode   int32                   `json:"mlnxPerNode"`
			EnableMNNVL   bool                    `json:"enableMNNVL"`
			FrameworkType string                  `json:"frameworkType"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		got := BuildWorkflowSpec(&input.Run, input.GpusPerNode, input.MlnxPerNode,
			input.EnableMNNVL, input.FrameworkType)

		b, err := json.MarshalIndent(project(got), "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// projection holds the parts of a rendered WorkflowSpec that these cases check.
type projection struct {
	Trainer         *trainerv1alpha1.Trainer `json:"trainer"`
	TimeoutPerJob   string                   `json:"timeoutPerJob"`
	Iterations      int                      `json:"iterations"`
	DependencyKinds []string                 `json:"dependencyKinds"`
	// Validation records the contents and not only whether the block is
	// present. The exact threshold key is part of it, because issue #52 is open
	// about one unknown key turning off every threshold check without saying so.
	Validation *crev1alpha1.ValidationSpec `json:"validation"`
	// WorkerEnv holds "NAME=value" for the worker container of the runtime
	// dependency, which is the container the workload runs in. It records values
	// and not only names, so a case can tell a variable the user asked for apart
	// from a default that has the same name.
	WorkerEnv []string `json:"workerEnv"`
	// WorkerVolumeMounts and RuntimeVolumes cover the two halves of an inline
	// config, which a person can remove one at a time.
	WorkerVolumeMounts []string `json:"workerVolumeMounts"`
	RuntimeVolumes     []string `json:"runtimeVolumes"`
	// OverrideCount is the number of overrides, not their contents. The count
	// is enough to catch an override that goes missing, and it does not change
	// when someone edits an override body.
	OverrideCount int `json:"overrideCount"`
	// GangSchedulerName is the schedulerName injected into pod specs by
	// pkg/platform when GangScheduler is set. Omitted when empty so that cases
	// without gang scheduling do not need to carry a blank field.
	GangSchedulerName string `json:"gangSchedulerName,omitempty"`
	// GangSchedulerQueue is the kai.scheduler/queue label injected into pod
	// template metadata by pkg/platform. Omitted when empty.
	GangSchedulerQueue string `json:"gangSchedulerQueue,omitempty"`
}

func project(s *crev1alpha1.WorkflowSpec) projection {
	out := projection{
		Iterations:    s.Orchestration.Iterations,
		Validation:    s.Validation,
		OverrideCount: len(s.Overrides),
	}
	if tj := s.JobTemplate.Spec.Workload.TrainJob; tj != nil {
		out.Trainer = tj.Trainer
	}
	if t := s.Orchestration.Execution.TimeoutPerJob; t != nil {
		out.TimeoutPerJob = t.Duration.String()
	}
	for i := range s.Dependencies {
		out.DependencyKinds = append(out.DependencyKinds, dependencyKind(&s.Dependencies[i]))
	}
	out.WorkerEnv, out.WorkerVolumeMounts, out.RuntimeVolumes, out.GangSchedulerName, out.GangSchedulerQueue = runtimeWorker(s)
	return out
}

// dependencyKind reads the Kind out of a dependency's embedded raw resource.
func dependencyKind(d *crev1alpha1.DependencySpec) string {
	var obj struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(d.Raw, &obj); err != nil {
		return ""
	}
	return obj.Kind
}

// runtimeWorker reads the "node" replicatedJob out of the runtime dependency
// and returns its env vars, volume mounts, pod volumes, schedulerName, and
// gang-scheduler queue label. It follows one named path rather than searching
// the document, because a search would also find containers the workload does
// not run in (e.g. an MPI launcher).
func runtimeWorker(s *crev1alpha1.WorkflowSpec) (env, mounts, volumes []string, schedulerName, queueLabel string) {
	if len(s.Dependencies) == 0 || len(s.Dependencies[0].Raw) == 0 {
		return nil, nil, nil, "", ""
	}
	var rt trainerv1alpha1.TrainingRuntime
	if err := json.Unmarshal(s.Dependencies[0].Raw, &rt); err != nil {
		return nil, nil, nil, "", ""
	}
	// Pick the job by name. An MPI runtime holds two jobs, "node" and
	// "launcher", and only "node" runs the worker processes.
	for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
		if rj.Name != "node" {
			continue
		}
		schedulerName = rj.Template.Spec.Template.Spec.SchedulerName
		queueLabel = rj.Template.Labels["kai.scheduler/queue"]
		pod := rj.Template.Spec.Template.Spec
		for _, v := range pod.Volumes {
			volumes = append(volumes, v.Name)
		}
		for _, c := range pod.Containers {
			if c.Name != "node" {
				continue
			}
			for _, e := range c.Env {
				env = append(env, e.Name+"="+e.Value)
			}
			for _, m := range c.VolumeMounts {
				mounts = append(mounts, m.Name+" at "+m.MountPath)
			}
		}
		return env, mounts, volumes, schedulerName, queueLabel
	}
	return nil, nil, nil, "", ""
}

// TestValidateExecFramework drives golden-file cases for validateExecFramework.
// Each case provides a WorkloadRunSpec in input.yaml and expects a JSON object
// with a single "error" key — null on success or the error message on failure.
func TestValidateExecFramework(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "validate-exec-framework",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var spec crev1alpha1.WorkloadRunSpec
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &spec); err != nil {
			return err
		}

		err := validateExecFramework(&spec, "test-wr")

		var result struct {
			Error any `json:"error"`
		}
		if err != nil {
			result.Error = err.Error()
		}

		b, marshalErr := json.MarshalIndent(result, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
