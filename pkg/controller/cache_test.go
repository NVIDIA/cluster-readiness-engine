// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/podread"
)

// testJobSetNameLabel is the label JobSet stamps with the workload name.
const testJobSetNameLabel = "jobset.sigs.k8s.io/jobset-name"

// recordingCache stands in for the inner cache and counts the calls that
// reach it. Methods the guard does not override are never called here.
type recordingCache struct {
	cache.Cache
	reached int
}

func (c *recordingCache) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	c.reached++
	return nil
}

func (c *recordingCache) List(context.Context, client.ObjectList, ...client.ListOption) error {
	c.reached++
	return nil
}

func (c *recordingCache) GetInformer(context.Context, client.Object, ...cache.InformerGetOption) (cache.Informer, error) {
	c.reached++
	return nil, nil
}

func (c *recordingCache) GetInformerForKind(context.Context, schema.GroupVersionKind, ...cache.InformerGetOption) (cache.Informer, error) {
	c.reached++
	return nil, nil
}

func newTestScopedCache() (*scopedCache, *recordingCache) {
	inner := &recordingCache{}
	return &scopedCache{Cache: inner, scheme: clientgoscheme.Scheme}, inner
}

// TestCacheOptionsScopesPodsToJobLabel pins the Pod cache selector (ADR-102).
func TestCacheOptionsScopesPodsToJobLabel(t *testing.T) {
	for obj, byObject := range CacheOptions().ByObject {
		if _, ok := obj.(*corev1.Pod); ok {
			require.NotNil(t, byObject.Label)
			assert.Equal(t, podread.JobLabel, byObject.Label.String())
			assert.Nil(t, byObject.Transform, "a Pod Transform would replace the managedFields strip")
			return
		}
	}
	t.Fatal("CacheOptions has no Pod entry")
}

// TestEveryScopedCacheKindIsGuardedOrExempt fails when a kind is cached under
// a selector without being in guardedScopes or exemptScopes, so a kind scoped
// later cannot skip the guard silently (ADR-102).
func TestEveryScopedCacheKindIsGuardedOrExempt(t *testing.T) {
	for obj, byObject := range CacheOptions().ByObject {
		if byObject.Label == nil {
			continue
		}
		gvk, err := apiutil.GVKForObject(obj, clientgoscheme.Scheme)
		require.NoError(t, err)
		_, exempt := exemptScopes[gvk]
		assert.True(t, scopeForGVK(gvk) != nil || exempt,
			"%s is cached under a selector but is neither in guardedScopes nor in exemptScopes", gvk.Kind)
	}
}

// mustParseSelector parses a label selector for a test case.
func mustParseSelector(t *testing.T, s string) labels.Selector {
	t.Helper()
	sel, err := labels.Parse(s)
	require.NoError(t, err)
	return sel
}

// TestScopedCacheList pins which Pod Lists the cache answers: those whose
// options guarantee the NVCRE job label.
func TestScopedCacheList(t *testing.T) {
	const job = podread.JobLabel
	tests := []struct {
		name   string
		opts   func(t *testing.T) []client.ListOption
		scoped bool
	}{
		{"job label value", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.InNamespace("ns"), client.MatchingLabels{job: "j"}}
		}, true},
		{"job label exists", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.HasLabels{job}}
		}, true},
		{"job label in a set", func(t *testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingLabelsSelector{Selector: mustParseSelector(t, job+" in (a,b)")}}
		}, true},
		{"job field index", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingFields{podread.JobIndexField: "j"}}
		}, true},
		{"jobset-name with the job label", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingLabels{testJobSetNameLabel: "w"}, client.HasLabels{job}}
		}, true},
		{"node index with the job label", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingFields{podread.NodeNameIndexField: "n"}, client.HasLabels{job}}
		}, true},
		{"no options", func(*testing.T) []client.ListOption { return nil }, false},
		{"namespace only", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.InNamespace("kube-system")}
		}, false},
		{"jobset-name only", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingLabels{testJobSetNameLabel: "w"}}
		}, false},
		{"node index only", func(*testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingFields{podread.NodeNameIndexField: "n"}}
		}, false},
		{"job label absent", func(t *testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingLabelsSelector{Selector: mustParseSelector(t, "!"+job)}}
		}, false},
		{"job label not equal", func(t *testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingLabelsSelector{Selector: mustParseSelector(t, job+"!=j")}}
		}, false},
		{"job label not in a set", func(t *testing.T) []client.ListOption {
			return []client.ListOption{client.MatchingLabelsSelector{Selector: mustParseSelector(t, job+" notin (a)")}}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, inner := newTestScopedCache()
			err := c.List(context.Background(), &corev1.PodList{}, tt.opts(t)...)
			if tt.scoped {
				assert.NoError(t, err)
				assert.Equal(t, 1, inner.reached)
			} else {
				assert.ErrorIs(t, err, errUnscopedCacheRead)
				assert.Equal(t, 0, inner.reached)
			}
		})
	}
}

// TestScopedCacheRefusesByKind pins the reads refused for Pods whatever their
// Go type, and that other kinds, including exempt Nodes, pass untouched.
func TestScopedCacheRefusesByKind(t *testing.T) {
	ctx := context.Background()
	podGVK := corev1.SchemeGroupVersion.WithKind("Pod")
	podListGVK := corev1.SchemeGroupVersion.WithKind("PodList")

	metadataPods := &metav1.PartialObjectMetadataList{}
	metadataPods.SetGroupVersionKind(podListGVK)
	unstructuredPods := &unstructured.UnstructuredList{}
	unstructuredPods.SetGroupVersionKind(podListGVK)

	refused := []struct {
		name string
		call func(c *scopedCache) error
	}{
		{"Pod Get", func(c *scopedCache) error {
			return c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "p"}, &corev1.Pod{})
		}},
		{"Pod informer", func(c *scopedCache) error {
			_, err := c.GetInformer(ctx, &corev1.Pod{})
			return err
		}},
		{"Pod informer by kind", func(c *scopedCache) error {
			_, err := c.GetInformerForKind(ctx, podGVK)
			return err
		}},
		{"metadata-only Pod list", func(c *scopedCache) error {
			return c.List(ctx, metadataPods, client.InNamespace("ns"))
		}},
		{"unstructured Pod list", func(c *scopedCache) error {
			return c.List(ctx, unstructuredPods, client.InNamespace("ns"))
		}},
	}
	for _, tt := range refused {
		t.Run("refuses "+tt.name, func(t *testing.T) {
			c, inner := newTestScopedCache()
			before := UnscopedCacheReads()
			assert.ErrorIs(t, tt.call(c), errUnscopedCacheRead)
			assert.Equal(t, 0, inner.reached)
			assert.Equal(t, before+1, UnscopedCacheReads())
		})
	}

	passed := []struct {
		name string
		call func(c *scopedCache) error
	}{
		{"Node Get", func(c *scopedCache) error {
			return c.Get(ctx, types.NamespacedName{Name: "n"}, &corev1.Node{})
		}},
		{"Node list without a selector", func(c *scopedCache) error {
			return c.List(ctx, &corev1.NodeList{})
		}},
		{"Node informer", func(c *scopedCache) error {
			_, err := c.GetInformer(ctx, &corev1.Node{})
			return err
		}},
		{"PersistentVolume list", func(c *scopedCache) error {
			return c.List(ctx, &corev1.PersistentVolumeList{})
		}},
	}
	for _, tt := range passed {
		t.Run("passes "+tt.name, func(t *testing.T) {
			c, inner := newTestScopedCache()
			assert.NoError(t, tt.call(c))
			assert.Equal(t, 1, inner.reached)
		})
	}
}
