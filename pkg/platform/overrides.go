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

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	corev1 "k8s.io/api/core/v1"
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
	EntryName   string
	NodesPerJob int32
	GpusPerNode int32
	MlnxPerNode int32

	// NicResourceName is the extended resource name of the RDMA NIC devices
	// for the on-prem GB200/GB300 override. Empty means the override omits
	// the NIC resource block. The per-container count comes from MlnxPerNode.
	NicResourceName string

	EnableMNNVL   bool
	FrameworkType string

	// UserEnv overrides matching names in platform trainer.env patches and
	// mpiArgs -x pairs so WorkloadRun values retain precedence over platform
	// values. User-only variables stay in the runtime container environment.
	UserEnv []corev1.EnvVar `json:"-" yaml:"-"`
}

// WorkloadRunOverride extends OverrideSpec with fields that are consumed
// by the WorkloadRun controller at build time and never stored in the
// Kubernetes API. Keeping them out of OverrideSpec avoids CRD schema changes.
type WorkloadRunOverride struct {
	nvcrev1alpha1.OverrideSpec

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
	mergeUserEnvIntoTrainerPatches(overrides, cfg.UserEnv)
	mergeUserEnvIntoRuntimeDependencies(overrides, cfg.UserEnv)
	mergeUserEnvIntoMPIArgs(overrides, cfg.UserEnv)
	return overrides
}

// mergeUserEnvIntoMPIArgs preserves WorkloadRun spec.env values in platform
// mpiArgs. MPI ranks see only what mpirun forwards with -x, so a platform pair
// for a name the user also set would otherwise decide the value at the ranks.
// Only matching names are replaced, in place.
func mergeUserEnvIntoMPIArgs(overrides []WorkloadRunOverride, userEnv []corev1.EnvVar) {
	user := make(map[string]corev1.EnvVar, len(userEnv))
	for _, env := range userEnv {
		user[env.Name] = env
	}
	for i := range overrides {
		args := overrides[i].MPIArgs
		for j := range args {
			name, ok := mpiEnvArgName(args, j)
			if !ok {
				continue
			}
			if env, set := user[name]; set {
				args[j+1] = mpiEnvArg(env)
			}
		}
	}
}

// mergeUserEnvIntoTrainerPatches preserves WorkloadRun spec.env values in
// platform overrides that replace the unnamed Trainer.Env list. Kubeflow
// Trainer applies trainer.env after the runtime container environment, so a
// conflicting platform value would otherwise silently win. Only matching
// names are overridden: user-only values belong to the runtime container and
// must not be copied into every platform trainer patch.
func mergeUserEnvIntoTrainerPatches(overrides []WorkloadRunOverride, userEnv []corev1.EnvVar) {
	if len(userEnv) == 0 {
		return
	}

	for i := range overrides {
		patch := overrides[i].JobTemplate
		if patch == nil {
			continue
		}

		var root map[string]any
		if err := json.Unmarshal(patch.Raw, &root); err != nil {
			panic(fmt.Sprintf("platform: decode jobTemplate override[%d]: %v", i, err))
		}
		trainer, ok := nestedOverrideMap(root, "spec", "workload", "trainJob", "trainer")
		if !ok {
			continue
		}
		rawEnv, ok := trainer["env"]
		if !ok {
			continue
		}
		envJSON, err := json.Marshal(rawEnv)
		if err != nil {
			panic(fmt.Sprintf("platform: encode trainer env override[%d]: %v", i, err))
		}
		var patchEnv []corev1.EnvVar
		if err := json.Unmarshal(envJSON, &patchEnv); err != nil {
			panic(fmt.Sprintf("platform: decode trainer env override[%d]: %v", i, err))
		}
		trainer["env"] = overrideEnvByName(patchEnv, userEnv)

		updated, err := json.Marshal(root)
		if err != nil {
			panic(fmt.Sprintf("platform: encode jobTemplate override[%d]: %v", i, err))
		}
		overrides[i].JobTemplate.Raw = updated
	}
}

