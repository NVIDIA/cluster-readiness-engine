// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// resetDepsFixture builds the [deps] reset phase against a fake helm binary
// on PATH and a fake cluster holding one Trainer CRD and the shared JobSet
// CRD, counting every CRD delete the phase issues.
func resetDepsFixture(t *testing.T, helmScript string) (setupPhaseParams, *int, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "helm"), []byte(helmScript), 0o755))
	t.Setenv("PATH", dir)

	crd := func(name, group string) *apiextv1.CustomResourceDefinition {
		return &apiextv1.CustomResourceDefinition{
			Name: name,
			Spec: apiextv1.CustomResourceDefinitionSpec{Group: group},
		}
	}
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).
		WithObjects(crd("trainjobs."+trainerAPIGroup, trainerAPIGroup), crd(jobSetCRDName, jobsetAPIGroup)).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, underlying client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++
				return underlying.Delete(ctx, obj, opts...)
			},
		}).Build()
	var out bytes.Buffer
	return setupPhaseParams{
		ctx: context.Background(), c: c, skip: map[string]bool{},
		kubeconfig: "/tmp/test-kubeconfig", kubeContext: "test-context", out: &out,
	}, &deletes, &out
}

// TestUninstallDepsPhaseStopsBeforeCRDCleanupOnUninstallFailure pins ADR-078
// decision 3: a failed Trainer Helm uninstall propagates with its diagnostics
// and reset stops before any Trainer CRD deletion.
func TestUninstallDepsPhaseStopsBeforeCRDCleanupOnUninstallFailure(t *testing.T) {
	sp, deletes, out := resetDepsFixture(t, "#!/bin/sh\nprintf 'simulated uninstall timeout\\n' >&2\nexit 1\n")
	err := uninstallDepsPhase(sp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outcome may be partial")
	assert.Contains(t, out.String(), "simulated uninstall timeout")
	assert.Zero(t, *deletes, "no CRD deletion may follow a failed uninstall")
	assert.NotContains(t, out.String(), "Removing Kubeflow Trainer CRDs")
	assert.True(t, crdPresent(t, sp.c, "trainjobs."+trainerAPIGroup))
}

// TestUninstallDepsPhaseRetainsJobSetCRD pins that a successful reset removes
// the Trainer-owned CRDs and never touches the shared JobSet CRD.
func TestUninstallDepsPhaseRetainsJobSetCRD(t *testing.T) {
	sp, deletes, out := resetDepsFixture(t, "#!/bin/sh\nexit 0\n")
	require.NoError(t, uninstallDepsPhase(sp))
	assert.Equal(t, 1, *deletes)
	assert.Contains(t, out.String(), "Removing Kubeflow Trainer CRDs")
	assert.False(t, crdPresent(t, sp.c, "trainjobs."+trainerAPIGroup))
	assert.True(t, crdPresent(t, sp.c, jobSetCRDName))
}

// TestUnservedCRDBlocksRecoveryButNotResetCR pins the split between the two
// discovery callers. Kubernetes requires exactly one storage version but no
// served one, so a half-applied or deliberately disabled CRD can reach this
// state. Recovery must fail closed on it, because it cannot enumerate the
// objects it is about to destroy. Reset's [cr] phase must not: it is an
// explicit operator cleanup, and the later [helm] phase removes the CRD
// either way (ADR-078 decisions 3 and 4).
func TestUnservedCRDBlocksRecoveryButNotResetCR(t *testing.T) {
	unserved := func(name, group, kind, plural string) *apiextv1.CustomResourceDefinition {
		return &apiextv1.CustomResourceDefinition{
			Name: name,
			Spec: apiextv1.CustomResourceDefinitionSpec{
				Group: group,
				Names: apiextv1.CustomResourceDefinitionNames{Kind: kind, Plural: plural},
				Versions: []apiextv1.CustomResourceDefinitionVersion{
					{Name: "v1alpha1", Served: false, Storage: true},
				},
			},
		}
	}
	c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(
		unserved("certifications."+nvcreAPIGroup, nvcreAPIGroup, "Certification", "certifications"),
		unserved("trainjobs."+trainerAPIGroup, trainerAPIGroup, "TrainJob", "trainjobs"),
	).Build()
	ctx := context.Background()

	_, err := discoverServedResourcesByGroup(ctx, c, trainerAPIGroup)
	require.Error(t, err, "recovery discovery must fail closed on an unserved CRD")
	assert.Contains(t, err.Error(), "has no served version")

	resources, err := discoverNVCREResources(ctx, c)
	require.NoError(t, err, "reset's [cr] phase must tolerate an unserved CRD")
	assert.Empty(t, resources, "an unserved CRD exposes no version to enumerate")

	var out bytes.Buffer
	require.NoError(t, deleteNVCRECRs(ctx, c, &out))
}

func crdPresent(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Name: name}, &apiextv1.CustomResourceDefinition{})
	if client.IgnoreNotFound(err) != nil {
		t.Fatalf("get CRD %s: %v", name, err)
	}
	return err == nil
}
