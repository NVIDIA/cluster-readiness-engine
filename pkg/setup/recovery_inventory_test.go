// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//nolint:goconst // Inventory fixtures intentionally repeat Kubernetes field and kind literals.
package setup

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestTrainerHelmStorageIdentity(t *testing.T) {
	encode := func(payload string, compressed bool) string {
		t.Helper()
		data := []byte(payload)
		if compressed {
			var buf bytes.Buffer
			writer := gzip.NewWriter(&buf)
			_, err := writer.Write(data)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			data = buf.Bytes()
		}
		return base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(data)))
	}
	const release = `{"name":"kubeflow-trainer","namespace":"kubeflow-system","version":1,"info":{"status":"failed"}}`
	base := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "helm.sh/release.v1",
		"metadata": map[string]any{
			"name": "sh.helm.release.v1.kubeflow-trainer.v1", "namespace": trainerNamespace,
			"labels": map[string]any{"owner": "helm", "name": trainerReleaseName, "version": "1", "status": "failed"},
		},
		"data": map[string]any{"release": encode(release, true)},
	}}
	for _, tc := range []struct {
		name    string
		path    []string
		value   string
		allowed bool
	}{
		{"compressed", nil, "", true},
		{"legacy", []string{"data", "release"}, encode(release, false), true},
		{"arbitrary-suffix", []string{"metadata", "name"}, "sh.helm.release.v1.kubeflow-trainer.vbackup", false},
		{"wrong-namespace", []string{"metadata", "namespace"}, "other", false},
		{"wrong-revision", []string{"metadata", "labels", "version"}, "2", false},
		{"noncanonical-revision", []string{"metadata", "labels", "version"}, "01", false},
		{"wrong-status", []string{"metadata", "labels", "status"}, "deployed", false},
		{"arbitrary-data", []string{"data", "release"}, "dXNlci1kYXRh", false},
		{"invalid-base64", []string{"data", "release"}, "!", false},
		{"empty-release", []string{"data", "release"}, encode("{}", true), false},
		{"payload-name", []string{"data", "release"}, encode(`{"name":"other","namespace":"kubeflow-system","version":1,"info":{"status":"failed"}}`, true), false},
		{"payload-namespace", []string{"data", "release"}, encode(`{"name":"kubeflow-trainer","namespace":"other","version":1,"info":{"status":"failed"}}`, true), false},
		{"payload-revision", []string{"data", "release"}, encode(`{"name":"kubeflow-trainer","namespace":"kubeflow-system","version":2,"info":{"status":"failed"}}`, true), false},
		{"invalid-json", []string{"data", "release"}, encode("{", true), false},
		{"truncated-gzip", []string{"data", "release"}, encode(string([]byte{0x1f, 0x8b, 0x08}), false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			object := base.DeepCopy()
			if tc.path != nil {
				require.NoError(t, unstructured.SetNestedField(object.Object, tc.value, tc.path...))
			}
			allowed, _ := directlyAllowedRecoveryObject(setupPhaseParams{}, *object)
			assert.Equal(t, tc.allowed, allowed)
		})
	}
}

