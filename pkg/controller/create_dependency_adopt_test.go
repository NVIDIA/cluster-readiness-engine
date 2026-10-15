// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// createDependencyResource adopts an existing holder of the name only when it
// is the object this create stands for: this Workflow's, and created for the
// same group attempt. Job-scoped copies keep their names across a group's
// retries, so a holder can be the previous attempt's copy, possibly mid-
// deletion. Adopting that would bind the new attempt to an object that is about
// to vanish (issue #389); the create reports errPreviousAttemptRetiring and the
// launch waits instead.
func TestCreateDependencyAdopt(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "create-dependency-adopt", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			// Holder is the existing object: absent, owned, or foreign.
			Holder string `json:"holder"`
			// HolderAttempt and Attempt are the group attempts the holder and
			// the object being created are for; 0 leaves them unannotated.
			HolderAttempt int  `json:"holderAttempt"`
			Attempt       int  `json:"attempt"`
			Terminating   bool `json:"terminating"`
		}
		if err := yaml.UnmarshalStrict([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		const ns, name = "ns", "dep-cm"
		workflow := &nvcrev1alpha1.Workflow{Name: "wf", Namespace: ns, UID: "wf-uid"}

		objs := []client.Object{}
		if in.Holder != testAbsent {
			holder := &corev1.ConfigMap{Name: name, Namespace: ns, Annotations: map[string]string{}}
			if in.Holder == "owned" {
				holder.Annotations[annotationWorkflowUID] = string(workflow.UID)
			}
			if in.HolderAttempt > 0 {
				holder.Annotations[annotationGroupAttempt] = strconv.Itoa(in.HolderAttempt)
			}
			if in.Terminating {
				holder.Finalizers = []string{"test.nvcre.nvidia.com/hold"}
				holder.DeletionTimestamp = new(metav1.Now())
			}
			objs = append(objs, holder)
		}
		c := fake.NewClientBuilder().WithScheme(newFallbackScheme(t)).WithObjects(objs...).Build()
		r := &WorkflowReconciler{Client: c, Scheme: c.Scheme()}

		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion("v1")
		obj.SetKind(kindConfigMap)
		obj.SetName(name)
		if in.Attempt > 0 {
			obj.SetAnnotations(map[string]string{annotationGroupAttempt: strconv.Itoa(in.Attempt)})
		}
		ref, created, err := r.createDependencyResource(context.Background(), nil, workflow, nvcrev1alpha1.DependencySpec{}, obj)

		out := map[string]any{"created": created, "recorded": ref != nil}
		if err != nil {
			out["retiring"] = errors.Is(err, errPreviousAttemptRetiring)
			_, collision := errors.AsType[*nameCollisionError](err)
			out["collision"] = collision
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
