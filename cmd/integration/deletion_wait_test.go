// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	kindConfigMap      = "ConfigMap"
	resourceConfigMaps = "configmaps"
	deletionProbeName  = "deletion-probe"
)

func TestReadObjectPreservesErrors(t *testing.T) {
	spec := collectSpec{Kind: kindConfigMap, Name: deletionProbeName, Namespace: corev1.NamespaceDefault}
	c := fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
		Name: spec.Name, Namespace: spec.Namespace}).Build()
	obj, err := readObject(context.Background(), c, spec)
	require.NoError(t, err)
	require.NotNil(t, obj)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readObject(ctx, c, spec)
	require.ErrorIs(t, err, context.Canceled)
	spec.Name = "absent-probe"
	_, err = readObject(context.Background(), c, spec)
	require.True(t, apierrors.IsNotFound(err))
	for _, readErr := range []error{
		context.DeadlineExceeded,
		apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, spec.Name, errors.New("denied")),
		errors.New("transport unavailable"),
	} {
		failing := interceptor.NewClient(c, interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				return readErr
			},
		})
		_, err = readObject(context.Background(), failing, spec)
		require.ErrorIs(t, err, readErr)
	}
}

func TestWaitForDeletionPropagatesDeadline(t *testing.T) {
	deadline := time.Now().Add(5 * time.Second)
	var observed time.Time
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey,
			_ client.Object, _ ...client.GetOption) error {
			observed, _ = ctx.Deadline()
			return apierrors.NewNotFound(schema.GroupResource{Resource: resourceConfigMaps}, key.Name)
		},
	}).Build()
	waitForDeletion(t, c, waitConfig{TimeoutSeconds: 2, WaitForDeletion: []collectSpec{{
		Kind: kindConfigMap, Name: deletionProbeName, Namespace: corev1.NamespaceDefault,
	}}}, deadline)
	require.Equal(t, deadline, observed, "the deletion lookup must inherit the case deadline")
}

// Exercise the fatal wait path in a subprocess. Neither a failed read nor a
// context expiring during a read may satisfy the deletion predicate.
func TestWaitForDeletionRejectsReadFailures(t *testing.T) {
	if mode := os.Getenv("CRE_TEST_DELETION_ERROR"); mode != "" {
		c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey,
				_ client.Object, _ ...client.GetOption) error {
				if mode == "deadline" {
					<-ctx.Done()
					return ctx.Err()
				}
				return apierrors.NewForbidden(schema.GroupResource{Resource: resourceConfigMaps}, key.Name, errors.New("denied"))
			},
		}).Build()
		waitForDeletion(t, c, waitConfig{TimeoutSeconds: 1, WaitForDeletion: []collectSpec{{
			Kind: kindConfigMap, Name: deletionProbeName, Namespace: corev1.NamespaceDefault,
		}}}, time.Now().Add(200*time.Millisecond))
		return
	}
	for _, mode := range []string{"deadline", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWaitForDeletionRejectsReadFailures$")
			cmd.Env = append(os.Environ(), "CRE_TEST_DELETION_ERROR="+mode)
			output, err := cmd.CombinedOutput()
			require.NoError(t, ctx.Err(), "child must fail, not hang")
			require.Error(t, err)
			require.Contains(t, string(output), "timed out waiting for deletion")
			require.NotContains(t, string(output), "panic:")
		})
	}
}