func TestPriorLeaseEvidenceSurvivesOwnerCleanupAndRenewal(t *testing.T) {
	lease := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "coordination.k8s.io/v1",
		"kind":       "Lease",
		"metadata": map[string]any{
			"name": "trainer.kubeflow.org", "namespace": trainerNamespace, "uid": "lease-uid",
		},
		"spec": map[string]any{
			"holderIdentity":       "kubeflow-trainer-controller-manager-abc_pod-uuid",
			"leaseDurationSeconds": int64(15), "renewTime": "2026-09-14T00:00:00.000000Z",
		},
	}}
	lease.SetGroupVersionKind(schema.GroupVersionKind{Group: "coordination.k8s.io", Version: "v1", Kind: "Lease"})
	classification := "verified-controller-lease:" + stableLeaseClassification(lease, "kubeflow-trainer-controller-manager-abc")
	prior := &recoveryEvidence{objects: map[string]recoveryObjectEvidence{
		recoveryIdentity(lease): objectEvidence(lease, classification),
	}}

	renewed := *lease.DeepCopy()
	_ = unstructured.SetNestedField(renewed.Object, "2026-09-14T00:01:00.000000Z", "spec", "renewTime")
	assert.True(t, matchesPriorEvidence(prior, renewed), "renewal is volatile and the owner Pod may already be gone")

	_ = unstructured.SetNestedField(renewed.Object, "different-controller_pod-uuid", "spec", "holderIdentity")
	assert.False(t, matchesPriorEvidence(prior, renewed), "changing the verified holder Pod invalidates the evidence")

	// Exercise the full collection path after the holder Pod was removed.
	_ = unstructured.SetNestedField(renewed.Object, "kubeflow-trainer-controller-manager-abc_pod-uuid", "spec", "holderIdentity")
	namespace := &unstructured.Unstructured{}
	namespace.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	namespace.SetName(trainerNamespace)
	namespace.SetUID("namespace-uid")
	c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(namespace, &renewed).Build()
	evidence, blockers := collectRecoveryEvidence(setupPhaseParams{
		ctx: context.Background(), c: c,
		discoverNamespaced: func() ([]*metav1.APIResourceList, error) {
			return []*metav1.APIResourceList{{GroupVersion: "coordination.k8s.io/v1", APIResources: []metav1.APIResource{
				{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: metav1.Verbs{"list"}},
			}}}, nil
		},
	}, prior, false)
	assert.Empty(t, blockers)
	assert.Contains(t, evidence.objects, recoveryIdentity(renewed))
}

func TestCollectRecoveryEvidenceRejectsInvalidOwnerChains(t *testing.T) {
	tests := map[string][]unstructured.Unstructured{
		"wrong uid": {protectedConfigMap("child", "child-uid", metav1.OwnerReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "manager", UID: "wrong-uid", Controller: new(true),
		})},
		"dangling owner": {protectedConfigMap("child", "child-uid", metav1.OwnerReference{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "missing", UID: "missing-uid", Controller: new(true),
		})},
		"cyclic owners": {
			protectedConfigMap("a", "a-uid", metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "b", UID: "b-uid", Controller: new(true)}),
			protectedConfigMap("b", "b-uid", metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "a", UID: "a-uid", Controller: new(true)}),
		},
	}
	for name, objects := range tests {
		t.Run(name, func(t *testing.T) {
			namespace := namespaceObject("namespace-uid")
			deployment := manifestDeployment()
			clientObjects := []client.Object{namespace, &deployment}
			for i := range objects {
				clientObjects = append(clientObjects, &objects[i])
			}
			c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(clientObjects...).Build()
			_, blockers := collectRecoveryEvidence(setupPhaseParams{
				ctx: context.Background(), c: c,
				releaseManifestObjects: map[string]struct{}{manifestIdentity("apps/v1", "Deployment", trainerNamespace, "manager"): {}},
				discoverNamespaced: func() ([]*metav1.APIResourceList, error) {
					return []*metav1.APIResourceList{
						{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: metav1.Verbs{"list"}}}},
						{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: metav1.Verbs{"list"}}}},
					}, nil
				},
			}, nil, false)
			assert.NotEmpty(t, blockers)
			assert.Contains(t, fmt.Sprint(blockers), "not a verified Trainer release resource or UID-linked descendant")
		})
	}
}

func TestCollectRecoveryEvidenceNeverListsExcludedAPIs(t *testing.T) {
	lists := 0
	c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(namespaceObject("namespace-uid")).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, underlying client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists++
			return underlying.List(ctx, list, opts...)
		}}).Build()
	_, blockers := collectRecoveryEvidence(setupPhaseParams{
		ctx: context.Background(), c: c,
		discoverNamespaced: func() ([]*metav1.APIResourceList, error) {
			return []*metav1.APIResourceList{
				{GroupVersion: "v1", APIResources: []metav1.APIResource{
					{Name: "events", Kind: "Event", Namespaced: true, Verbs: metav1.Verbs{"list"}},
					{Name: "endpoints", Kind: "Endpoints", Namespaced: true, Verbs: metav1.Verbs{"list"}},
				}},
				{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "events", Kind: "Event", Namespaced: true, Verbs: metav1.Verbs{"list"}}}},
				{GroupVersion: "discovery.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "endpointslices", Kind: "EndpointSlice", Namespaced: true, Verbs: metav1.Verbs{"list"}}}},
			}, nil
		},
	}, nil, false)
	assert.Empty(t, blockers)
	assert.Zero(t, lists)
}

