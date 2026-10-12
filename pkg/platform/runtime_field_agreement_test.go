// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	trainerv1alpha1 "github.com/kubeflow/trainer/v2/pkg/apis/trainer/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// Scopes a sentinel is searched in. Each replicatedJob is its own scope named
// "job:<name>", so the golden shows which pod a field reaches.
const (
	agreeScopeMetadata = "metadata"
	agreeScopeMLPolicy = "mlPolicy"
	agreeScopeJob      = "job:"
)

// fieldAgreementInput is each case's input.yaml: the builder to call.
type fieldAgreementInput struct {
	FrameworkType string `json:"frameworkType"`
}

// fieldProbe is how one RuntimeConfig field is exercised: set writes a value
// no baseline render contains, and sentinel is the text that value leaves in
// the rendered runtime (numbers as their decimal text).
type fieldProbe struct {
	set      func(*RuntimeConfig)
	sentinel string
}

// fieldProbes has one entry per RuntimeConfig field. A struct field with no
// entry is a violation, so adding a field means adding a probe here and then
// either making every builder honour it or recording the (builder, field)
// pair in fieldAgreementExceptions with a reason.
//
// The two queue probes also name a scheduler: applyGangScheduler emits the
// queue label only when GangSchedulerName is set, so without one their
// sentinels could never appear and the probes would report a false miss.
var fieldProbes = map[string]fieldProbe{
	"EntryName": {
		set:      func(c *RuntimeConfig) { c.EntryName = "agree-sentinel-entry" },
		sentinel: "agree-sentinel-entry",
	},
	"Image": {
		set:      func(c *RuntimeConfig) { c.Image = "agree.example.com/image:sentinel" },
		sentinel: "agree.example.com/image:sentinel",
	},
	"NodesPerJob": {
		set:      func(c *RuntimeConfig) { c.NodesPerJob = 4242 },
		sentinel: "4242",
	},
	"GpusPerNode": {
		set:      func(c *RuntimeConfig) { c.GpusPerNode = 1337 },
		sentinel: "1337",
	},
	"Env": {
		set:      func(c *RuntimeConfig) { c.Env = []corev1.EnvVar{{Name: "AGREE_SENTINEL_ENV", Value: "1"}} },
		sentinel: "AGREE_SENTINEL_ENV",
	},
	"Volumes": {
		set: func(c *RuntimeConfig) {
			c.Volumes = []corev1.Volume{{
				Name:     "agree-sentinel-volume",
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			}}
		},
		sentinel: "agree-sentinel-volume",
	},
	"VolumeMounts": {
		set: func(c *RuntimeConfig) {
			c.VolumeMounts = []corev1.VolumeMount{{Name: "agree-sentinel-mount", MountPath: "/agree-sentinel-mount"}}
		},
		sentinel: "agree-sentinel-mount",
	},
	"InitContainers": {
		set: func(c *RuntimeConfig) {
			c.InitContainers = []corev1.Container{{Name: "agree-sentinel-init", Image: "busybox:1.36"}}
		},
		sentinel: "agree-sentinel-init",
	},
	"Resources": {
		set: func(c *RuntimeConfig) {
			c.Resources = &corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4321Gi")},
			}
		},
		sentinel: "4321Gi",
	},
	"GangSchedulerName": {
		set:      func(c *RuntimeConfig) { c.GangSchedulerName = "agree-sentinel-scheduler" },
		sentinel: "agree-sentinel-scheduler",
	},
	"GangSchedulerQueue": {
		set: func(c *RuntimeConfig) {
			c.GangSchedulerName = schedulerNameKAI
			c.GangSchedulerQueue = "agree-sentinel-queue"
		},
		sentinel: "agree-sentinel-queue",
	},
	"GangSchedulerQueueLabelKey": {
		set: func(c *RuntimeConfig) {
			c.GangSchedulerName = schedulerNameKAI
			c.GangSchedulerQueueLabelKey = "agree.example.com/queue"
		},
		sentinel: "agree.example.com/queue",
	},
}

// fieldAgreementExceptions records every (builder, field) pair whose sentinel
// is allowed to appear nowhere, with the reason. An exception whose sentinel
// does appear is itself a violation, so an entry cannot outlive the behaviour
// it describes.
var fieldAgreementExceptions = map[string]map[string]string{
	wfFrameworkMPI: {
		"NodesPerJob": "BuildMPIRuntime writes a fixed mlPolicy.numNodes of 1 as the runtime default; " +
			"the worker replicatedJob is scaled by the TrainJob's trainer.numNodes, " +
			"which both call sites set from NodesPerJobForScale",
	},
}

// fieldAgreementField is one golden row: where the field's sentinel landed,
// and the recorded reason when it is allowed to land nowhere.
type fieldAgreementField struct {
	Field     string   `json:"field"`
	Sentinel  string   `json:"sentinel"`
	FoundIn   []string `json:"foundIn"`
	Exception string   `json:"exception,omitempty"`
}

// fieldAgreementProjection is the golden: every RuntimeConfig field in
// declaration order. violations is always empty in a recorded golden; a
// non-empty list fails the case before the golden is compared.
type fieldAgreementProjection struct {
	Fields     []fieldAgreementField `json:"fields"`
	Violations []string              `json:"violations"`
}

// agreeScope is one searchable part of a rendered runtime as compact JSON.
type agreeScope struct {
	name string
	json string
}