// mergeUserEnvIntoRuntimeDependencies preserves WorkloadRun spec.env values
// when a platform override patches a TrainingRuntime dependency. Our own
// mergeOrAppendDependency merges the override object into the generated
// runtime and replaces the container env list, so matching platform values
// would otherwise win. Only matching names are replaced.
func mergeUserEnvIntoRuntimeDependencies(overrides []WorkloadRunOverride, userEnv []corev1.EnvVar) {
	if len(userEnv) == 0 {
		return
	}

	for overrideIndex := range overrides {
		for dependencyIndex := range overrides[overrideIndex].Dependencies {
			dependency := &overrides[overrideIndex].Dependencies[dependencyIndex]
			var root map[string]any
			if err := json.Unmarshal(dependency.Raw, &root); err != nil {
				panic(fmt.Sprintf("platform: decode dependency override[%d][%d]: %v", overrideIndex, dependencyIndex, err))
			}
			kind, _ := root[keyKind].(string)
			if kind != kindTrainingRuntime {
				continue
			}
			runtimeSpec, ok := nestedOverrideMap(root, keySpec, keyTemplate, keySpec)
			if !ok {
				continue
			}
			replicatedJobs, ok := runtimeSpec[keyReplicatedJobs].([]any)
			if !ok {
				continue
			}
			for _, rawJob := range replicatedJobs {
				job, ok := rawJob.(map[string]any)
				if !ok {
					continue
				}
				podSpec, ok := nestedOverrideMap(job, keyTemplate, keySpec, keyTemplate, keySpec)
				if !ok {
					continue
				}
				containers, ok := podSpec[keyContainers].([]any)
				if !ok {
					continue
				}
				for _, rawContainer := range containers {
					container, ok := rawContainer.(map[string]any)
					if !ok {
						continue
					}
					rawEnv, ok := container[keyEnv]
					if !ok {
						continue
					}
					envJSON, err := json.Marshal(rawEnv)
					if err != nil {
						panic(fmt.Sprintf("platform: encode dependency env override[%d][%d]: %v", overrideIndex, dependencyIndex, err))
					}
					var patchEnv []corev1.EnvVar
					if err := json.Unmarshal(envJSON, &patchEnv); err != nil {
						panic(fmt.Sprintf("platform: decode dependency env override[%d][%d]: %v", overrideIndex, dependencyIndex, err))
					}
					updatedEnv, err := json.Marshal(overrideEnvByName(patchEnv, userEnv))
					if err != nil {
						panic(fmt.Sprintf("platform: encode dependency env override[%d][%d]: %v", overrideIndex, dependencyIndex, err))
					}
					container[keyEnv] = json.RawMessage(updatedEnv)
				}
			}
			updated, err := json.Marshal(root)
			if err != nil {
				panic(fmt.Sprintf("platform: encode dependency override[%d][%d]: %v", overrideIndex, dependencyIndex, err))
			}
			dependency.Raw = updated
		}
	}
}

func nestedOverrideMap(root map[string]any, path ...string) (map[string]any, bool) {
	current := root
	for _, key := range path {
		value, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = value
	}
	return current, true
}

func overrideEnvByName(base, user []corev1.EnvVar) []corev1.EnvVar {
	overridden := append([]corev1.EnvVar(nil), base...)
	indices := make(map[string]int, len(overridden))
	for i, env := range overridden {
		indices[env.Name] = i
	}
	for _, env := range user {
		if index, ok := indices[env.Name]; ok {
			overridden[index] = env
		}
	}
	return overridden
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
func parseOneOverride(raw json.RawMessage, spec *nvcrev1alpha1.OverrideSpec) error {
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
		spec.Dependencies = make([]nvcrev1alpha1.DependencySpec, len(depList))
		for i, d := range depList {
			spec.Dependencies[i].RawExtension = runtime.RawExtension{Raw: d}
		}
	}

	if o, ok := fields["orchestration"]; ok {
		spec.Orchestration = &nvcrev1alpha1.OrchestrationOverrideSpec{}
		if err := json.Unmarshal(o, spec.Orchestration); err != nil {
			return err
		}
	}

	return nil
}
