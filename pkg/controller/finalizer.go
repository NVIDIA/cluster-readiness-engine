// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// ensureFinalizer adds finalizer to obj if it is not already present, reporting
// whether a write was issued.
//
// The write is a merge patch rather than a full Update. Update sends the whole
// object, so a reconciler that read from a slightly stale cache can revert a
// concurrent change to a field it never touched; a patch limited to the
// finalizer list cannot. Three of the reconcilers previously used Update here
// and two used Patch — this makes the safer form uniform.
//
// When this returns (true, nil) the caller should return from Reconcile without
// further work: the patch bumps resourceVersion, which delivers a watch event
// that drives the next reconcile against the updated object.
func ensureFinalizer[T client.Object](ctx context.Context, c client.Client, obj T, finalizer string) (bool, error) {
	if controllerutil.ContainsFinalizer(obj, finalizer) {
		return false, nil
	}

	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	controllerutil.AddFinalizer(obj, finalizer)
	if err := c.Patch(ctx, obj, patch); err != nil {
		return false, fmt.Errorf("failed to add finalizer %s: %w", finalizer, err)
	}

	logf.FromContext(ctx).Info("Added finalizer", "finalizer", finalizer, "name", obj.GetName())
	return true, nil
}
