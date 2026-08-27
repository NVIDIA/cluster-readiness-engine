// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	crev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// A Certification that finds no schedulable nodes retries for up to
// nodeDiscoveryTimeout. That retry has to say so on the status: it used to log
// only, so the CR carried no conditions and an operator watching it saw nothing
// at all until the CLI timed out.
func TestWaitingForNodes(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "waiting-for-nodes",
		ExpectedSuffix: ".json",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			NodeSelector map[string]string `yaml:"nodeSelector"`
			Nodes        []string          `yaml:"nodes"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}

		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			return err
		}
		if err := crev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}

		cert := &crev1alpha1.Certification{
			ObjectMeta: metav1.ObjectMeta{
				Name: "c", Namespace: "ns",
				CreationTimestamp: metav1.Now(),
				Finalizers:        []string{certificationFinalizer},
			},
			Spec: crev1alpha1.CertificationSpec{
				Target: crev1alpha1.TargetSpec{NodeSelector: in.NodeSelector},
				Categories: []crev1alpha1.CertificateCategory{
					{Domain: "communication", Variant: "nccl-all-reduce"},
				},
			},
		}

		builder := fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(cert).WithStatusSubresource(cert)
		for _, n := range in.Nodes {
			builder = builder.WithObjects(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
		c := builder.Build()

		r := &CertificationReconciler{Client: c, Scheme: scheme}
		_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "c", Namespace: "ns"}})

		got := &crev1alpha1.Certification{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: "c", Namespace: "ns"}, got); err != nil {
			return err
		}

		type condOut struct {
			Type             string `json:"type"`
			Status           string `json:"status"`
			Reason           string `json:"reason"`
			MentionsSelector bool   `json:"messageNamesSelector"`
		}
		out := struct {
			Conditions []condOut `json:"conditions"`
		}{}
		for _, cd := range got.Status.Conditions {
			out.Conditions = append(out.Conditions, condOut{
				Type: cd.Type, Status: string(cd.Status), Reason: cd.Reason,
				MentionsSelector: strings.Contains(cd.Message, "nvidia.com/gpu.present"),
			})
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}
