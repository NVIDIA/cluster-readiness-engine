// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// envDuplicate is one name an arm states more than once in a single list.
// Values are in composition order, so the last one is what the name resolves
// to: mpirun takes the final -x for a name, and the API server takes the last
// entry for a name in a map list. Conflicting marks the subset where that
// choice changes behaviour.
type envDuplicate struct {
	Platform    string   `json:"platform"`
	GPUArch     string   `json:"gpuArch"`
	Entry       string   `json:"entry"`
	Field       string   `json:"field"`
	Name        string   `json:"name"`
	Values      []string `json:"values"`
	Conflicting bool     `json:"conflicting"`
}

// envDuplicatesInput is each case's input.yaml: which slice of the cross
// product to sweep. Empty lists mean "every registered value", so a new
// platform, architecture or catalog entry is swept the day it is added
// without anyone editing this file.
type envDuplicatesInput struct {
	Platforms []string `json:"platforms"`
	GPUArchs  []string `json:"gpuArchs"`
	Entries   []string `json:"entries"`

	// NodesPerJob and EnableMNNVL feed the templates. They do not gate any
	// name, but GpusPerNode does: gcp/rtxpro6000 picks NCCL_P2P_LEVEL from
	// two mutually exclusive template branches keyed on it, and a sweep that
	// pinned one value would never walk the other.
	NodesPerJob int32   `json:"nodesPerJob"`
	GpusPerNode []int32 `json:"gpusPerNode"`
	EnableMNNVL bool    `json:"enableMNNVL"`
}