// TestBuildRuntimeFieldAgreement pins that every field of RuntimeConfig
// reaches every builder's output, unless fieldAgreementExceptions names the
// (builder, field) pair with a reason.
//
// This is the property issue #455 was missing: RuntimeConfig carried fields
// (Volumes, VolumeMounts, InitContainers, and ImagePullSecrets, which no
// builder ever read) that one builder silently ignored, so a WorkloadRun
// field was a no-op on one framework and nothing said so. The fields are
// enumerated with reflect rather than listed by hand, so a field added to the
// struct fails this test until every builder honours it or an exception
// records why one does not. Each field is driven with a sentinel value that no
// baseline render contains and the rendered runtime is searched for it, scope
// by scope: the metadata, the mlPolicy and each replicatedJob.
//
// It only asks whether each builder reads each field at all. Where volumes,
// mounts and init containers are placed, and in what order relative to the
// built-ins, is pinned by TestBuildRuntimeWorkloadFields.
func TestBuildRuntimeFieldAgreement(t *testing.T) {
	p := &testutil.TestCaseParser{Subdir: "build-runtime-field-agreement"}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in fieldAgreementInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		build, err := fieldAgreementBuilder(in.FrameworkType)
		if err != nil {
			return err
		}

		base := baseRuntimeConfig()
		baseline := string(build(base).Raw)
		exceptions := fieldAgreementExceptions[in.FrameworkType]
		proj := fieldAgreementProjection{Fields: []fieldAgreementField{}, Violations: []string{}}

		cfgType := reflect.TypeFor[RuntimeConfig]()
		for field := range cfgType.Fields() {
			name := field.Name
			probe, ok := fieldProbes[name]
			if !ok {
				proj.Violations = append(proj.Violations, "RuntimeConfig."+name+
					" has no probe in fieldProbes: add a setter and sentinel, then make every builder honour the field or record an exception")
				continue
			}
			row := fieldAgreementField{Field: name, Sentinel: probe.sentinel, FoundIn: []string{}, Exception: exceptions[name]}
			// A sentinel the baseline already contains would be found whether
			// or not the builder read the field, so the check would be vacuous.
			if strings.Contains(baseline, probe.sentinel) {
				proj.Violations = append(proj.Violations, name+": sentinel "+probe.sentinel+" already occurs in the baseline render")
			}

			cfg := base
			probe.set(&cfg)
			scopes, err := fieldAgreementScopes(build(cfg))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			for _, s := range scopes {
				if strings.Contains(s.json, probe.sentinel) {
					row.FoundIn = append(row.FoundIn, s.name)
				}
			}

			switch {
			case len(row.FoundIn) == 0 && row.Exception == "":
				proj.Violations = append(proj.Violations, name+": sentinel "+probe.sentinel+
					" appears nowhere in the "+in.FrameworkType+" runtime and no exception is recorded")
			case len(row.FoundIn) > 0 && row.Exception != "":
				proj.Violations = append(proj.Violations, name+": exception is stale, sentinel "+probe.sentinel+
					" appears in "+strings.Join(row.FoundIn, ", "))
			}
			proj.Fields = append(proj.Fields, row)
		}

		// A probe or exception that names no struct field is dead weight that
		// would hide a rename; say so rather than skip it.
		for name := range fieldProbes {
			if _, ok := cfgType.FieldByName(name); !ok {
				proj.Violations = append(proj.Violations, "fieldProbes names "+name+", which is not a RuntimeConfig field")
			}
		}
		for name := range exceptions {
			if _, ok := cfgType.FieldByName(name); !ok {
				proj.Violations = append(proj.Violations, "fieldAgreementExceptions names "+name+", which is not a RuntimeConfig field")
			}
		}

		sort.Strings(proj.Violations)
		require.Empty(tc.T, proj.Violations, "RuntimeConfig field agreement violated")

		out, err := json.MarshalIndent(proj, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(out) + "\n"
		return nil
	})
}

func fieldAgreementBuilder(frameworkType string) (func(RuntimeConfig) nvcrev1alpha1.DependencySpec, error) {
	switch frameworkType {
	case wfFrameworkMPI:
		return BuildMPIRuntime, nil
	case wfFrameworkTorch:
		return BuildTorchRuntime, nil
	case wfFrameworkExec:
		return BuildExecRuntime, nil
	default:
		return nil, fmt.Errorf("unknown frameworkType %q", frameworkType)
	}
}

// fieldAgreementScopes splits a rendered runtime into the scopes a sentinel is
// searched in, in a stable order: metadata, mlPolicy, then each replicatedJob
// in runtime order. Each is the typed object marshalled back to compact JSON,
// so the search sees what the Trainer would: a key the pinned API has no field
// for is dropped here as the API server would prune it, and does not count as
// the builder honouring the field.
func fieldAgreementScopes(dep nvcrev1alpha1.DependencySpec) ([]agreeScope, error) {
	var rt trainerv1alpha1.TrainingRuntime
	if err := json.Unmarshal(dep.Raw, &rt); err != nil {
		return nil, err
	}
	meta, err := json.Marshal(rt.ObjectMeta)
	if err != nil {
		return nil, err
	}
	policy, err := json.Marshal(rt.Spec.MLPolicy)
	if err != nil {
		return nil, err
	}
	scopes := []agreeScope{
		{name: agreeScopeMetadata, json: string(meta)},
		{name: agreeScopeMLPolicy, json: string(policy)},
	}
	for _, rj := range rt.Spec.Template.Spec.ReplicatedJobs {
		raw, err := json.Marshal(rj)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, agreeScope{name: agreeScopeJob + rj.Name, json: string(raw)})
	}
	return scopes, nil
}
