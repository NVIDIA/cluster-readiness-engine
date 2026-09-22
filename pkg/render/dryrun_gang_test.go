// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	nvcreplatform "github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

// labelKeyQueue is the KAI queue label key, spelled out here so a rename in
// pkg/platform shows up as a test failure rather than silently following.
const (
	labelKeyQueue = "kai.scheduler/queue"
	// configuredQueue is the queue the persisted intent names in these cases.
	configuredQueue = "queue-a"
)

// recorder captures what a dry run actually submits. Counting calls proves a
// check ran before any request; keeping the objects proves the requests carry
// what they are supposed to.
type recorder struct {
	creates   int
	submitted []client.Object
}

// trainJobLabels returns the labels on the submitted TrainJob, or nil when no
// TrainJob was submitted at all.
func (r *recorder) trainJobLabels() map[string]string {
	for _, obj := range r.submitted {
		if _, ok := obj.(*trainerv1alpha1.TrainJob); ok {
			return obj.GetLabels()
		}
	}
	return nil
}

// runtimeScheduling flattens the scheduling fields of the submitted
// TrainingRuntime: the queue label at both template levels and the pod
// scheduler name. ADR-076 puts the queue in all three places, so a dry run
// that validated a TrainJob with the right label against a runtime with the
// wrong one would be reporting a gang that cannot form.
func (r *recorder) runtimeScheduling(t *testing.T) map[string]string {
	t.Helper()
	for _, obj := range r.submitted {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok || u.GetKind() != "TrainingRuntime" {
			continue
		}
		jobs, found, err := unstructured.NestedSlice(u.Object,
			"spec", "template", "spec", "replicatedJobs")
		require.NoError(t, err)
		require.Truef(t, found, "submitted TrainingRuntime has no replicatedJobs")

		out := map[string]string{}
		for _, rj := range jobs {
			job, isMap := rj.(map[string]any)
			require.Truef(t, isMap, "replicatedJob is not an object")
			name, _, _ := unstructured.NestedString(job, "name")

			jobQueue, _, _ := unstructured.NestedString(job,
				"template", "metadata", "labels", labelKeyQueue)
			podQueue, _, _ := unstructured.NestedString(job,
				"template", "spec", "template", "metadata", "labels", labelKeyQueue)
			scheduler, _, _ := unstructured.NestedString(job,
				"template", "spec", "template", "spec", "schedulerName")

			out[name+"/jobTemplateQueue"] = jobQueue
			out[name+"/podTemplateQueue"] = podQueue
			out[name+"/schedulerName"] = scheduler
		}
		return out
	}
	return nil
}

// countingClient wraps a fake client and records every Create it is asked to
// make, dry-run or not.
func countingClient(t *testing.T, rec *recorder) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := nvcrev1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add nvcre scheme: %v", err)
	}
	if err := trainerv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add trainer scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object,
			opts ...client.CreateOption) error {
			rec.creates++
			rec.submitted = append(rec.submitted, obj.DeepCopyObject().(client.Object))
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
}

// runtimeDependencyYAML is a resolved TrainingRuntime dependency whose queue
// label is substituted in, so a case can make the runtime agree with the
// persisted intent or conflict with it. Written as YAML rather than nested
// maps because that is how these documents appear everywhere else.
const runtimeDependencyYAML = `
apiVersion: trainer.kubeflow.org/v1alpha1
kind: TrainingRuntime
metadata:
  name: dry-runtime
spec:
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            metadata:
              labels:
                kai.scheduler/queue: %[1]s
            spec:
              template:
                metadata:
                  labels:
                    kai.scheduler/queue: %[1]s
                spec:
                  schedulerName: kai-scheduler
                  containers:
                    - name: node
                      image: test:latest
`

// gangDryRunSpec builds a resolved WorkflowSpec whose runtime dependency
// carries runtimeQueue, so a case can agree with the persisted intent or
// conflict with it.
func gangDryRunSpec(t *testing.T, runtimeQueue string) *nvcrev1alpha1.WorkflowSpec {
	t.Helper()

	raw, err := yaml.YAMLToJSON([]byte(fmt.Sprintf(runtimeDependencyYAML, runtimeQueue)))
	if err != nil {
		t.Fatalf("convert runtime dependency: %v", err)
	}

	return &nvcrev1alpha1.WorkflowSpec{
		GangScheduler: &nvcrev1alpha1.GangSchedulerSpec{
			SchedulerName: "kai-scheduler",
			Queue:         configuredQueue,
			QueueLabelKey: "kai.scheduler/queue",
		},
		JobTemplate: nvcrev1alpha1.JobTemplateSpec{
			Spec: nvcrev1alpha1.JobSpec{
				WorkloadMetadata: &nvcrev1alpha1.WorkloadMetadata{
					Labels: map[string]nvcrev1alpha1.WorkloadLabelValue{
						labelKeyQueue: configuredQueue,
					},
				},
				Workload: nvcrev1alpha1.WorkloadSpec{
					TrainJob: &trainerv1alpha1.TrainJobSpec{
						RuntimeRef: trainerv1alpha1.RuntimeRef{
							Name: "dry-runtime",
							Kind: new("TrainingRuntime"),
						},
						Trainer: &trainerv1alpha1.Trainer{
							Image:    new("test:latest"),
							NumNodes: new(int32(1)),
						},
					},
				},
			},
		},
		Dependencies: []nvcrev1alpha1.DependencySpec{{Raw: raw}},
		Orchestration: nvcrev1alpha1.OrchestrationSpec{
			Iterations: 1,
		},
	}
}

func dryRunNodes() []corev1.Node {
	return []corev1.Node{{
		Name:   "dry-node-01",
		Labels: map[string]string{"kubernetes.io/hostname": "dry-node-01"},
	}}
}

