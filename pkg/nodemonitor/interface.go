// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodemonitor provides interfaces and implementations for detecting
// hardware failures on Kubernetes nodes.
package nodemonitor

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// DetectionResult represents the outcome of evaluating a node for hardware failures.
type DetectionResult struct {
	// Failed indicates whether the node has a hardware failure.
	Failed bool

	// NodeName is the name of the node that was evaluated.
	NodeName string

	// Reason is a short, machine-readable reason code for the failure.
	Reason string

	// Message provides human-readable details about the failure.
	Message string
}

// NodeFailureDetector is the interface for detecting hardware failures on nodes.
// Implementations can use different mechanisms such as CEL expressions,
// external APIs, or custom logic.
type NodeFailureDetector interface {
	// Name returns the unique identifier for this detector type.
	Name() string

	// Detect evaluates the given node and returns whether it has a hardware failure.
	// The context can be used for cancellation and timeouts.
	Detect(ctx context.Context, node *corev1.Node) (DetectionResult, error)

	// Validate checks if the detector configuration is valid.
	// This should be called during setup to catch configuration errors early.
	Validate() error
}
