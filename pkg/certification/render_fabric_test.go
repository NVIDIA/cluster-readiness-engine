// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/gpu"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// fabricSliceReader serves ResourceSlices to resolveDryRunFabric, or refuses
// the list the way an API server refuses a kubeconfig without the grant.
type fabricSliceReader struct {
	slices  []resourcev1.ResourceSlice
	refused bool
}

func (r fabricSliceReader) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	l, ok := list.(*resourcev1.ResourceSliceList)
	if !ok {
		return errors.New("unexpected list type")
	}
	if r.refused {
		return apierrors.NewForbidden(
			schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "",
			errors.New("user cannot list resourceslices"))
	}
	l.Items = append([]resourcev1.ResourceSlice(nil), r.slices...)
	return nil
}

func (r fabricSliceReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("unexpected get")
}

// TestResolveDryRunFabric pins that "certification render --dry-run" resolves
// the fabric the way the controller does (ADR-110): on the nodes the job can
// run on (the primary architecture's nodes with enough GPUs), and with the
// user's kubeconfig, where a refused ResourceSlice list keeps an explicit roce
// with a warning instead of failing the preview.
func TestResolveDryRunFabric(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-dry-run-fabric",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Fabric       string `yaml:"fabric"`
			PlatformFlag string `yaml:"platformFlag"`
			GpusPerNode  *int32 `yaml:"gpusPerNode"`
			Nodes        []struct {
				Name        string            `yaml:"name"`
				Product     string            `yaml:"product"`
				Allocatable map[string]string `yaml:"allocatable"`
			} `yaml:"nodes"`
			// RoCENodes publish one RoCE device each from the AWS
			// networking DRA driver.
			RoCENodes   []string `yaml:"roceNodes"`
			ListRefused bool     `yaml:"listRefused"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{
				Name: n.Name, Labels: map[string]string{gpu.ProductLabel: n.Product},
				Spec:   corev1.NodeSpec{ProviderID: "aws://us-east-1a/i-" + n.Name},
				Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}},
			}
			for res, qty := range n.Allocatable {
				parsed, err := resource.ParseQuantity(qty)
				if err != nil {
					return err
				}
				node.Status.Allocatable[corev1.ResourceName(res)] = parsed
			}
			nodes = append(nodes, node)
		}
		reader := fabricSliceReader{refused: input.ListRefused}
		for _, name := range input.RoCENodes {
			reader.slices = append(reader.slices, resourcev1.ResourceSlice{Spec: resourcev1.ResourceSliceSpec{
				Driver:   "dra.networking.k8s.aws",
				NodeName: new(name),
				Pool:     resourcev1.ResourcePool{Name: name, Generation: 1},
				Devices: []resourcev1.Device{{
					Name: "rocep201s0",
					Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{
						"dra.vpc.amazonaws.com/deviceType": {StringValue: new("roce")},
					},
				}},
			}})
		}

		cert := &nvcrev1alpha1.Certification{}
		cert.Spec.Fabric = nvcrev1alpha1.Fabric(input.Fabric)
		cert.Spec.GpusPerNode = input.GpusPerNode

		var stderr bytes.Buffer
		fabric, err := resolveDryRunFabric(context.Background(), reader, cert, nodes, input.PlatformFlag, &stderr)
		out := struct {
			Fabric     string `json:"fabric"`
			Source     string `json:"source"`
			EFAPerNode int32  `json:"efaPerNode"`
			Surface    bool   `json:"surface"`
			Stderr     string `json:"stderr,omitempty"`
			Error      string `json:"error,omitempty"`
		}{Fabric: fabric.Fabric, Source: fabric.Source, EFAPerNode: fabric.EFAPerNode, Surface: fabric.Surface,
			Stderr: stderr.String()}
		if err != nil {
			out.Error = err.Error()
		}
		data, mErr := json.MarshalIndent(out, "", "  ")
		if mErr != nil {
			return mErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
