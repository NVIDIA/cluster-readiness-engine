// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// workloadVolumesInput is each case's input.yaml: the framework the spec
// selects, whether spec.config is set, and the user's volumes and mounts.
type workloadVolumesInput struct {
	Framework    string               `json:"framework"`
	HasConfig    bool                 `json:"hasConfig,omitempty"`
	Volumes      []corev1.Volume      `json:"volumes,omitempty"`
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`
}

// workloadValidateResult is the golden of both tests in this file: the
// refusal, or null when the entries are accepted.
type workloadValidateResult struct {
	Error *string `json:"error"`
}

// workloadInitContainersInput is each case's input.yaml: the user's init
// containers.
type workloadInitContainersInput struct {
	InitContainers []corev1.Container `json:"initContainers,omitempty"`
}

// TestValidateWorkloadVolumes pins which spec.volumes names and
// spec.volumeMounts paths a WorkloadRun may not use, on which framework, and
// the message each refusal carries. The reserved set is exactly what the
// builders and the Kubeflow Trainer MPI plugin add to the same pod themselves
// (see ValidateWorkloadVolumes); the accepting cases show that nearby names
// the launcher alone uses, such as ssh-keys and /etc/mpi, stay available.
func TestValidateWorkloadVolumes(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "validate-workload-volumes"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in workloadVolumesInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		var res workloadValidateResult
		if err := ValidateWorkloadVolumes(in.Volumes, in.VolumeMounts, in.Framework, in.HasConfig); err != nil {
			msg := err.Error()
			res.Error = &msg
		}
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out) + "\n"
		return nil
	})
}

// TestValidateWorkloadInitContainers pins that an init container may not take
// the workload container's name, node, on any framework, and that the
// launcher's own init container names stay available, since user init
// containers never share a pod with them.
func TestValidateWorkloadInitContainers(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "validate-workload-init-containers"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in workloadInitContainersInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		var res workloadValidateResult
		if err := ValidateWorkloadInitContainers(in.InitContainers); err != nil {
			msg := err.Error()
			res.Error = &msg
		}
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out) + "\n"
		return nil
	})
}
