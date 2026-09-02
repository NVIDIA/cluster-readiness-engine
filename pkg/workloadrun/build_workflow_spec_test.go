// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// BuildWorkflowSpec renders the Workflow that "nvcrectl workloadrun render" and
// "--dry-run" print. The controller does not call it. The controller has its
// own copy in pkg/controller/workloadrun_controller.go (both now share
// controller.NodesPerJobForScale for scale-based sizing, issue #85), so these
// cases cover the preview only. On-cluster behaviour is covered by
// cmd/integration/testdata/reconcile/workloadrun-*.
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
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		// The tags below are json, not yaml. sigs.k8s.io/yaml converts the YAML
		// to JSON and then calls encoding/json, which ignores a yaml tag and
		// leaves an unmatched field at its zero value without reporting an
		// error. With yaml tags, a typo in a key would go unnoticed and would
		// write gpusPerNode: 0 into the expected file.
		var input struct {
			Run           nvcrev1alpha1.WorkloadRun `json:"run"`
			GpusPerNode   int32                     `json:"gpusPerNode"`
			MlnxPerNode   int32                     `json:"mlnxPerNode"`
			EnableMNNVL   bool                      `json:"enableMNNVL"`
			FrameworkType string                    `json:"frameworkType"`
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

// nodeJobName is the name of both the worker replicatedJob and the workload
// container inside a rendered TrainingRuntime. An MPI runtime also holds a
// "launcher" replicatedJob, whose container is likewise named "node".
const nodeJobName = "node"

// projection holds the parts of a rendered WorkflowSpec that these cases check.
type projection struct {
	Trainer         *trainerv1alpha1.Trainer `json:"trainer"`
	TimeoutPerJob   string                   `json:"timeoutPerJob"`
	Iterations      int                      `json:"iterations"`
	DependencyKinds []string                 `json:"dependencyKinds"`
	// Validation records the contents and not only whether the block is
	// present. The exact threshold key is part of it: since issue #52 was
	// fixed, a key must be a threshold-registry key — readWorkloadRun rejects
	// unknown keys and the Job controller fails validation on them — so the
	// exact string decides whether a run is accepted at all.
	Validation *nvcrev1alpha1.ValidationSpec `json:"validation"`
	// WorkerEnv holds "NAME=value" for the worker container of the runtime
	// dependency, which is the container the workload runs in. It records values
	// and not only names, so a case can tell a variable the user asked for apart
	// from a default that has the same name.
	WorkerEnv []string `json:"workerEnv"`
	// LauncherEnv holds the same for the launcher container. Only MPI runtimes
	// render a launcher, so torch and exec cases omit the field. It exists
	// because the fix for issue #68 emits env on both MPI containers, and the
	// worker projection alone cannot see the launcher half regressing.
	LauncherEnv []string `json:"launcherEnv,omitempty"`
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

func project(s *nvcrev1alpha1.WorkflowSpec) projection {
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
	out.LauncherEnv = runtimeLauncherEnv(s)
	return out
}

// dependencyKind reads the Kind out of a dependency's embedded raw resource.
func dependencyKind(d *nvcrev1alpha1.DependencySpec) string {
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
func runtimeWorker(s *nvcrev1alpha1.WorkflowSpec) (env, mounts, volumes []string, schedulerName, queueLabel string) {
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
		if rj.Name != nodeJobName {
			continue
		}
		schedulerName = rj.Template.Spec.Template.Spec.SchedulerName
		queueLabel = rj.Template.Labels["kai.scheduler/queue"]
		pod := rj.Template.Spec.Template.Spec
		for _, v := range pod.Volumes {
			volumes = append(volumes, v.Name)
		}
		for _, c := range pod.Containers {
			if c.Name != nodeJobName {
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

// runtimeLauncherEnv reads the env vars of the "node" container inside the
// "launcher" replicatedJob of the runtime dependency. Only MPI runtimes render
// a launcher, so the result is nil for torch and exec cases. It follows the
// same named path as runtimeWorker rather than searching the document.
func runtimeLauncherEnv(s *nvcrev1alpha1.WorkflowSpec) []string {
	if len(s.Dependencies) == 0 || len(s.Dependencies[0].Raw) == 0 {
		return nil
	}
	var rt trainerv1alpha1.TrainingRuntime
	if err := json.Unmarshal(s.Dependencies[0].Raw, &rt); err != nil {
		return nil
	}
	var env []string
	for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
		if rj.Name != "launcher" {
			continue
		}
		for _, c := range rj.Template.Spec.Template.Spec.Containers {
			if c.Name != nodeJobName {
				continue
			}
			for _, e := range c.Env {
				env = append(env, e.Name+"="+e.Value)
			}
		}
		return env
	}
	return nil
}

// TestValidateExecFramework drives golden-file cases for validateExecFramework.
// Each case provides a WorkloadRunSpec in input.yaml and expects a JSON object
// with a single "error" key — null on success or the error message on failure.
func TestValidateExecFramework(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "validate-exec-framework",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var spec nvcrev1alpha1.WorkloadRunSpec
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
