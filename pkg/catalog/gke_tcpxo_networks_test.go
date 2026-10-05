// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
)

const annotationGKEInterfaces = "networking.gke.io/interfaces"

type gkeTCPXONetworksInput struct {
	Category         string                   `json:"category"`
	Subcategory      string                   `json:"subcategory"`
	Target           nvcrev1alpha1.TargetSpec `json:"target"`
	NodesPerJob      int32                    `json:"nodesPerJob"`
	GpusPerNode      int32                    `json:"gpusPerNode"`
	GKETCPXONetworks []string                 `json:"gkeTCPXONetworks"`
}

// TestGKETCPXONetworks verifies the networking.gke.io/interfaces annotation
// the GCP H100 TCPXO patch renders: the detected GKE networks on eth1..eth8 in
// the order given, and gpu-nic0..gpu-nic7 when none were detected, which is
// byte-for-byte what the catalog hardcoded before detection (issue #432). The
// annotation must parse as JSON, since GKE reads it at pod admission.
func TestGKETCPXONetworks(t *testing.T) {
	p := &testutil.TestCaseParser{
		Subdir:         "gke-tcpxo-networks",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input gkeTCPXONetworksInput
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		entry := Lookup(input.Category, input.Subcategory)
		if entry == nil {
			return fmt.Errorf("category %s/%s not registered", input.Category, input.Subcategory)
		}
		spec, err := entry.Build(input.Target, BuildConfig{
			NodesPerJob:      input.NodesPerJob,
			GpusPerNode:      input.GpusPerNode,
			GPUArchitecture:  GPUArchFromNodeSelector(input.Target.NodeSelector),
			GKETCPXONetworks: input.GKETCPXONetworks,
		})
		if err != nil {
			return err
		}
		interfaces, err := gkeInterfaceAnnotations(spec)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(interfaces, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b)
		return nil
	})
}

// gkeInterfaceAnnotations finds every networking.gke.io/interfaces annotation
// anywhere in a built WorkflowSpec (the TCPXO patch lives in an override's
// TrainingRuntime dependency) and returns each as "interfaceName=network"
// pairs, failing if a value is not valid JSON. Entries also render the GCP
// GB200 RoCE override, so its fixed rdma-N annotation shows up too and pins
// that detected TCPXO networks never leak into it.
func gkeInterfaceAnnotations(spec nvcrev1alpha1.WorkflowSpec) ([][]string, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var values []string
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case map[string]any:
			for k, child := range n {
				if s, ok := child.(string); ok && k == annotationGKEInterfaces {
					values = append(values, s)
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range n {
				walk(child)
			}
		}
	}
	walk(doc)

	out := make([][]string, 0, len(values))
	for _, v := range values {
		var ifaces []struct {
			InterfaceName string `json:"interfaceName"`
			Network       string `json:"network"`
		}
		if err := json.Unmarshal([]byte(v), &ifaces); err != nil {
			return nil, fmt.Errorf("%s is not valid JSON: %w: %s", annotationGKEInterfaces, err, v)
		}
		pairs := make([]string, 0, len(ifaces))
		for _, i := range ifaces {
			pairs = append(pairs, i.InterfaceName+"="+i.Network)
		}
		out = append(out, pairs)
	}
	// Map iteration order is random; sort so the golden is stable.
	slices.SortFunc(out, func(a, b []string) int {
		return strings.Compare(strings.Join(a, ","), strings.Join(b, ","))
	})
	return out, nil
}
