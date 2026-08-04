// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	gzip "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/noderesults"
)

func decodeOrFail(t *testing.T, b []byte) []string {
	t.Helper()
	s, err := gzip.GunzipString(b)
	if err != nil {
		t.Fatalf("gunzipString: %v", err)
	}
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func newNodeResultsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := burninv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add burnin scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return s
}

func testWorkflow(name string) *burninv1alpha1.Workflow {
	return &burninv1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
	}
}

func getCMByRef(t *testing.T, c client.Client, ref *corev1.TypedLocalObjectReference) *corev1.ConfigMap {
	t.Helper()
	if ref == nil || ref.Name == "" {
		t.Fatalf("expected a non-nil node-results ref")
	}
	cm := &corev1.ConfigMap{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: ref.Name, Namespace: "default"}, cm); err != nil {
		t.Fatalf("get ConfigMap %s: %v", ref.Name, err)
	}
	return cm
}

func readSucceededNodes(t *testing.T, c client.Client, wf *burninv1alpha1.Workflow) []string {
	t.Helper()
	cm := getCMByRef(t, c, wf.Status.SucceededNodesRef)
	decoded, err := gzip.GunzipString(cm.BinaryData[noderesults.SucceededNodesConfigMapKey])
	if err != nil {
		t.Fatalf("gunzip CM value: %v", err)
	}
	if decoded == "" {
		return nil
	}
	return strings.Split(decoded, ",")
}

func readFailedNodes(t *testing.T, c client.Client, wf *burninv1alpha1.Workflow) []burninv1alpha1.FailedNode {
	t.Helper()
	cm := getCMByRef(t, c, wf.Status.FailedNodesRef)
	nodes, err := noderesults.DecodeFailedNodesFromConfigMap(cm)
	if err != nil {
		t.Fatalf("DecodeFailedNodesFromConfigMap: %v", err)
	}
	return nodes
}

