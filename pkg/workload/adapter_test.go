// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package workload

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// --- ForSpec (all adapters) ---

func TestForSpec(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "for-spec",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var spec crev1alpha1.WorkloadSpec
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &spec); err != nil {
			return err
		}
		adapter, err := ForSpec(&spec)
		if err != nil {
			return err
		}
		typeName := fmt.Sprintf("%T", adapter)
		b, err := json.MarshalIndent(map[string]any{
			"adapterType": strings.TrimPrefix(typeName, "*"),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// --- TrainJob (unique patterns: RuntimePatches, different status conditions) ---

func TestTrainJobBuild(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-build",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Name      string                   `yaml:"name"`
			Namespace string                   `yaml:"namespace"`
			Spec      crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		obj, err := adapter.Build(input.Name, input.Namespace, &input.Spec)
		if err != nil {
			return err
		}
		gvk := adapter.GVK()
		b, err := json.MarshalIndent(map[string]any{
			"name":      obj.(metav1.ObjectMetaAccessor).GetObjectMeta().GetName(),
			"namespace": obj.(metav1.ObjectMetaAccessor).GetObjectMeta().GetNamespace(),
			"gvk": map[string]string{
				"group":   gvk.Group,
				"version": gvk.Version,
				"kind":    gvk.Kind,
			},
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestTrainJobInjectPodLabel(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-inject-label",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			LabelKey   string                   `yaml:"labelKey"`
			LabelValue string                   `yaml:"labelValue"`
			Spec       crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		adapter.InjectPodLabel(&input.Spec, input.LabelKey, input.LabelValue)

		b, err := trainJobRuntimePatchesToJSON(input.Spec.TrainJob.RuntimePatches)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestTrainJobSetNodeSelector(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-set-node-selector",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			NodeSelector map[string]string        `yaml:"nodeSelector"`
			Spec         crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		adapter.SetNodeSelector(&input.Spec, input.NodeSelector)

		b, err := trainJobRuntimePatchesToJSON(input.Spec.TrainJob.RuntimePatches)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestTrainJobSetNodeAffinity(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-set-node-affinity",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Affinity *corev1.NodeAffinity     `yaml:"affinity"`
			Spec     crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		adapter.SetNodeAffinity(&input.Spec, input.Affinity)

		b, err := trainJobRuntimePatchesToJSON(input.Spec.TrainJob.RuntimePatches)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestTrainJobGetStatus(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-get-status",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var meta struct {
			WrongType bool `yaml:"wrongType"`
		}
		_ = yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &meta)
		if meta.WrongType {
			adapter := &TrainJobAdapter{}
			_, err := adapter.GetStatus(&metav1.PartialObjectMetadata{})
			return err
		}
		var trainJob trainerv1alpha1.TrainJob
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &trainJob); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		status, err := adapter.GetStatus(&trainJob)
		if err != nil {
			return err
		}
		b, err := statusToJSON(status)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestTrainJobNodesRequired(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-nodes-required",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		n, err := adapter.NodesRequired(&input.Spec)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(map[string]any{
			"nodesRequired": n,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestTrainJobSetTolerations(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "trainjob-set-tolerations",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Tolerations []corev1.Toleration      `yaml:"tolerations"`
			Spec        crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		adapter := &TrainJobAdapter{}
		adapter.SetTolerations(&input.Spec, input.Tolerations)

		b, err := trainJobRuntimePatchesToJSON(input.Spec.TrainJob.RuntimePatches)
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// TestEnsureLauncherTarget verifies that EnsureLauncherTarget registers both
// the node and launcher replicated jobs in the controller-owned RuntimePatch,
// so that SetNodeAffinity/SetTolerations (whose targets come from existing
// patches) pin the MPI launcher alongside the workers, and HasLauncherTarget
// turns on the blanket MPI toleration. It is called twice to prove idempotence.
func TestEnsureLauncherTarget(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "ensure-launcher-target",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		EnsureLauncherTarget(input.Spec.TrainJob)
		EnsureLauncherTarget(input.Spec.TrainJob) // idempotent

		patches, err := trainJobRuntimePatchesToJSON(input.Spec.TrainJob.RuntimePatches)
		if err != nil {
			return err
		}
		var patchList []map[string]any
		if err := json.Unmarshal(patches, &patchList); err != nil {
			return err
		}
		b, err := json.MarshalIndent(map[string]any{
			"runtimePatches":    patchList,
			"hasLauncherTarget": HasLauncherTarget(&input.Spec),
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestHasLauncherTarget(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "has-launcher-target",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Spec crev1alpha1.WorkloadSpec `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		got := HasLauncherTarget(&input.Spec)
		b, err := json.MarshalIndent(map[string]any{
			"hasLauncherTarget": got,
		}, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// --- Test helpers ---

// trainJobRuntimePatchesToJSON serializes controller-owned RuntimePatches to a stable JSON format.
func trainJobRuntimePatchesToJSON(patches []trainerv1alpha1.RuntimePatch) ([]byte, error) {
	result := make([]map[string]any, 0)
	for _, patch := range patches {
		if patch.Manager != RuntimePatchManager {
			continue
		}
		if patch.TrainingRuntimeSpec == nil || patch.TrainingRuntimeSpec.Template == nil ||
			patch.TrainingRuntimeSpec.Template.Spec == nil {
			continue
		}
		for _, rjob := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
			entry := map[string]any{
				"targetJobs": []string{rjob.Name},
			}
			if rjob.Template != nil && rjob.Template.Spec != nil &&
				rjob.Template.Spec.Template != nil {
				podTemplate := rjob.Template.Spec.Template
				if podTemplate.Metadata != nil && len(podTemplate.Metadata.Labels) > 0 {
					entry["labels"] = podTemplate.Metadata.Labels
				}
				if podTemplate.Spec != nil {
					if len(podTemplate.Spec.NodeSelector) > 0 {
						entry["nodeSelector"] = podTemplate.Spec.NodeSelector
					}
					if podTemplate.Spec.Affinity != nil && podTemplate.Spec.Affinity.NodeAffinity != nil {
						entry["nodeAffinity"] = podTemplate.Spec.Affinity.NodeAffinity
					}
					if len(podTemplate.Spec.Tolerations) > 0 {
						entry["tolerations"] = podTemplate.Spec.Tolerations
					}
				}
			}
			result = append(result, entry)
		}
	}
	return json.MarshalIndent(result, "", "  ")
}

// statusToJSON serializes a WorkloadStatus to JSON.
func statusToJSON(status *WorkloadStatus) ([]byte, error) {
	output := map[string]any{
		"phase": string(status.Phase),
	}
	if status.Message != "" {
		output["message"] = status.Message
	}
	return json.MarshalIndent(output, "", "  ")
}
