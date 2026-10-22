// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/workload"
)

// placementDryRunSpec builds a resolved WorkflowSpec for a placement case.
//
// When mpi is set the launcher replicatedJob is registered, which is what makes
// the MPI case meaningful: SetNodeAffinity and SetTolerations derive their
// targets from existing runtimePatches via allTargetJobs, which falls back to
// ["node"] alone when nothing is registered. Without the launcher entry a
// launcher-escapes-the-target regression would be invisible here.
func placementDryRunSpec(
	placement string,
	numNodes int32,
	mpi bool,
	target *nvcrev1alpha1.TargetSpec,
) *nvcrev1alpha1.WorkflowSpec {
	trainJob := &trainerv1alpha1.TrainJobSpec{
		RuntimeRef: trainerv1alpha1.RuntimeRef{
			Name: dryRuntimeName,
			Kind: new("TrainingRuntime"),
		},
		Trainer: &trainerv1alpha1.Trainer{
			Image:    new("test:latest"),
			NumNodes: new(numNodes),
		},
	}
	if mpi {
		workload.EnsureLauncherTarget(trainJob)
	}

	return &nvcrev1alpha1.WorkflowSpec{
		JobTemplate: nvcrev1alpha1.JobTemplateSpec{
			Spec: nvcrev1alpha1.JobSpec{
				Workload: nvcrev1alpha1.WorkloadSpec{TrainJob: trainJob},
			},
		},
		Orchestration: nvcrev1alpha1.OrchestrationSpec{
			Target:     target,
			Placement:  placement,
			Iterations: 1,
		},
	}
}

// placementNodes is the fleet every case runs against: four nodes carrying a
// gpu.product label, which is more than any case asks for. The surplus is the
// point. Under Pinned the dry run slices the first N and names them; under
// Unpinned it must name none of them, and a fleet exactly the size of the
// request could not tell those two apart.
func placementNodes() []corev1.Node {
	names := []string{dryNodeName, "dry-node-02", "dry-node-03", "dry-node-04"}
	nodes := make([]corev1.Node, 0, len(names))
	for _, name := range names {
		nodes = append(nodes, corev1.Node{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/hostname":   name,
				"nvidia.com/gpu.product":   "NVIDIA-H100-80GB-HBM3",
				"nvidia.com/gpu.present":   "true",
				"nvcre.nvidia.com/testbed": "burnin",
			},
		})
	}
	return nodes
}

// podPlacement is the per-replicatedJob slice of the submitted Job that
// placement actually controls.
type podPlacement struct {
	NodeAffinity *corev1.NodeAffinity `json:"nodeAffinity"`
	Tolerations  []corev1.Toleration  `json:"tolerations,omitempty"`
	NodeSelector map[string]string    `json:"nodeSelector,omitempty"`
}

// TestDryRunPlacement pins what a `--dry-run` render of a Workflow submits in
// each placement mode. The inputs are bare Workflow specs, which is the one
// render input that can carry placement: Unpinned (see ADR-089).
//
// The dry run exists to tell an operator what the controller is about to
// create, so a divergence here is worse than having no preview: it would
// validate a pinned Job against the API server and then the controller would
// create an unpinned one. Both modes run against the same four-node fleet, so
// the diff between the two goldens is the whole behavioral difference.
//
// Every replicatedJob is recorded, not just the worker. SetNodeAffinity writes
// all of them precisely so an MPI launcher cannot schedule outside the target,
// and a golden that only looked at "node" would pass while the launcher landed
// anywhere with a free GPU.
func TestDryRunPlacement(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "dryrun-placement",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			Placement string                    `json:"placement"`
			NumNodes  int32                     `json:"numNodes"`
			MPI       bool                      `json:"mpi"`
			Target    *nvcrev1alpha1.TargetSpec `json:"target"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return fmt.Errorf("parse input.yaml: %w", err)
		}

		spec := placementDryRunSpec(in.Placement, in.NumNodes, in.MPI, in.Target)

		rec := &recorder{}
		c := countingClient(t, rec)
		if _, err := DryRunCreate(context.Background(), c, "default", spec, placementNodes(), nil); err != nil {
			return fmt.Errorf("DryRunCreate: %w", err)
		}

		var out struct {
			Pods map[string]podPlacement `json:"pods"`
			// NumNodes must survive untouched in both modes. Under Unpinned the
			// group has no nodes, so a SetNumNodes call that was not skipped
			// would write 0 here and the job would never run. On GB200 it would
			// also desynchronize the trainer from the ComputeDomain templated
			// off the same number.
			NumNodes *int32 `json:"numNodes"`
		}
		out.Pods = map[string]podPlacement{}

		for _, obj := range rec.submitted {
			job, ok := obj.(*nvcrev1alpha1.Job)
			if !ok {
				continue
			}
			trainJob := job.Spec.Workload.TrainJob
			out.NumNodes = trainJob.Trainer.NumNodes
			for _, patch := range trainJob.RuntimePatches {
				if patch.TrainingRuntimeSpec == nil ||
					patch.TrainingRuntimeSpec.Template == nil ||
					patch.TrainingRuntimeSpec.Template.Spec == nil {
					continue
				}
				for _, rjob := range patch.TrainingRuntimeSpec.Template.Spec.ReplicatedJobs {
					if rjob.Template == nil || rjob.Template.Spec == nil ||
						rjob.Template.Spec.Template == nil ||
						rjob.Template.Spec.Template.Spec == nil {
						continue
					}
					podSpec := rjob.Template.Spec.Template.Spec
					entry := podPlacement{
						Tolerations:  podSpec.Tolerations,
						NodeSelector: podSpec.NodeSelector,
					}
					if podSpec.Affinity != nil {
						entry.NodeAffinity = podSpec.Affinity.NodeAffinity
					}
					out.Pods[rjob.Name] = entry
				}
			}
			break
		}

		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