func TestMergeSucceededNodesCSV(t *testing.T) {
	cases := map[string]struct {
		existingNodes []string // nil => no existing entry
		newNodes      []string
		want          []string
	}{
		"empty existing": {
			existingNodes: nil,
			newNodes:      []string{"gpu-02", "gpu-01"},
			want:          []string{"gpu-01", "gpu-02"},
		},
		"union and sort": {
			existingNodes: []string{"gpu-03", "gpu-01"},
			newNodes:      []string{"gpu-02", "gpu-04"},
			want:          []string{"gpu-01", "gpu-02", "gpu-03", "gpu-04"},
		},
		"dedupe across existing and new": {
			existingNodes: []string{"gpu-01", "gpu-02"},
			newNodes:      []string{"gpu-02", "gpu-03"},
			want:          []string{"gpu-01", "gpu-02", "gpu-03"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var existing []byte
			if tc.existingNodes != nil {
				enc, err := gzip.GzipString(strings.Join(tc.existingNodes, ","))
				if err != nil {
					t.Fatalf("seed gzip: %v", err)
				}
				existing = enc
			}

			merged, err := mergeSucceededNodesCSV(existing, tc.newNodes)
			if err != nil {
				t.Fatalf("mergeSucceededNodesCSV: %v", err)
			}
			got := decodeOrFail(t, merged)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestMergeFailedNodesJSON(t *testing.T) {
	enc := func(nodes []burninv1alpha1.FailedNode) []byte {
		jsonBytes, err := noderesults.FailedNodesToJSON(nodes)
		if err != nil {
			t.Fatalf("FailedNodesToJSON: %v", err)
		}
		b, err := gzip.GzipString(string(jsonBytes))
		if err != nil {
			t.Fatalf("gzip: %v", err)
		}
		return b
	}

	cases := map[string]struct {
		existing []burninv1alpha1.FailedNode // nil => no existing entry
		incoming []burninv1alpha1.FailedNode
		want     []burninv1alpha1.FailedNode
	}{
		"empty existing": {
			incoming: []burninv1alpha1.FailedNode{{Name: "gpu-02", Reason: burninv1alpha1.NodeFailureWorkloadFailed, Message: "boom"}},
			want:     []burninv1alpha1.FailedNode{{Name: "gpu-02", Reason: burninv1alpha1.NodeFailureWorkloadFailed, Message: "boom"}},
		},
		"union sort and dedupe by name+reason": {
			existing: []burninv1alpha1.FailedNode{{Name: "gpu-03", Reason: burninv1alpha1.NodeFailureWorkloadFailed}},
			incoming: []burninv1alpha1.FailedNode{
				{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureHardwareDetected},
				{Name: "gpu-03", Reason: burninv1alpha1.NodeFailureWorkloadFailed},
			},
			want: []burninv1alpha1.FailedNode{
				{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureHardwareDetected},
				{Name: "gpu-03", Reason: burninv1alpha1.NodeFailureWorkloadFailed},
			},
		},
		"same node two reasons kept": {
			incoming: []burninv1alpha1.FailedNode{
				{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureThresholdViolation},
				{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureHardwareDetected},
			},
			want: []burninv1alpha1.FailedNode{
				{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureHardwareDetected},
				{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureThresholdViolation},
			},
		},
		"message with comma and quote preserved": {
			incoming: []burninv1alpha1.FailedNode{
				{Name: "gpu-09", Reason: burninv1alpha1.NodeFailureWorkloadFailed, Message: `exited 1, signal "kill"`},
			},
			want: []burninv1alpha1.FailedNode{
				{Name: "gpu-09", Reason: burninv1alpha1.NodeFailureWorkloadFailed, Message: `exited 1, signal "kill"`},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var existing []byte
			if tc.existing != nil {
				existing = enc(tc.existing)
			}
			merged, err := mergeFailedNodesJSON(existing, tc.incoming)
			if err != nil {
				t.Fatalf("mergeFailedNodesJSON: %v", err)
			}
			decoded, err := gzip.GunzipString(merged)
			if err != nil {
				t.Fatalf("gunzip: %v", err)
			}
			got, err := noderesults.FailedNodesFromJSON([]byte(decoded))
			if err != nil {
				t.Fatalf("FailedNodesFromJSON: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("at %d got %+v want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestRecordSucceededNodes(t *testing.T) {
	s := newNodeResultsScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &WorkflowReconciler{Client: c, Scheme: s}
	wf := testWorkflow("test-cert-training-hello")
	ctx := context.Background()

	if err := r.recordSucceededNodes(ctx, wf, []string{"gpu-02", "gpu-01"}); err != nil {
		t.Fatalf("recordSucceededNodes (1): %v", err)
	}
	wantName := nodeResultsCMName(succeededNodesPrefix, string(wf.UID))
	if ref := wf.Status.SucceededNodesRef; ref == nil || ref.Name != wantName {
		t.Fatalf("SucceededNodesRef = %+v, want name %q", wf.Status.SucceededNodesRef, wantName)
	}
	if got := strings.Join(readSucceededNodes(t, c, wf), ","); got != "gpu-01,gpu-02" {
		t.Fatalf("after first write got %q", got)
	}
	if err := r.recordSucceededNodes(ctx, wf, []string{"gpu-02", "gpu-03"}); err != nil {
		t.Fatalf("recordSucceededNodes (2): %v", err)
	}
	if wf.Status.SucceededNodesRef.Name != wantName {
		t.Fatalf("second write changed ConfigMap name to %q (want %q)", wf.Status.SucceededNodesRef.Name, wantName)
	}
	if got := strings.Join(readSucceededNodes(t, c, wf), ","); got != "gpu-01,gpu-02,gpu-03" {
		t.Fatalf("after second write got %q", got)
	}
}

func TestRecordFailedNodes(t *testing.T) {
	s := newNodeResultsScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &WorkflowReconciler{Client: c, Scheme: s}
	wf := testWorkflow("test-cert-training-hello")
	ctx := context.Background()

	if err := r.recordFailedNodes(ctx, wf, []burninv1alpha1.FailedNode{
		{Name: "gpu-02", Reason: burninv1alpha1.NodeFailureWorkloadFailed, Message: "boom"},
	}); err != nil {
		t.Fatalf("recordFailedNodes (1): %v", err)
	}
	wantFailedName := nodeResultsCMName(failedNodesPrefix, string(wf.UID))
	if ref := wf.Status.FailedNodesRef; ref == nil || ref.Name != wantFailedName {
		t.Fatalf("FailedNodesRef = %+v, want name %q", wf.Status.FailedNodesRef, wantFailedName)
	}
	got := readFailedNodes(t, c, wf)
	if len(got) != 1 || got[0].Name != "gpu-02" || got[0].Message != "boom" {
		t.Fatalf("after first write got %+v", got)
	}

	if err := r.recordFailedNodes(ctx, wf, []burninv1alpha1.FailedNode{
		{Name: "gpu-02", Reason: burninv1alpha1.NodeFailureWorkloadFailed, Message: "boom"},
		{Name: "gpu-01", Reason: burninv1alpha1.NodeFailureHardwareDetected},
	}); err != nil {
		t.Fatalf("recordFailedNodes (2): %v", err)
	}
	if wf.Status.FailedNodesRef.Name != wantFailedName {
		t.Fatalf("second write changed ConfigMap name to %q (want %q)", wf.Status.FailedNodesRef.Name, wantFailedName)
	}
	got = readFailedNodes(t, c, wf)
	if len(got) != 2 || got[0].Name != "gpu-01" || got[1].Name != "gpu-02" {
		t.Fatalf("after second write got %+v", got)
	}
}
