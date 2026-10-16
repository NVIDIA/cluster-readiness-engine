// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// TestMultiGroupWithoutBandwidthHasNoGroupBandwidth pins that a multi-group
// category whose workflow does not measure bandwidth, such as training, gets
// no group bandwidth section, even with a failed group.
func TestMultiGroupWithoutBandwidthHasNoGroupBandwidth(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	wf := &nvcrev1alpha1.Workflow{
		Status: nvcrev1alpha1.WorkflowStatus{
			Orchestration: &nvcrev1alpha1.OrchestrationStatus{
				TotalGroups: 2,
				Groups: []nvcrev1alpha1.GroupStatus{
					{
						Name: "train-0", Nodes: []string{"n0", "n1"},
						Phase:  nvcrev1alpha1.GroupFailed,
						JobRef: &nvcrev1alpha1.WorkloadReference{Name: "j0"},
					},
					{
						Name: "train-1", Nodes: []string{"n2", "n3"},
						Phase:  nvcrev1alpha1.GroupSucceeded,
						JobRef: &nvcrev1alpha1.WorkloadReference{Name: "j1"},
					},
				},
			},
		},
	}
	wf.Name, wf.Namespace = "training", "default"
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	cat := &CategoryReport{Domain: "training"}
	PopulateCategoryFromWorkflow(context.Background(), c, cat, wf)

	if len(cat.GroupBandwidth) != 0 {
		t.Fatalf("GroupBandwidth = %+v, want none", cat.GroupBandwidth)
	}
	var buf bytes.Buffer
	printCategoryCard(&buf, cat)
	if strings.Contains(buf.String(), "Bandwidth by group") {
		t.Errorf("output has a group bandwidth section:\n%s", buf.String())
	}
	data, err := json.Marshal(cat)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "groupBandwidth") {
		t.Errorf("JSON has groupBandwidth: %s", data)
	}
}
