// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

type lookupInput struct {
	Category           string                   `yaml:"category"`
	Subcategory        string                   `yaml:"subcategory"`
	Target             nvcrev1alpha1.TargetSpec `yaml:"target"`
	NodesPerJob        int32                    `yaml:"nodesPerJob"`
	GpusPerNode        int32                    `yaml:"gpusPerNode"`
	EnableCheckpoint   bool                     `yaml:"enableCheckpoint"`
	SaveInterval       int32                    `yaml:"saveInterval"`
	SaveRetainInterval int32                    `yaml:"saveRetainInterval"`
	SaveTopK           int32                    `yaml:"saveTopK"`
	StorageSize        string                   `yaml:"storageSize"`
	TestScale          string                   `yaml:"testScale"`
	MaxBytes           string                   `yaml:"maxBytes"`
	NumIterations      int32                    `yaml:"numIterations"`
	NumCycles          int32                    `yaml:"numCycles"`
	MaxConcurrent      int32                    `yaml:"maxConcurrent"`
	Thresholds         map[string]string        `yaml:"thresholds"`
}

func TestLookup(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "lookup",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input lookupInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		entry := Lookup(input.Category, input.Subcategory)
		if entry == nil {
			tc.Actual = `{"found": false}`
			return nil
		}
		gpuArch := GPUArchFromNodeSelector(input.Target.NodeSelector)
		spec, buildErr := entry.Build(input.Target, BuildConfig{
			NodesPerJob:        input.NodesPerJob,
			GpusPerNode:        input.GpusPerNode,
			GPUArchitecture:    gpuArch,
			EnableCheckpoint:   input.EnableCheckpoint,
			SaveInterval:       input.SaveInterval,
			SaveRetainInterval: input.SaveRetainInterval,
			SaveTopK:           input.SaveTopK,
			StorageSize:        input.StorageSize,
			TestScale:          input.TestScale,
			MaxBytes:           input.MaxBytes,
			NumIterations:      input.NumIterations,
			NumCycles:          input.NumCycles,
			MaxConcurrent:      input.MaxConcurrent,
			Thresholds:         input.Thresholds,
		})
		if buildErr != nil {
			return buildErr
		}
		b, err := json.MarshalIndent(lookupOutput(spec), "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

func TestLookupUnsupportedNodesPerJob(t *testing.T) {
	// nemotron5-56b requires minGPUs: 32; 4 nodes × 4 GPUs = 16 is too few.
	entry := Lookup("training", "nemotron5-56b")
	if entry == nil {
		t.Fatal("expected nemotron5-56b to be registered")
	}
	_, err := entry.Build(nvcrev1alpha1.TargetSpec{}, BuildConfig{
		NodesPerJob:     4,
		GpusPerNode:     4,
		GPUArchitecture: "gb200",
	})
	if err == nil {
		t.Fatal("expected error for insufficient GPUs, got nil")
	}
	if !strings.Contains(err.Error(), "requires at least 32 GPUs") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// lookupOutput extracts key fields from a WorkflowSpec for golden file comparison.
func lookupOutput(spec nvcrev1alpha1.WorkflowSpec) map[string]any {
	result := map[string]any{
		"found": true,
	}
	// Orchestration
	orch := map[string]any{
		"iterations": spec.Orchestration.Iterations,
	}
	if spec.Orchestration.Target != nil && spec.Orchestration.Target.NodeSelector != nil {
		orch["targetNodeSelector"] = spec.Orchestration.Target.NodeSelector
	}
	if spec.Orchestration.Topology != nil {
		orch["topologyKey"] = spec.Orchestration.Topology.TopologyKey
	}
	if spec.Orchestration.Execution.MaxConcurrent > 0 {
		orch["maxConcurrent"] = spec.Orchestration.Execution.MaxConcurrent
	}
	// Validation thresholds
	if spec.Validation != nil && spec.Validation.Performance != nil &&
		spec.Validation.Performance.Thresholds != nil && len(spec.Validation.Performance.Thresholds.Thresholds) > 0 {
		result["validationThresholds"] = spec.Validation.Performance.Thresholds.Thresholds
	}
	result["orchestration"] = orch

	// Job template
	job := map[string]any{}
	if spec.JobTemplate.Spec.Workload.TrainJob != nil {
		tj := spec.JobTemplate.Spec.Workload.TrainJob
		trainJob := map[string]any{
			"runtimeRef": tj.RuntimeRef.Name,
		}
		if tj.Trainer != nil && tj.Trainer.Image != nil {
			trainJob["trainerImage"] = *tj.Trainer.Image
		}
		job["trainJob"] = trainJob
	}
	if spec.JobTemplate.Spec.NodeHealthMonitor != nil && spec.JobTemplate.Spec.NodeHealthMonitor.CEL != nil {
		job["nodeHealthMonitorCEL"] = *spec.JobTemplate.Spec.NodeHealthMonitor.CEL
	}
	if spec.JobTemplate.Spec.GoodputMeasurement != nil {
		gm := map[string]any{
			"logProfileRef": spec.JobTemplate.Spec.GoodputMeasurement.LogProfileRef,
		}
		if spec.JobTemplate.Spec.GoodputMeasurement.SampleInterval != nil {
			gm["sampleInterval"] = spec.JobTemplate.Spec.GoodputMeasurement.SampleInterval.Duration.String()
		}
		job["goodputMeasurement"] = gm
	}
	result["jobTemplate"] = job

	// Checkpoint config
	if spec.JobTemplate.Spec.Checkpoint != nil {
		cp := map[string]any{
			"pvcName": spec.JobTemplate.Spec.Checkpoint.PVCName,
		}
		if spec.JobTemplate.Spec.Checkpoint.MaxRestarts != nil {
			cp["maxRestarts"] = *spec.JobTemplate.Spec.Checkpoint.MaxRestarts
		}
		result["checkpoint"] = cp
	}

	// Dependencies
	if len(spec.Dependencies) > 0 {
		var deps []map[string]any
		for _, dep := range spec.Dependencies {
			var obj map[string]any
			if err := json.Unmarshal(dep.Raw, &obj); err != nil {
				continue
			}
			entry := map[string]any{
				"kind": obj["kind"],
				"name": obj["metadata"].(map[string]any)["name"],
			}
			// Capture PVC storage size for checkpoint tests.
			if obj["kind"] == "PersistentVolumeClaim" {
				if specMap, ok := obj["spec"].(map[string]any); ok {
					if res, ok := specMap["resources"].(map[string]any); ok {
						if req, ok := res["requests"].(map[string]any); ok {
							if storage, ok := req["storage"]; ok {
								entry["storage"] = storage
							}
						}
					}
				}
			}
			deps = append(deps, entry)
		}
		result["dependencies"] = deps
	}

	return result
}
