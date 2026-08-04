// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodemonitor

import (
	"fmt"
	"sync"
)

// DetectorFactory creates a NodeFailureDetector from a configuration value.
// The config parameter is detector-specific (e.g., a CEL expression string).
type DetectorFactory func(config any) (NodeFailureDetector, error)

// Registry manages NodeFailureDetector implementations.
// It allows registration of detector factories and creation of detectors by name.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]DetectorFactory
}

// NewRegistry creates a new detector registry.
func NewRegistry() *Registry {
	return &Registry{
		factories: make(map[string]DetectorFactory),
	}
}

// Register adds a detector factory to the registry.
// If a factory with the same name already exists, it will be replaced.
func (r *Registry) Register(name string, factory DetectorFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// Create instantiates a detector by name with the given configuration.
// Returns an error if the detector type is not registered.
func (r *Registry) Create(name string, config any) (NodeFailureDetector, error) {
	r.mu.RLock()
	factory, ok := r.factories[name]
	r.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("unknown detector type: %s", name)
	}
	return factory(config)
}

// Has returns true if a detector with the given name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[name]
	return ok
}

// DefaultRegistry is the global registry with built-in detectors.
// Use this for standard detector lookups.
var DefaultRegistry = NewRegistry()
