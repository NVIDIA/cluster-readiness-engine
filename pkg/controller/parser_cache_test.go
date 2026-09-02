// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// logProfile builds a minimal LogProfile carrying the given resourceVersion and
// trainingStep regex.
func logProfile(name, resourceVersion, regex string) *nvcrev1alpha1.LogProfile {
	return &nvcrev1alpha1.LogProfile{
		Name:            name,
		ResourceVersion: resourceVersion,
		Spec: nvcrev1alpha1.LogProfileSpec{
			Timestamp: nvcrev1alpha1.TimestampSpec{Layout: "2006-01-02 15:04:05"},
			Patterns: nvcrev1alpha1.LogPatternSet{
				TrainingStep: &nvcrev1alpha1.EventPattern{Regex: regex},
			},
		},
	}
}

// TestGoodputParserCacheInvalidatesOnResourceVersion verifies that editing a
// LogProfile takes effect without a controller restart. Keying the cache on name
// alone made pattern edits invisible until the process was recycled.
func TestGoodputParserCacheInvalidatesOnResourceVersion(t *testing.T) {
	tests := []struct {
		name          string
		second        *nvcrev1alpha1.LogProfile
		wantSameCache bool
	}{
		{
			name:          "unchanged resourceVersion reuses the cached parser",
			second:        logProfile("megatron", "1", `iteration (?P<globalStep>\d+)`),
			wantSameCache: true,
		},
		{
			name:          "bumped resourceVersion recompiles the parser",
			second:        logProfile("megatron", "2", `step (?P<globalStep>\d+)`),
			wantSameCache: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &GoodputMeasurementReconciler{}

			first := logProfile("megatron", "1", `iteration (?P<globalStep>\d+)`)
			p1, err := r.getOrCreateParser(first)
			require.NoError(t, err)
			require.NotNil(t, p1)

			p2, err := r.getOrCreateParser(tt.second)
			require.NoError(t, err)
			require.NotNil(t, p2)

			if tt.wantSameCache {
				require.Same(t, p1, p2, "expected the cached parser to be reused")
			} else {
				require.NotSame(t, p1, p2, "expected a recompiled parser after the LogProfile changed")
			}

			// Only one entry is retained per profile name, so the cache does not
			// grow with every edit.
			require.Len(t, r.parsers, 1)
			require.Equal(t, tt.second.ResourceVersion, r.parsers["megatron"].resourceVersion)
		})
	}
}

// TestBandwidthParserCacheInvalidatesOnResourceVersion is the BandwidthMeasurement
// counterpart: the same cache-keying bug existed in both controllers.
func TestBandwidthParserCacheInvalidatesOnResourceVersion(t *testing.T) {
	ncclProfile := func(resourceVersion, regex string) *nvcrev1alpha1.LogProfile {
		p := logProfile("nccl", resourceVersion, `iteration (?P<globalStep>\d+)`)
		p.Spec.Patterns.BandwidthResult = &nvcrev1alpha1.EventPattern{Regex: regex}
		return p
	}

	const (
		regexV1 = `(?P<size>\d+)\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`
		regexV2 = `(?P<size>\d+)\s+\S+\s+(?P<algBW>[\d.]+)\s+(?P<busBW>[\d.]+)`
	)

	// The fake client owns resourceVersion assignment, so the "changed" case
	// mutates the stored object and lets the client bump it, exactly as the API
	// server would.
	tests := []struct {
		name          string
		editProfile   bool
		wantSameCache bool
	}{
		{
			name:          "unchanged LogProfile reuses the cached parser",
			editProfile:   false,
			wantSameCache: true,
		},
		{
			name:          "edited LogProfile recompiles the parser",
			editProfile:   true,
			wantSameCache: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			scheme := runtime.NewScheme()
			require.NoError(t, nvcrev1alpha1.AddToScheme(scheme))

			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(ncclProfile("", regexV1)).Build()
			r := &BandwidthMeasurementReconciler{Client: c}

			p1, _, err := r.getOrCreateParser(ctx, "nccl")
			require.NoError(t, err)
			require.NotNil(t, p1)

			if tt.editProfile {
				stored := &nvcrev1alpha1.LogProfile{}
				require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "nccl"}, stored))
				stored.Spec.Patterns.BandwidthResult.Regex = regexV2
				require.NoError(t, c.Update(ctx, stored))
			}

			p2, profile, err := r.getOrCreateParser(ctx, "nccl")
			require.NoError(t, err)
			require.NotNil(t, p2)
			require.NotNil(t, profile, "the profile is always returned, cache hit or miss")

			if tt.wantSameCache {
				require.Same(t, p1, p2, "expected the cached parser to be reused")
			} else {
				require.NotSame(t, p1, p2, "expected a recompiled parser after the LogProfile changed")
			}

			require.Len(t, r.parsers, 1)
		})
	}
}