func TestCompareRecoveryEvidenceRejectsClassificationAndUIDChanges(t *testing.T) {
	identity := "v1/ConfigMap/kubeflow-system/release-data"
	previous := recoveryEvidence{namespaceUID: "namespace-uid", resources: map[string]bool{"v1/configmaps": true}, objects: map[string]recoveryObjectEvidence{
		identity: {uid: "object-uid", classification: "helm-release-data:old"},
	}}

	changedClassification := recoveryEvidence{namespaceUID: "namespace-uid", resources: map[string]bool{"v1/configmaps": true}, objects: map[string]recoveryObjectEvidence{
		identity: {uid: "object-uid", classification: "helm-release-data:new"},
	}}
	assert.Contains(t, compareRecoveryEvidence(previous, changedClassification, false),
		"changed classification evidence for protected object: "+identity)

	replaced := recoveryEvidence{namespaceUID: "namespace-uid", resources: map[string]bool{"v1/configmaps": true}, objects: map[string]recoveryObjectEvidence{
		identity: {uid: "replacement-uid", classification: "helm-release-data:old"},
	}}
	assert.Contains(t, compareRecoveryEvidence(previous, replaced, false),
		"replacement UID for protected object: "+identity)
}

func TestTrainerWorkloadGateExternalJobSetScope(t *testing.T) {
	jobSetCRD := unstructured.Unstructured{Object: map[string]any{"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition", "metadata": map[string]any{"name": "jobsets.jobset.x-k8s.io"}, "spec": map[string]any{"group": jobsetAPIGroup, "names": map[string]any{"plural": "jobsets", "kind": "JobSet"}, "scope": "Namespaced", "versions": []any{map[string]any{"name": "v1alpha2", "served": true, "storage": true}}}}}
	external := workloadObject("jobset.x-k8s.io/v1alpha2", "JobSet", "external-system", "external-jobset")
	insideTrainerNamespace := workloadObject("jobset.x-k8s.io/v1alpha2", "JobSet", trainerNamespace, "trainer-jobset")

	t.Run("external namespace is retained", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(&jobSetCRD, &external).Build()
		safe, blockers := trainerWorkloadGate(setupPhaseParams{ctx: context.Background(), c: c, jobSetMode: jobSetModeExternal})
		assert.True(t, safe)
		assert.Empty(t, blockers)
	})

	t.Run("trainer namespace would be deleted", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(newSetupScheme(t)).WithObjects(&jobSetCRD, &insideTrainerNamespace).Build()
		safe, blockers := trainerWorkloadGate(setupPhaseParams{ctx: context.Background(), c: c, jobSetMode: jobSetModeExternal})
		assert.False(t, safe)
		assert.Contains(t, fmt.Sprint(blockers), "kubeflow-system/trainer-jobset")
	})
}

func namespaceObject(uid string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	object.SetName(trainerNamespace)
	object.SetUID(types.UID(uid))
	return object
}

func manifestDeployment() unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{
			"name": "manager", "namespace": trainerNamespace, "uid": "manager-uid",
			"labels": map[string]any{"app.kubernetes.io/managed-by": "Helm"},
			"annotations": map[string]any{
				"meta.helm.sh/release-name": trainerReleaseName, "meta.helm.sh/release-namespace": trainerNamespace,
			},
		},
	}}
	return object
}

func protectedConfigMap(name, uid string, owner metav1.OwnerReference) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": name, "namespace": trainerNamespace, "uid": uid},
	}}
	object.SetOwnerReferences([]metav1.OwnerReference{owner})
	return object
}

func workloadObject(apiVersion, kind, namespace, name string) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"namespace": namespace, "name": name},
	}}
	return object
}
