// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

// ValidateCertificationResources rejects the resources a Certification's CRD
// schema cannot: a negative quantity, and a request above its matching limit.
//
// These were CEL rules once, and API servers through 1.33 refused the whole
// CRD over them, because they cost any CEL on an int-or-string value as a
// 3 MiB string (ADR-101). The schema's Pattern still rejects a negative
// string at admission; a negative integer and the request/limit comparison
// are only caught here. The controller runs this before it creates any
// Workflow and nvcrectl runs it when it reads a certification file, so both
// fail before work starts.
//
// It checks spec.resources and each category's own resources, the scope the
// CEL rules had. ResolveOptions replaces Resources whole, so that is also
// every resources block a Workflow can be built from. Every violation is
// reported, not only the first: the spec is immutable, so each round trip
// costs the operator a delete and a re-create.
func ValidateCertificationResources(spec *nvcrev1alpha1.CertificationSpec) error {
	var problems []string
	problems = append(problems, categoryResourcesProblems("spec.resources", spec.Resources)...)
	for i, category := range spec.Categories {
		if category.Options == nil {
			continue
		}
		path := fmt.Sprintf("spec.categories[%d].options.resources", i)
		problems = append(problems, categoryResourcesProblems(path, category.Options.Resources)...)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid resources: %s", strings.Join(problems, "; "))
}

// The two sides of a resources block, as they appear in field paths.
const (
	resourcesRequests = "requests"
	resourcesLimits   = "limits"
)

// namedQuantity pairs a resource name with one side's value for it.
type namedQuantity struct {
	name  string
	value *resource.Quantity
}

func categoryResourcesProblems(path string, res *nvcrev1alpha1.CategoryResources) []string {
	if res == nil {
		return nil
	}
	var problems []string
	sides := []struct {
		name string
		list *nvcrev1alpha1.CategoryResourceList
	}{{resourcesRequests, res.Requests}, {resourcesLimits, res.Limits}}
	for _, side := range sides {
		for _, q := range resourceListQuantities(side.list) {
			if q.value != nil && q.value.Sign() < 0 {
				problems = append(problems, fmt.Sprintf("%s.%s.%s: %s must be a non-negative quantity, got %s",
					path, side.name, q.name, q.name, q.value))
			}
		}
	}
	requests, limits := resourceListQuantities(res.Requests), resourceListQuantities(res.Limits)
	for i := range requests {
		request, limit := requests[i].value, limits[i].value
		if request != nil && limit != nil && request.Cmp(*limit) > 0 {
			name := requests[i].name
			problems = append(problems, fmt.Sprintf("%s: requests.%s (%s) must not exceed limits.%s (%s)",
				path, name, request, name, limit))
		}
	}
	return problems
}

// resourceListQuantities returns cpu then memory, with nil values for an unset
// list or field, so requests and limits line up by index.
func resourceListQuantities(list *nvcrev1alpha1.CategoryResourceList) []namedQuantity {
	if list == nil {
		return []namedQuantity{{name: "cpu"}, {name: "memory"}}
	}
	return []namedQuantity{{"cpu", list.CPU}, {"memory", list.Memory}}
}
