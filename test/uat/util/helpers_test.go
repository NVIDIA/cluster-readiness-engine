// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build uat

package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A pod read from a real cluster carries CNI annotations whose values change
// on every run. They must not reach a golden file.
func TestStripPodVolatileRemovesCNIAnnotations(t *testing.T) {
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "workload-node-0-0-abcde",
			Annotations: map[string]string{
				"cni.projectcalico.org/containerID": "c62cbba8b5c6751f0d078ff3",
				"cni.projectcalico.org/podIP":       "172.29.112.176/32",
				"cni.projectcalico.org/podIPs":      "172.29.112.176/32",
				"k8s.v1.cni.cncf.io/network-status": "[{...}]",
				"io.cilium/whatever":                "x",
				"jobset.sigs.k8s.io/jobset-uid":     "1234",
				// Must survive: NVCRE writes this one on purpose.
				"nvcrectl.nvidia.com/applied-overrides": "gb200",
			},
		},
	}

	StripPodVolatile(&pod)

	for _, gone := range []string{
		"cni.projectcalico.org/containerID",
		"cni.projectcalico.org/podIP",
		"cni.projectcalico.org/podIPs",
		"k8s.v1.cni.cncf.io/network-status",
		"io.cilium/whatever",
		"jobset.sigs.k8s.io/jobset-uid",
	} {
		assert.NotContains(t, pod.Annotations, gone, "%s must be stripped", gone)
	}

	assert.Equal(t, "gb200", pod.Annotations["nvcrectl.nvidia.com/applied-overrides"],
		"annotations NVCRE sets on purpose must survive")
}

// Two reads of the same pod must produce the same stripped result even when
// one read happened before the CNI plugin annotated it. This is the case that
// made the real-cluster golden unusable.
func TestStripPodVolatileIsStableWhenCNIAnnotationsAreAbsent(t *testing.T) {
	withCNI := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "workload-node-0-0-abcde",
			Annotations: map[string]string{
				"cni.projectcalico.org/podIP": "172.29.112.176/32",
				"kubectl.kubernetes.io/x":     "keep",
			},
		},
	}
	withoutCNI := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "workload-node-0-0-zyxwv",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/x": "keep",
			},
		},
	}

	StripPodVolatile(&withCNI)
	StripPodVolatile(&withoutCNI)

	assert.Equal(t, withoutCNI.Annotations, withCNI.Annotations,
		"a pod read before and after CNI annotation must strip to the same thing")
}

// A pod with no annotations at all must not panic.
func TestStripPodVolatileNoAnnotations(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	assert.NotPanics(t, func() { StripPodVolatile(&pod) })
}