// TestDryRunGangScheduling drives DryRunCreate through the gang-scheduling
// and label contracts with golden files, one case per directory under
// testdata/dryrun-gang.
//
// The golden records three things together: the error DryRunCreate returned,
// how many create requests it issued before returning, and what the submitted
// TrainJob and TrainingRuntime actually carried. Recording the count next to
// the error is what makes a conflict case meaningful — a check that ran after
// the requests would still return the right error. The consistent cases record
// a non-zero count for the same reason: they prove the conflict cases are not
// passing merely because nothing ever ran.
//
// `--dry-run` reports what the API server makes of each manifest, which is
// exactly the wrong way to learn that NVCRE is about to submit manifests
// inconsistent with the queue its owner configured: the API server would
// happily accept them. So the conflict has to surface as a conflict, before
// any request is issued.
func TestDryRunGangScheduling(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "dryrun-gang",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in dryRunGangInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return fmt.Errorf("parse input.yaml: %w", err)
		}

		spec := gangDryRunSpec(t, in.RuntimeQueue)
		if in.StripRuntimeQueueLabels {
			stripRuntimeQueueLabels(t, spec)
		}
		if in.NoGangScheduler {
			spec.GangScheduler = nil
		}
		if in.WorkloadLabels != nil {
			spec.JobTemplate.Spec.WorkloadMetadata = workload.MetadataFrom(in.WorkloadLabels)
		}

		var out dryRunGangResult
		if in.Certification != nil {
			// Resolve real Certification options rather than handing the
			// transform an already-merged map: the per-key merge between the
			// global and per-category levels is itself part of the contract.
			// The transform stage is the same code the controller and offline
			// render use, so a change to the merge order shows up here too.
			intent := spec.GangScheduler
			spec.GangScheduler = nil
			global := nvcrev1alpha1.CategoryOptions{
				WorkloadMetadata: workload.MetadataFrom(in.Certification.Global),
			}
			category := &nvcrev1alpha1.CategoryOptions{
				WorkloadMetadata: workload.MetadataFrom(in.Certification.Category),
			}
			opts := controller.ResolveOptions(&global, category)
			if err := nvcreplatform.ApplyResolvedWorkflowTransforms(spec,
				nvcreplatform.ResolvedWorkflowTransforms{
					GangScheduler:  intent,
					WorkloadLabels: workload.LabelsOf(opts.WorkloadMetadata),
				}); err != nil {
				return fmt.Errorf("ApplyResolvedWorkflowTransforms: %w", err)
			}
		}

		rec := &recorder{}
		c := countingClient(t, rec)
		_, err := DryRunCreate(context.Background(), c, "default", spec, dryRunNodes())
		if err != nil {
			out.Error = err.Error()
		}
		out.Creates = rec.creates
		out.TrainJobLabels = rec.trainJobLabels()
		out.RuntimeScheduling = rec.runtimeScheduling(t)

		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// dryRunGangInput describes one dryrun-gang case. Every case starts from
// gangDryRunSpec: a persisted queue-a intent, a workload object labelled
// queue-a, and a runtime dependency labelled with runtimeQueue.
type dryRunGangInput struct {
	// RuntimeQueue is the queue the runtime dependency carries at both
	// template levels, so a case can agree with the intent or conflict with it.
	RuntimeQueue string `yaml:"runtimeQueue"`
	// StripRuntimeQueueLabels removes the queue label from the runtime at
	// every level, standing in for a runtime that was never given one.
	StripRuntimeQueueLabels bool `yaml:"stripRuntimeQueueLabels"`
	// NoGangScheduler drops the persisted intent, leaving only the label
	// contract in force.
	NoGangScheduler bool `yaml:"noGangScheduler"`
	// WorkloadLabels, when set, replaces the Job template's workloadMetadata.
	WorkloadLabels map[string]string `yaml:"workloadLabels"`
	// Certification, when set, runs the real Certification option resolution
	// and transform stage over the spec before the dry run.
	Certification *struct {
		Global   map[string]string `yaml:"global"`
		Category map[string]string `yaml:"category"`
	} `yaml:"certification"`
}

// dryRunGangResult is the golden shape.
type dryRunGangResult struct {
	Error             string            `json:"error,omitempty"`
	Creates           int               `json:"creates"`
	TrainJobLabels    map[string]string `json:"trainJobLabels,omitempty"`
	RuntimeScheduling map[string]string `json:"runtimeScheduling,omitempty"`
}

// stripRuntimeQueueLabels removes the queue label from every level of the
// spec's runtime dependency, standing in for a runtime that was never given
// one.
func stripRuntimeQueueLabels(t *testing.T, spec *nvcrev1alpha1.WorkflowSpec) {
	t.Helper()
	require.Len(t, spec.Dependencies, 1, "expected exactly one runtime dependency")

	obj := map[string]any{}
	require.NoError(t, json.Unmarshal(spec.Dependencies[0].Raw, &obj))

	jobs, found, err := unstructured.NestedSlice(obj, "spec", "template", "spec", "replicatedJobs")
	require.NoError(t, err)
	require.True(t, found)
	for _, rj := range jobs {
		job := rj.(map[string]any)
		unstructured.RemoveNestedField(job, "template", "metadata", "labels", labelKeyQueue)
		unstructured.RemoveNestedField(job,
			"template", "spec", "template", "metadata", "labels", labelKeyQueue)
	}
	require.NoError(t, unstructured.SetNestedSlice(obj, jobs,
		"spec", "template", "spec", "replicatedJobs"))

	raw, err := json.Marshal(obj)
	require.NoError(t, err)
	spec.Dependencies[0].Raw = raw
}
