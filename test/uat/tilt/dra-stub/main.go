// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Stub DRA controller for KWOK UAT tests.
//
// Simulates the GPU operator's DRA driver by:
// 1. Creating ResourceClaimTemplates from ComputeDomain specs
// 2. Marking ResourceClaims as allocated so pods become schedulable
//
// Build: CGO_ENABLED=0 go build -o dra-stub ./test/uat/tilt/dra-stub/
package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var (
	computeDomainGVR = schema.GroupVersionResource{
		Group: "resource.nvidia.com", Version: "v1beta1", Resource: "computedomains",
	}
	rctGVR = schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaimtemplates",
	}
	rcGVR = schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims",
	}
)

func main() {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	client := dynamic.NewForConfigOrDie(cfg)

	log.Println("dra-stub controller started")
	ctx := context.Background()
	for {
		reconcileComputeDomains(ctx, client)
		reconcileResourceClaims(ctx, client)
		time.Sleep(2 * time.Second)
	}
}

// reconcileComputeDomains creates ResourceClaimTemplates referenced by ComputeDomains.
func reconcileComputeDomains(ctx context.Context, client dynamic.Interface) {
	cds, err := client.Resource(computeDomainGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return // CRD may not exist yet
	}

	for _, cd := range cds.Items {
		channel, ok, _ := unstructured.NestedMap(cd.Object, "spec", "channel")
		if !ok {
			continue
		}
		rctObj, ok, _ := unstructured.NestedMap(channel, "resourceClaimTemplate")
		if !ok {
			continue
		}
		templateName, ok, _ := unstructured.NestedString(rctObj, "name")
		if !ok || templateName == "" {
			continue
		}

		ns := cd.GetNamespace()
		// Check if ResourceClaimTemplate already exists.
		_, err := client.Resource(rctGVR).Namespace(ns).Get(ctx, templateName, metav1.GetOptions{})
		if err == nil {
			continue // already exists
		}

		// Create ResourceClaimTemplate with ownerReference to ComputeDomain.
		rct := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceClaimTemplate",
			"metadata": map[string]any{
				"name":      templateName,
				"namespace": ns,
				"labels": map[string]any{
					"app.kubernetes.io/created-by": "dra-stub",
				},
				"ownerReferences": []any{
					map[string]any{
						"apiVersion": cd.GetAPIVersion(),
						"kind":       cd.GetKind(),
						"name":       cd.GetName(),
						"uid":        string(cd.GetUID()),
					},
				},
			},
			"spec": map[string]any{
				"spec": map[string]any{
					"devices": map[string]any{
						"requests": []any{
							map[string]any{
								"name": "gpu-channel",
								"exactly": map[string]any{
									"deviceClassName": "gpu.nvidia.com",
								},
							},
						},
					},
				},
			},
		}}

		if _, err := client.Resource(rctGVR).Namespace(ns).Create(ctx, rct, metav1.CreateOptions{}); err != nil {
			log.Printf("create ResourceClaimTemplate %s/%s: %v", ns, templateName, err)
		} else {
			log.Printf("created ResourceClaimTemplate %s/%s for ComputeDomain %s", ns, templateName, cd.GetName())
		}
	}
}

// reconcileResourceClaims marks unallocated ResourceClaims as allocated.
func reconcileResourceClaims(ctx context.Context, client dynamic.Interface) {
	rcs, err := client.Resource(rcGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}

	for _, rc := range rcs.Items {
		alloc, _, _ := unstructured.NestedMap(rc.Object, "status", "allocation")
		if alloc != nil {
			continue // already allocated
		}

		// Patch status.allocation to mark the claim as allocated.
		patch := map[string]any{
			"status": map[string]any{
				"allocation": map[string]any{
					"devices": map[string]any{
						"results": []any{},
					},
				},
			},
		}
		patchBytes, _ := json.Marshal(patch)
		name := rc.GetName()
		ns := rc.GetNamespace()

		if _, err := client.Resource(rcGVR).Namespace(ns).Patch(
			ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{}, "status",
		); err != nil {
			log.Printf("patch ResourceClaim %s/%s status: %v", ns, name, err)
		} else {
			log.Printf("allocated ResourceClaim %s/%s", ns, name)
		}
	}
}
