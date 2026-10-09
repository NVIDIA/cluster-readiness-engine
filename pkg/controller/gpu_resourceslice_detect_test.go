// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// resourceSliceReader hands back nodes for discoverTargetNodes and
// gpu.nvidia.com ResourceSlices for augmentGPUProductLabels's fallback
// lookup. A configured listErr simulates a missing RBAC grant or an
// unavailable API group on the ResourceSlice List call specifically, so the
// "log and leave nodes unlabeled" path can be exercised without also
// breaking node discovery itself.
type resourceSliceReader struct {
	nodes   []corev1.Node
	slices  []resourcev1.ResourceSlice
	listErr error
}

func (r resourceSliceReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch l := list.(type) {
	case *corev1.NodeList:
		l.Items = append([]corev1.Node(nil), r.nodes...)
	case *resourcev1.ResourceSliceList:
		if r.listErr != nil {
			return r.listErr
		}
		l.Items = append([]resourcev1.ResourceSlice(nil), r.slices...)
	}
	return nil
}

func (r resourceSliceReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return nil
}

// TestAugmentGPUProductLabels exercises augmentGPUProductLabels through
// discoverTargetNodes's public behavior: nodes on a DRA-only platform carry
// no nvidia.com/gpu.product label, so architecture has to fall back to the
// productName attribute on the node's gpu.nvidia.com ResourceSlice.
func TestAugmentGPUProductLabels(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "augment-gpu-product-labels",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Nodes []struct {
				Name       string `yaml:"name"`
				GPUProduct string `yaml:"gpuProduct"`
			} `yaml:"nodes"`
			ResourceSlices []struct {
				NodeName string `yaml:"nodeName"`
				Driver   string `yaml:"driver"`
				Devices  []struct {
					Type                 string `yaml:"type"`
					ProductName          string `yaml:"productName"`
					MalformedProductName bool   `yaml:"malformedProductName"`
				} `yaml:"devices"`
			} `yaml:"resourceSlices"`
			ListError bool `yaml:"listError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		given := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			labels := map[string]string{GPUNodeLabel: present}
			if n.GPUProduct != "" {
				labels[testGPUProductLabel] = n.GPUProduct
			}
			given = append(given, corev1.Node{Name: n.Name, Labels: labels})
		}

		slices := make([]resourcev1.ResourceSlice, 0, len(input.ResourceSlices))
		for _, s := range input.ResourceSlices {
			nodeName := s.NodeName
			devices := make([]resourcev1.Device, 0, len(s.Devices))
			for i, d := range s.Devices {
				// Literal keys, not the production constants, so renaming an
				// attribute in the code breaks these cases instead of silently
				// following the rename.
				attrs := map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}
				if d.Type != "" {
					attrs["type"] = resourcev1.DeviceAttribute{StringValue: &d.Type}
				}
				switch {
				case d.MalformedProductName:
					attrs["productName"] = resourcev1.DeviceAttribute{IntValue: new(int64(1))}
				case d.ProductName != "":
					attrs["productName"] = resourcev1.DeviceAttribute{StringValue: &d.ProductName}
				}
				devices = append(devices, resourcev1.Device{Name: fmt.Sprintf("dev%d", i), Attributes: attrs})
			}
			slices = append(slices, resourcev1.ResourceSlice{
				Spec: resourcev1.ResourceSliceSpec{
					Driver:   s.Driver,
					NodeName: &nodeName,
					Devices:  devices,
				},
			})
		}

		reader := resourceSliceReader{nodes: given, slices: slices}
		if input.ListError {
			reader.listErr = errors.New("resourceslices forbidden")
		}

		nodes, _, _, err := discoverTargetNodes(context.Background(), reader, nil, &nvcrev1alpha1.TargetSpec{})
		if err != nil {
			return err
		}

		type nodeResult struct {
			Name       string `json:"name"`
			GPUProduct string `json:"gpuProduct"`
		}
		results := make([]nodeResult, 0, len(nodes))
		for i := range nodes {
			results = append(results, nodeResult{
				Name:       nodes[i].Name,
				GPUProduct: nodes[i].Labels[testGPUProductLabel],
			})
		}

		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// TestCountDRAGPUs pins which gpu.nvidia.com ResourceSlice devices count as a
// node's GPUs. Every device carries productName, as VFIO devices do upstream,
// so only the type attribute can tell a full GPU apart.
func TestCountDRAGPUs(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "count-dra-gpus",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			ResourceSlices []struct {
				NodeName   string   `yaml:"nodeName"`
				Driver     string   `yaml:"driver"`
				Pool       string   `yaml:"pool"`
				Generation int64    `yaml:"generation"`
				Devices    []string `yaml:"devices"`
			} `yaml:"resourceSlices"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		productName := "NVIDIA GB300"
		slices := make([]resourcev1.ResourceSlice, 0, len(input.ResourceSlices))
		for _, s := range input.ResourceSlices {
			rs := resourcev1.ResourceSlice{
				Spec: resourcev1.ResourceSliceSpec{
					Driver: s.Driver,
					Pool:   resourcev1.ResourcePool{Name: s.Pool, Generation: s.Generation},
				},
			}
			if s.NodeName != "" {
				rs.Spec.NodeName = &s.NodeName
			}
			for i, deviceType := range s.Devices {
				rs.Spec.Devices = append(rs.Spec.Devices, resourcev1.Device{
					Name: fmt.Sprintf("dev%d", i),
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						productNameAttribute: {StringValue: &productName},
						deviceTypeAttribute:  {StringValue: &deviceType},
					},
				})
			}
			slices = append(slices, rs)
		}

		counts, err := CountDRAGPUs(context.Background(), resourceSliceReader{slices: slices})
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(counts, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

func TestCountDRAGPUsReturnsListError(t *testing.T) {
	listErr := errors.New("resourceslices forbidden")
	_, err := CountDRAGPUs(context.Background(), resourceSliceReader{listErr: listErr})
	if !errors.Is(err, listErr) {
		t.Fatalf("CountDRAGPUs error = %v, want %v", err, listErr)
	}
}
