// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	sigsyaml "sigs.k8s.io/yaml"
)

func newSetupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(s))
	require.NoError(t, apiextv1.AddToScheme(s))
	// LogProfile has no typed Go package here, so register the list kind the
	// check function asks for.
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: creAPIGroup, Version: "v1alpha1", Kind: "LogProfile"},
		&unstructured.Unstructured{})
	s.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: creAPIGroup, Version: "v1alpha1", Kind: "LogProfileList"},
		&unstructured.UnstructuredList{})
	return s
}

func TestCheckDCGM(t *testing.T) {
	t.Run("present when the gpu-operator service exists", func(t *testing.T) {
		c := fake.NewClientBuilder().
			WithScheme(newSetupScheme(t)).
			WithObjects(&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dcgmServiceName,
					Namespace: dcgmServiceNamespace,
				},
			}).
			Build()
		assert.NoError(t, checkDCGM(context.Background(), c))
	})

	t.Run("absent when no service exists", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).Build()
		assert.True(t, apierrors.IsNotFound(checkDCGM(context.Background(), c)))
	})

	t.Run("absent when the service is in another namespace", func(t *testing.T) {
		c := fake.NewClientBuilder().
			WithScheme(newSetupScheme(t)).
			WithObjects(&corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dcgmServiceName,
					Namespace: "default",
				},
			}).
			Build()
		assert.True(t, apierrors.IsNotFound(checkDCGM(context.Background(), c)))
	})

	t.Run("a denied lookup is not reported as absent", func(t *testing.T) {
		status := collectSetupStatus(context.Background(), forbiddenServiceClient(t), allDeployedHelmQuery)
		assert.False(t, status.Components.DCGM)
		assert.False(t, status.dcgmAbsent, "a denied lookup must not print the patch command")
	})
}

// forbiddenServiceClient answers every Service read with a Forbidden error, the
// way an API server does when the user may not read the gpu-operator namespace.
func forbiddenServiceClient(t *testing.T) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newSetupScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey,
				obj client.Object, _ ...client.GetOption) error {
				return apierrors.NewForbidden(
					schema.GroupResource{Resource: "services"}, key.Name, errors.New("denied"))
			},
		}).
		Build()
}

// crd builds a CustomResourceDefinition that the check functions can read.
func crd(name, group, kind string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"group": group,
			"names": map[string]any{"kind": kind},
		},
	}}
	u.SetAPIVersion("apiextensions.k8s.io/v1")
	u.SetKind("CustomResourceDefinition")
	u.SetName(name)
	return u
}

// logProfile builds a LogProfile that checkLogProfiles can count.
func logProfile(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	u.SetAPIVersion(creAPIGroup + "/v1alpha1")
	u.SetKind("LogProfile")
	u.SetName(name)
	return u
}

// readyClusterObjects returns every object needed for installed to be true,
// with no DCGM service.
func readyClusterObjects() []client.Object {
	return []client.Object{
		crd("certifications."+creAPIGroup, creAPIGroup, "Certification"),
		crd("trainjobs."+trainerAPIGroup, trainerAPIGroup, "TrainJob"),
		logProfile("megatron-training"),
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "cre-controller", Namespace: creNamespace},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "gpu-node-0",
				Labels: map[string]string{"nvidia.com/gpu.present": "true"},
			},
		},
	}
}

// A missing DCGM service must not make the cluster look unready, because only
// the diagnostics/dcgm-level4 category needs it.
func TestCollectSetupStatusDCGMIsOptional(t *testing.T) {
	objs := readyClusterObjects()

	t.Run("ready without dcgm", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(objs...).Build()
		s := collectSetupStatus(context.Background(), c, allDeployedHelmQuery)
		assert.True(t, s.Installed, "installed must not depend on DCGM")
		assert.False(t, s.Components.DCGM)
	})

	t.Run("ready with dcgm", func(t *testing.T) {
		withDCGM := append(objs, &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: dcgmServiceName, Namespace: dcgmServiceNamespace},
		})
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(withDCGM...).Build()
		s := collectSetupStatus(context.Background(), c, allDeployedHelmQuery)
		assert.True(t, s.Installed)
		assert.True(t, s.Components.DCGM)
	})
}

// allDeployedHelmQuery reports every managed Helm release as deployed.
func allDeployedHelmQuery(string, string) string { return helmStateDeployed }

// TestSetupStatusHelmReleases drives collectSetupStatus and printSetupStatus
// against a ready cluster with the Helm release states given in input.yaml.
// The golden file holds the JSON status followed by the rendered table, so a
// failed release flipping 'installed' shows up in both output formats.
func TestSetupStatusHelmReleases(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "setup-status-helm",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			// States maps a managed release name to the state the helm stub
			// reports. A missing key reads as a release helm has no record of.
			States map[string]string `yaml:"states"`
		}
		if err := sigsyaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		query := func(release, _ string) string {
			if state, ok := in.States[release]; ok {
				return state
			}
			return helmStateNotInstalled
		}

		c := fake.NewClientBuilder().
			WithScheme(newSetupScheme(t)).
			WithObjects(readyClusterObjects()...).
			Build()
		s := collectSetupStatus(context.Background(), c, query)

		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(s); err != nil {
			return err
		}
		out.WriteString("\n")
		printSetupStatus(&out, s)

		tc.Actual = out.String()
		return nil
	})
}
