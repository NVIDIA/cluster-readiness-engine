// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"

	"k8s.io/apimachinery/pkg/runtime"
)

// MarshalJSON implements custom JSON serialization for DependencySpec.
// The inline runtime.RawExtension normally swallows all sibling fields during
// marshaling. This method merges the embedded resource JSON with the when
// field so it survives the round-trip through the API server.
func (d DependencySpec) MarshalJSON() ([]byte, error) {
	// Decode the embedded resource into a map
	m := make(map[string]json.RawMessage)
	if len(d.Raw) > 0 {
		if err := json.Unmarshal(d.Raw, &m); err != nil {
			return nil, err
		}
	}

	// Add when if non-zero (all fields are pointers or strings, safe to compare)
	if d.When != (WhenSpec{}) {
		b, err := json.Marshal(d.When)
		if err != nil {
			return nil, err
		}
		m["when"] = b
	}

	return json.Marshal(m)
}

// UnmarshalJSON implements custom JSON deserialization for DependencySpec.
// The inline runtime.RawExtension normally consumes the entire JSON input,
// leaving sibling fields unpopulated. This method extracts the when field
// separately, then stores only the resource fields in RawExtension.
func (d *DependencySpec) UnmarshalJSON(data []byte) error {
	// Extract sibling fields using a plain struct (no inline embedding)
	type siblingFields struct {
		When WhenSpec `json:"when,omitempty"`
	}
	var sf siblingFields
	if err := json.Unmarshal(data, &sf); err != nil {
		return err
	}
	d.When = sf.When

	// Strip sibling fields from the raw data to keep only the embedded resource
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	delete(m, "when")

	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	d.RawExtension = runtime.RawExtension{Raw: raw}
	return nil
}
