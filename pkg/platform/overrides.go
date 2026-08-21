// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package platform provides model-independent, CSP/GPU-architecture-only
// overrides for WorkloadRun resources. Override definitions live in YAML
// templates (overrides/workloadrun.yaml) that reference the same _lib/
// fragments used by catalog entries, ensuring a single source of truth
// for CSP/GPU-specific networking, NCCL configuration, and DRA setup.
package platform

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"text/template"

	crev1alpha1 "github.com/dsx-ai-factory/cluster-readiness-engine/api/v1alpha1"
	"github.com/dsx-ai-factory/cluster-readiness-engine/pkg/catalog"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	sigyaml "sigs.k8s.io/yaml"
)

//go:embed overrides/*.yaml
var overridesFS embed.FS

// OverrideConfig holds template data for rendering platform overrides.
// Field names match the catalog template data shape so _lib/ fragments
// can be rendered directly without mapping.
type OverrideConfig struct {
	EntryName     string
	NodesPerJob   int32
	GpusPerNode   int32
	MlnxPerNode   int32
	EnableMNNVL   bool
	FrameworkType string
}

// WorkloadRunOverride extends OverrideSpec with fields that are consumed
// by the WorkloadRun controller at build time and never stored in the
// Kubernetes API. Keeping them out of OverrideSpec avoids CRD schema changes.
type WorkloadRunOverride struct {
	crev1alpha1.OverrideSpec

	// PreCommand contains shell lines prepended to the trainer command.
	// Applied by the WorkloadRun controller; baked into trainer.command/args.
	PreCommand []string

	// MPIArgs contains mpirun arguments prepended to the MPI launcher command.
	// Applied by the WorkloadRun controller; baked into trainer.args.
	MPIArgs []string
}

// BuildOverrides renders the platform override templates and returns
// the resulting WorkloadRunOverride list.
func BuildOverrides(cfg OverrideConfig) []WorkloadRunOverride {
	rendered, err := renderTemplate("overrides/workloadrun.yaml", cfg)
	if err != nil {
		panic(fmt.Sprintf("platform: render overrides: %v", err))
	}

	overrides, err := parseWorkloadRunOverrides(rendered)
	if err != nil {
		panic(fmt.Sprintf("platform: parse overrides: %v", err))
	}
	return overrides
}

// renderTemplate renders an embedded YAML template with the given data.
func renderTemplate(name string, data OverrideConfig) ([]byte, error) {
	src, err := overridesFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}

	// Render with the catalog's function map. These templates pull in fragments
	// from the catalog's entries/_lib/, so they must be rendered with the same
	// functions those fragments are written against — a local subset would fail
	// to parse any fragment using a function it happened to omit.
	tmpl, err := template.New(name).Funcs(catalog.TemplateFuncsWithLib()).Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

// parseWorkloadRunOverrides unmarshals rendered YAML into WorkloadRunOverride
// objects. Each element is parsed as a raw JSON object; standard OverrideSpec
// fields plus WorkloadRun-only fields (preCommand, mpiArgs) are populated.
func parseWorkloadRunOverrides(data []byte) ([]WorkloadRunOverride, error) {
	jsonData, err := sigyaml.YAMLToJSON(data)
	if err != nil {
		return nil, fmt.Errorf("yaml to json: %w", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(jsonData, &items); err != nil {
		return nil, fmt.Errorf("unmarshal list: %w", err)
	}

	overrides := make([]WorkloadRunOverride, len(items))
	for i, item := range items {
		if err := parseOneOverride(item, &overrides[i].OverrideSpec); err != nil {
			return nil, fmt.Errorf("override[%d]: %w", i, err)
		}
		if err := parseWorkloadRunFields(item, &overrides[i]); err != nil {
			return nil, fmt.Errorf("override[%d]: %w", i, err)
		}
	}
	return overrides, nil
}

// parseWorkloadRunFields populates WorkloadRunOverride-only fields from raw JSON.
func parseWorkloadRunFields(raw json.RawMessage, o *WorkloadRunOverride) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	if pc, ok := fields["preCommand"]; ok {
		if err := json.Unmarshal(pc, &o.PreCommand); err != nil {
			return err
		}
	}
	if ma, ok := fields["mpiArgs"]; ok {
		if err := json.Unmarshal(ma, &o.MPIArgs); err != nil {
			return err
		}
	}
	return nil
}

// parseOneOverride populates an OverrideSpec from raw JSON. Fields that
// the Workflow controller treats as opaque (jobTemplate, dependencies)
// stay as raw bytes; typed fields (when, orchestration) are unmarshalled.
func parseOneOverride(raw json.RawMessage, spec *crev1alpha1.OverrideSpec) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}

	if w, ok := fields["when"]; ok {
		if err := json.Unmarshal(w, &spec.When); err != nil {
			return err
		}
	}

	if jt, ok := fields["jobTemplate"]; ok {
		spec.JobTemplate = &apiextensionsv1.JSON{Raw: jt}
	}

	if jtp, ok := fields["jobTemplatePatch"]; ok {
		// jobTemplatePatch is stored as a JSON string containing the patch array.
		var patchStr string
		if err := json.Unmarshal(jtp, &patchStr); err != nil {
			// Try treating it directly as raw JSON (array form).
			spec.JobTemplatePatch = &apiextensionsv1.JSON{Raw: jtp}
		} else {
			spec.JobTemplatePatch = &apiextensionsv1.JSON{Raw: []byte(patchStr)}
		}
	}

	if deps, ok := fields["dependencies"]; ok {
		var depList []json.RawMessage
		if err := json.Unmarshal(deps, &depList); err != nil {
			return err
		}
		spec.Dependencies = make([]crev1alpha1.DependencySpec, len(depList))
		for i, d := range depList {
			spec.Dependencies[i].RawExtension = runtime.RawExtension{Raw: d}
		}
	}

	if o, ok := fields["orchestration"]; ok {
		spec.Orchestration = &crev1alpha1.OrchestrationOverrideSpec{}
		if err := json.Unmarshal(o, spec.Orchestration); err != nil {
			return err
		}
	}

	return nil
}
