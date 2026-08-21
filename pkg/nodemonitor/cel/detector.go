// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cel provides a CEL-based node failure detector implementation.
package cel

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/cel-go/cel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/nodemonitor"
)

const (
	// DetectorName is the unique identifier for the CEL detector.
	DetectorName = "cel"

	// reasonCELMatch is the reason code when a CEL expression matches.
	reasonCELMatch = "CELExpressionMatched"
)

// nodeCacheEntry holds a previously converted node map keyed by resource version.
type nodeCacheEntry struct {
	resourceVersion string
	nodeMap         map[string]any
}

// Detector evaluates CEL expressions against Kubernetes Node objects.
type Detector struct {
	expression string
	program    cel.Program
	mu         sync.RWMutex

	nodeMapMu    sync.RWMutex
	nodeMapCache map[string]nodeCacheEntry
}

// Detector is the reference implementation of NodeFailureDetector. The Job
// controller constructs it directly — detectors are selected by which typed
// field is set on Job.spec.nodeHealthMonitor, the same way workload adapters are
// selected by WorkloadSpec — so this assertion is what keeps the interface an
// enforced contract for future implementations rather than documentation that
// nothing checks.
var _ nodemonitor.NodeFailureDetector = (*Detector)(nil)

// NewDetector creates a new CEL-based node failure detector.
// The expression must be a valid CEL expression that returns a boolean.
func NewDetector(expression string) (*Detector, error) {
	d := &Detector{
		expression: expression,
	}

	if err := d.Validate(); err != nil {
		return nil, err
	}

	return d, nil
}

// Name returns the detector identifier.
func (d *Detector) Name() string {
	return DetectorName
}

// Validate compiles and validates the CEL expression.
func (d *Detector) Validate() error {
	env, err := d.createCELEnvironment()
	if err != nil {
		return fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Compile(d.expression)
	if issues != nil && issues.Err() != nil {
		return fmt.Errorf("CEL compilation error: %w", issues.Err())
	}

	// Verify the expression returns a boolean
	if ast.OutputType() != cel.BoolType {
		return fmt.Errorf("CEL expression must return bool, got %v", ast.OutputType())
	}

	program, err := env.Program(ast)
	if err != nil {
		return fmt.Errorf("failed to create CEL program: %w", err)
	}

	d.mu.Lock()
	d.program = program
	d.mu.Unlock()

	return nil
}

// Detect evaluates the CEL expression against the given node.
// Returns a DetectionResult indicating whether the node has a hardware failure.
func (d *Detector) Detect(ctx context.Context, node *corev1.Node) (nodemonitor.DetectionResult, error) {
	d.mu.RLock()
	program := d.program
	d.mu.RUnlock()

	if program == nil {
		return nodemonitor.DetectionResult{}, fmt.Errorf("CEL program not initialized, call Validate() first")
	}

	// Convert node to unstructured map for CEL evaluation
	nodeMap, err := d.nodeToMap(node)
	if err != nil {
		return nodemonitor.DetectionResult{}, fmt.Errorf("failed to convert node to map: %w", err)
	}

	result, _, err := program.ContextEval(ctx, map[string]any{
		"node": nodeMap,
	})
	if err != nil {
		return nodemonitor.DetectionResult{}, fmt.Errorf("CEL evaluation error: %w", err)
	}

	failed, ok := result.Value().(bool)
	if !ok {
		return nodemonitor.DetectionResult{}, fmt.Errorf("unexpected CEL result type: %T, expected bool", result.Value())
	}

	return nodemonitor.DetectionResult{
		Failed:   failed,
		NodeName: node.Name,
		Reason:   reasonCELMatch,
		Message:  fmt.Sprintf("CEL expression evaluated to %v for node %s", failed, node.Name),
	}, nil
}

// createCELEnvironment sets up the CEL environment with the node variable.
func (d *Detector) createCELEnvironment() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("node", cel.MapType(cel.StringType, cel.DynType)),
		// Enable optional types for safer null handling
		cel.OptionalTypes(),
	)
}

// nodeToMap converts a Node object to a CEL-compatible nested map structure.
// This provides access to the full Node object in CEL expressions.
// It ensures commonly accessed fields always exist with default values to prevent
// "no such key" errors in CEL expressions.
//
// Results are cached by node UID and resource version so that repeated calls
// for the same (unchanged) node skip the expensive ToUnstructured conversion.
func (d *Detector) nodeToMap(node *corev1.Node) (map[string]any, error) {
	uid := string(node.UID)
	rv := node.ResourceVersion

	// Fast path: return cached map if the resource version matches.
	d.nodeMapMu.RLock()
	entry, ok := d.nodeMapCache[uid]
	d.nodeMapMu.RUnlock()
	if ok && entry.resourceVersion == rv {
		return entry.nodeMap, nil
	}

	// Slow path: convert and cache.
	unstructured, err := runtime.DefaultUnstructuredConverter.ToUnstructured(node)
	if err != nil {
		return nil, fmt.Errorf("failed to convert node to unstructured: %w", err)
	}
	d.ensureDefaults(unstructured)

	d.nodeMapMu.Lock()
	if d.nodeMapCache == nil {
		d.nodeMapCache = make(map[string]nodeCacheEntry)
	}
	d.nodeMapCache[uid] = nodeCacheEntry{resourceVersion: rv, nodeMap: unstructured}
	d.nodeMapMu.Unlock()

	return unstructured, nil
}

// ensureDefaults ensures commonly accessed fields have default values to prevent
// CEL "no such key" errors when fields are nil/empty in the original Node.
func (d *Detector) ensureDefaults(nodeMap map[string]any) {
	// Ensure metadata exists with defaults
	metadata, ok := nodeMap["metadata"].(map[string]any)
	if !ok {
		metadata = make(map[string]any)
		nodeMap["metadata"] = metadata
	}
	if _, ok := metadata["labels"]; !ok {
		metadata["labels"] = make(map[string]any)
	}
	if _, ok := metadata["annotations"]; !ok {
		metadata["annotations"] = make(map[string]any)
	}

	// Ensure spec exists with defaults
	spec, ok := nodeMap["spec"].(map[string]any)
	if !ok {
		spec = make(map[string]any)
		nodeMap["spec"] = spec
	}
	if _, ok := spec["taints"]; !ok {
		spec["taints"] = []any{}
	}
	if _, ok := spec["unschedulable"]; !ok {
		spec["unschedulable"] = false
	}

	// Ensure status exists with defaults
	status, ok := nodeMap["status"].(map[string]any)
	if !ok {
		status = make(map[string]any)
		nodeMap["status"] = status
	}
	if _, ok := status["conditions"]; !ok {
		status["conditions"] = []any{}
	}
	if _, ok := status["addresses"]; !ok {
		status["addresses"] = []any{}
	}
	if _, ok := status["capacity"]; !ok {
		status["capacity"] = make(map[string]any)
	}
	if _, ok := status["allocatable"]; !ok {
		status["allocatable"] = make(map[string]any)
	}
}