// TestEnvDuplicates renders every catalog entry at every platform and GPU
// architecture and records each environment variable an arm states twice.
//
// Arms compose shared fragments from entries/_lib/nccl/, and two fragments
// that both carry a name are resolved by splice order rather than by anything
// that fails: mpirun takes the last -x, so the composition order of two YAML
// includes silently decides what the ranks run with. That is what issue #507
// found on OCI GB300, where the shared profile forwarded
// NCCL_IB_SPLIT_DATA_ON_QPS=0 and the GB300 fragment forwarded 1, and the
// measured tuning survived only because the GB300 include happened to come
// second (ADR-100).
//
// The golden is the duplicate list itself rather than a pass/fail flag, so a
// regression names the arm and the two values instead of asserting that some
// count went up. An empty list is the catalog being clean.
//
// Two properties this has to keep:
//
//   - It asserts on RENDERED output, not on fragment text. A fragment may
//     write a name twice in two branches of one conditional and render it
//     once (entries/_lib/nccl/gcp-rtxpro6000-tcp-env.yaml does exactly this
//     with NCCL_P2P_LEVEL), so a source-level scan reports duplicates that do
//     not exist.
//   - It records same-value duplicates alongside conflicting ones. A repeat
//     that agrees with itself is not wrong, but it is two wasted entries
//     against Kubeflow's 128-item cap on Trainer.Args, and it is the cheap
//     signal that two fragments have started to overlap before one of them
//     changes a value.
func TestEnvDuplicates(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "env-duplicates",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input envDuplicatesInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		platforms := input.Platforms
		if len(platforms) == 0 {
			platforms = platform.Names()
		}
		arches := input.GPUArchs
		if len(arches) == 0 {
			arches = catalog.KnownGPUArchs()
		}
		entries := input.Entries
		if len(entries) == 0 {
			for _, c := range catalog.List() {
				entries = append(entries, c.Domain+"/"+c.Variant)
			}
		}
		gpuCounts := input.GpusPerNode
		if len(gpuCounts) == 0 {
			gpuCounts = []int32{0}
		}

		found := []envDuplicate{}
		for _, plat := range platforms {
			for _, arch := range arches {
				for _, gpus := range gpuCounts {
					dupes, err := sweepOne(plat, arch, entries, input, gpus)
					if err != nil {
						return err
					}
					found = append(found, dupes...)
				}
			}
		}

		data, err := json.MarshalIndent(found, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// sweepOne renders every entry for one (platform, arch, gpusPerNode) through
// the same path "nvcrectl certification render" takes, and returns the
// duplicates in the resolved workflows.
//
// A render or resolve error is recorded as a row rather than skipped. An entry
// that renders nothing has no duplicates, which is indistinguishable from an
// entry that is clean, so swallowing the error would let a catalog change that
// broke a whole platform's rendering read as this test passing. The one
// exception is an entry declining a shape below its own GPU floor, which is
// the catalog working; every other combination renders today, so a row with
// field "renderError" is something new.
func sweepOne(
	plat, arch string, entries []string, input envDuplicatesInput, gpusPerNode int32,
) ([]envDuplicate, error) {
	var found []envDuplicate

	for _, entry := range entries {
		domain, variant, ok := strings.Cut(entry, "/")
		if !ok {
			return nil, fmt.Errorf("entry %q is not domain/variant", entry)
		}

		cert := &nvcrev1alpha1.Certification{
			Spec: nvcrev1alpha1.CertificationSpec{
				NodesPerJob: new(input.NodesPerJob),
				EnableMNNVL: new(input.EnableMNNVL),
				Categories: []nvcrev1alpha1.CertificateCategory{
					{Domain: domain, Variant: variant},
				},
			},
		}
		cert.Name = "sweep"
		if gpusPerNode > 0 {
			cert.Spec.GpusPerNode = new(gpusPerNode)
		}

		workflows, err := renderCertification(cert, plat, arch, nil, "", catalog.DefaultOCISecondaryNetwork)
		if err != nil {
			// An entry whose GPU-count or parallelism floor the swept shape
			// does not meet is the catalog refusing an impossible run, not a
			// render that broke. nemotron5-56b does this at every gpusPerNode
			// below 4, and recording it once per combination would bury the
			// rows that mean something.
			if !strings.Contains(err.Error(), "requires at least") {
				found = append(found, renderFailure(plat, arch, entry, "render", err))
			}
			continue
		}
		if err := resolveWorkflowsOffline(cert, workflows, plat, arch); err != nil {
			found = append(found, renderFailure(plat, arch, entry, "resolve", err))
			continue
		}

		for i := range workflows {
			tj := workflows[i].Spec.JobTemplate.Spec.Workload.TrainJob
			if tj == nil || tj.Trainer == nil {
				continue
			}

			var envNames []string
			for _, e := range tj.Trainer.Env {
				envNames = append(envNames, e.Name+"="+e.Value)
			}

			for _, d := range duplicatesIn(mpiEnvPairs(tj.Trainer.Args)) {
				d.Platform, d.GPUArch, d.Entry, d.Field = plat, arch, entry, "args"
				found = append(found, d)
			}
			for _, d := range duplicatesIn(envNames) {
				d.Platform, d.GPUArch, d.Entry, d.Field = plat, arch, entry, "env"
				found = append(found, d)
			}
		}
	}

	return found, nil
}

// renderFailure records a combination that did not render, so the golden
// carries it instead of reading as a clean arm. The message is the error's
// first line: enough to identify what broke without pinning wording that a
// later change would churn.
func renderFailure(plat, arch, entry, stage string, err error) envDuplicate {
	msg, _, _ := strings.Cut(err.Error(), "\n")
	return envDuplicate{
		Platform: plat,
		GPUArch:  arch,
		Entry:    entry,
		Field:    "renderError",
		Name:     stage,
		Values:   []string{msg},
	}
}

// mpiEnvPairs returns the "NAME=VALUE" operand of every "-x" in an mpirun
// argument list, in order. A trailing "-x" with no operand is ignored: it is
// malformed, and not the thing this test is looking for.
func mpiEnvPairs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "-x" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// duplicatesIn reports every name that appears more than once in a list of
// "NAME=VALUE" entries, with its values in the order they were composed.
// Results come back in first-occurrence order so the golden is stable.
//
// An operand with no "=" is mpirun's forward-by-name form: the rank inherits
// whatever the launcher's own environment holds. The catalog writes it by hand
// for LD_LIBRARY_PATH on the AWS H100 arms, and pkg/platform/nccl.go emits it
// for any valueFrom variable, whose value Kubernetes resolves on the launcher.
// It is recorded as a distinct value rather than as an empty one, so a name
// forwarded once by value and once by name reads as the conflict it is: the
// two forms can disagree at run time, and which the rank gets depends on
// order.
func duplicatesIn(pairs []string) []envDuplicate {
	order := make([]string, 0, len(pairs))
	values := make(map[string][]string, len(pairs))

	for _, p := range pairs {
		name, value, byValue := strings.Cut(p, "=")
		if !byValue {
			value = "(forwarded by name)"
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = append(values[name], value)
	}

	var out []envDuplicate
	for _, name := range order {
		vs := values[name]
		if len(vs) < 2 {
			continue
		}
		conflicting := false
		for _, v := range vs[1:] {
			if v != vs[0] {
				conflicting = true
				break
			}
		}
		out = append(out, envDuplicate{Name: name, Values: vs, Conflicting: conflicting})
	}
	return out
}
