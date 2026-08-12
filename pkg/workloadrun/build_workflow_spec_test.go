// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workloadrun

import (
	"encoding/json"
	"strings"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"sigs.k8s.io/yaml"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// BuildWorkflowSpec renders the Workflow that "ncrectl workloadrun render" and
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
			Run           burninv1alpha1.WorkloadRun `json:"run"`
			GpusPerNode   int32                      `json:"gpusPerNode"`
			MlnxPerNode   int32                      `json:"mlnxPerNode"`
			EnableMNNVL   bool                       `json:"enableMNNVL"`
			FrameworkType string                     `json:"frameworkType"`
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
	Validation *burninv1alpha1.ValidationSpec `json:"validation"`
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
	// WorkerSchedulerName and WorkerQueueLabel are omitted when empty so that
	// existing expected files do not need to be updated for the gang-scheduler fix.
	WorkerSchedulerName string `json:"workerSchedulerName,omitempty"`
	WorkerQueueLabel    string `json:"workerQueueLabel,omitempty"`
}

func project(s *burninv1alpha1.WorkflowSpec) projection {
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
	out.WorkerEnv, out.WorkerVolumeMounts, out.RuntimeVolumes = runtimeWorker(s)
	out.WorkerSchedulerName, out.WorkerQueueLabel = runtimeScheduler(s)
	return out
}

// dependencyKind reads the Kind out of a dependency's embedded raw resource.
func dependencyKind(d *burninv1alpha1.DependencySpec) string {
	var obj struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(d.Raw, &obj); err != nil {
		return ""
	}
	return obj.Kind
}

// runtimeWorker reads the worker container out of the runtime dependency and
// returns its environment variables, its volume mounts, and the pod volumes.
//
// It follows one named path rather than searching the document, because a
// search would also find containers that the workload does not run in, such as
// an init container or an MPI launcher. A variable on one of those does not
// reach the worker processes, and a search would report it as though it did.
func runtimeWorker(s *burninv1alpha1.WorkflowSpec) (env, mounts, volumes []string) {
	if len(s.Dependencies) == 0 || len(s.Dependencies[0].Raw) == 0 {
		return nil, nil, nil
	}

	// The path below is the TrainingRuntime shape that pkg/platform builds.
	var dep runtimeDep
	if err := json.Unmarshal(s.Dependencies[0].Raw, &dep); err != nil {
		return nil, nil, nil
	}
	// Pick the job by name. An MPI runtime holds two jobs, "node" and
	// "launcher", and only "node" runs the worker processes. Taking the first
	// job would read whichever one pkg/platform happens to write first.
	idx := -1
	for i, j := range dep.Spec.Template.Spec.ReplicatedJobs {
		if j.Name == "node" {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, nil, nil
	}

	pod := dep.Spec.Template.Spec.ReplicatedJobs[idx].Template.Spec.Template.Spec
	for _, v := range pod.Volumes {
		volumes = append(volumes, v.Name)
	}
	for _, c := range pod.Containers {
		// "node" is the name pkg/platform gives the worker container.
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
	return env, mounts, volumes
}

// runtimeScheduler extracts the schedulerName and kai.scheduler/queue label
// from the "node" replicatedJob in the first runtime dependency.
func runtimeScheduler(s *burninv1alpha1.WorkflowSpec) (schedulerName, queueLabel string) {
	if len(s.Dependencies) == 0 || len(s.Dependencies[0].Raw) == 0 {
		return "", ""
	}
	var dep runtimeDep
	if err := json.Unmarshal(s.Dependencies[0].Raw, &dep); err != nil {
		return "", ""
	}
	for _, j := range dep.Spec.Template.Spec.ReplicatedJobs {
		if j.Name != "node" {
			continue
		}
		schedulerName = j.Template.Spec.Template.Spec.SchedulerName
		queueLabel = j.Template.Metadata.Labels["kai.scheduler/queue"]
		return schedulerName, queueLabel
	}
	return "", ""
}

// runtimeDep is the decoded shape of a TrainingRuntime dependency produced by
// pkg/platform. Both runtimeWorker and runtimeScheduler share this struct so
// the Unmarshal path is written once.
type runtimeDep struct {
	Spec struct {
		Template struct {
			Spec struct {
				ReplicatedJobs []struct {
					Name     string `json:"name"`
					Template struct {
						Metadata struct {
							Labels map[string]string `json:"labels"`
						} `json:"metadata"`
						Spec struct {
							Template struct {
								Spec struct {
									SchedulerName string `json:"schedulerName"`
									Containers    []struct {
										Name string `json:"name"`
										Env  []struct {
											Name  string `json:"name"`
											Value string `json:"value"`
										} `json:"env"`
										VolumeMounts []struct {
											Name      string `json:"name"`
											MountPath string `json:"mountPath"`
										} `json:"volumeMounts"`
									} `json:"containers"`
									Volumes []struct {
										Name string `json:"name"`
									} `json:"volumes"`
								} `json:"spec"`
							} `json:"template"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"replicatedJobs"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// TestBuildCLIJobTemplate_NilExecPanics verifies that buildCLIJobTemplate
// panics with a clear message when the exec framework is selected but
// spec.framework.exec is nil (a programming error: the caller set neither
// Torch nor MPI but left Exec nil).
func TestBuildCLIJobTemplate_NilExecPanics(t *testing.T) {
	run := &burninv1alpha1.WorkloadRun{}
	run.Name = "wr-nil-exec"
	run.Spec.Image = "nvcr.io/test:latest"
	run.Spec.NumNodes = 1
	// Framework.Exec is deliberately left nil; Torch and MPI are also nil,
	// so frameworkType falls through to the exec default.

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic but got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value is not a string: %v", r)
		}
		want := "spec.framework.exec is nil"
		if !strings.Contains(msg, want) {
			t.Fatalf("panic message %q does not contain %q", msg, want)
		}
	}()

	buildCLIJobTemplate(run, "exec", 8)
}
