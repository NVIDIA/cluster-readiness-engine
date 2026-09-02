// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains API Schema definitions for the nvcre v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=nvcre.nvidia.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "nvcre.nvidia.com", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder()

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// Register adds one or more types to the scheme for this group-version.
func Register(objects ...runtime.Object) {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, objects...)
		metav1.AddToGroupVersion(scheme, GroupVersion)
		return nil
	})
}
