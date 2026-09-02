// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GPUNodeLabel marks a node as GPU-equipped. The controllers only ever operate
// on GPU nodes — discoverTargetNodes filters on this label before a node can be
// partitioned into a group — so it also defines the Node cache boundary.
const GPUNodeLabel = "nvidia.com/gpu.present"

// present is the label value indicating a node has the GPUNodeLabel set.
const present = "true"

// CacheOptions returns the informer cache configuration for the manager.
//
// Without it, controller-runtime caches every object of every watched kind,
// cluster-wide and in full. This controller watches Nodes, Pods and
// PersistentVolumes, so on a large GPU cluster the default cache holds the
// entire Pod population of the cluster in the controller's heap — against a
// 1Gi limit. Two reductions are applied:
//
//   - Nodes are restricted to GPU nodes. Non-GPU nodes (control plane, CPU
//     workers, infra) are never targets, never run burn-in pods, and never need
//     health evaluation, so caching and watching them is pure overhead.
//
//   - managedFields is stripped from every cached object. It is metadata the
//     controllers never read and is frequently a large fraction of an object's
//     serialized size, particularly for Pods and CRs written by several
//     controllers.
//
// Pods are deliberately not label-scoped. The obvious selector,
// nvcre.nvidia.com/job, is injected only into the worker replicatedJob's pod
// template — MPI launcher pods do not carry it (see workerTargetJob in
// pkg/workload), and the launcher is exactly the pod that NCCL bandwidth
// measurement and timeout log capture read from. Scoping on that label would
// make launchers invisible to the cache. Pod reads are instead kept cheap via
// field indexes; see indexes.go.
func CacheOptions() cache.Options {
	return cache.Options{
		DefaultTransform: cache.TransformStripManagedFields(),
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Node{}: {
				Label: labels.SelectorFromSet(labels.Set{GPUNodeLabel: present}),
			},
		},
	}
}
