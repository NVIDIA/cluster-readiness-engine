// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// makeBaseNode returns a minimal healthy node for predicate tests.
func makeBaseNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/gpu.product": "NVIDIA-H100-80GB-HBM3",
			},
			Annotations: map[string]string{
				"node.alpha.kubernetes.io/ttl": "0",
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: false},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// callUpdatePredicate invokes the UpdateFunc of nodeHealthChangePredicate.
func callUpdatePredicate(t *testing.T, oldNode, newNode *corev1.Node) bool {
	t.Helper()
	r := &JobReconciler{}
	pred := r.nodeHealthChangePredicate()
	return pred.Update(event.UpdateEvent{
		ObjectOld: oldNode,
		ObjectNew: newNode,
	})
}

// TestNodeHealthPredicateAnnotationOnlyChange asserts that a node update
// differing only in annotations does NOT trigger a reconcile.
// Kubelet heartbeat annotations change every ~10 s per node; on large clusters
// this caused constant reconcile storms on the Job controller (issue #119).
func TestNodeHealthPredicateAnnotationOnlyChange(t *testing.T) {
	t.Parallel()

	oldNode := makeBaseNode("node-1")
	newNode := makeBaseNode("node-1")
	newNode.Annotations["kubectl.kubernetes.io/last-applied-configuration"] = `{"heartbeat":"updated"}`

	if callUpdatePredicate(t, oldNode, newNode) {
		t.Fatal("annotation-only node update should NOT trigger reconcile but predicate returned true")
	}
}

// TestNodeHealthPredicateConditionChange asserts that a Ready→NotReady
// status change DOES trigger a reconcile.
func TestNodeHealthPredicateConditionChange(t *testing.T) {
	t.Parallel()

	oldNode := makeBaseNode("node-1")
	newNode := makeBaseNode("node-1")
	newNode.Status.Conditions[0].Status = corev1.ConditionFalse

	if !callUpdatePredicate(t, oldNode, newNode) {
		t.Fatal("condition change should trigger reconcile but predicate returned false")
	}
}

// TestNodeHealthPredicateTaintChange asserts that adding a taint DOES trigger
// a reconcile.
func TestNodeHealthPredicateTaintChange(t *testing.T) {
	t.Parallel()

	oldNode := makeBaseNode("node-1")
	newNode := makeBaseNode("node-1")
	newNode.Spec.Taints = append(newNode.Spec.Taints, corev1.Taint{
		Key:    "nvidia.com/gpu",
		Value:  "unhealthy",
		Effect: corev1.TaintEffectNoSchedule,
	})

	if !callUpdatePredicate(t, oldNode, newNode) {
		t.Fatal("taint change should trigger reconcile but predicate returned false")
	}
}

// TestNodeHealthPredicateLabelChange asserts that a label change DOES trigger
// a reconcile (GPUd-style health labels).
func TestNodeHealthPredicateLabelChange(t *testing.T) {
	t.Parallel()

	oldNode := makeBaseNode("node-1")
	newNode := makeBaseNode("node-1")
	newNode.Labels["nvidia.com/gpu.health"] = "not-ready"

	if !callUpdatePredicate(t, oldNode, newNode) {
		t.Fatal("label change should trigger reconcile but predicate returned false")
	}
}

// TestNodeHealthPredicateCordonChange asserts that cordoning a node DOES
// trigger a reconcile.
func TestNodeHealthPredicateCordonChange(t *testing.T) {
	t.Parallel()

	oldNode := makeBaseNode("node-1")
	newNode := makeBaseNode("node-1")
	newNode.Spec.Unschedulable = true

	if !callUpdatePredicate(t, oldNode, newNode) {
		t.Fatal("cordon change should trigger reconcile but predicate returned false")
	}
}

// TestNodeHealthPredicateNoChange asserts that an identical node update does
// NOT trigger a reconcile.
func TestNodeHealthPredicateNoChange(t *testing.T) {
	t.Parallel()

	node := makeBaseNode("node-1")

	if callUpdatePredicate(t, node, node) {
		t.Fatal("identical node update should NOT trigger reconcile but predicate returned true")
	}
}
