// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/platform"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestResolveFabric pins the ADR-110 resolution order: an explicit
// spec.fabric wins once validated against the nodes (and fails rather than
// switching fabrics); otherwise AWS GB300 is detected from vpc.amazonaws.com/efa
// allocatable and from RoCE devices published by the dra.networking.k8s.aws
// DRA driver; otherwise, and whenever detection is inconclusive, the platform
// default applies. The fixtures use the node and ResourceSlice shapes measured
// on real stock-EKS and RoCE-rack p6e-gb300r clusters (issue #576).
func TestResolveFabric(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-fabric",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Field           string `yaml:"field"`
			Platform        string `yaml:"platform"`
			GPUArchitecture string `yaml:"gpuArchitecture"`
			Nodes           []struct {
				Name        string            `yaml:"name"`
				Allocatable map[string]string `yaml:"allocatable"`
			} `yaml:"nodes"`
			ResourceSlices []struct {
				NodeName   string `yaml:"nodeName"`
				Driver     string `yaml:"driver"`
				Pool       string `yaml:"pool"`
				Generation int64  `yaml:"generation"`
				Devices    []struct {
					Name       string            `yaml:"name"`
					Attributes map[string]string `yaml:"attributes"`
				} `yaml:"devices"`
			} `yaml:"resourceSlices"`
			// ListError is "forbidden", "notFound" or "timeout".
			ListError string `yaml:"listError"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			node := corev1.Node{Name: n.Name, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{}}}
			for res, qty := range n.Allocatable {
				parsed, err := resource.ParseQuantity(qty)
				if err != nil {
					return err
				}
				node.Status.Allocatable[corev1.ResourceName(res)] = parsed
			}
			nodes = append(nodes, node)
		}

		reader := resourceSliceReader{}
		for _, s := range input.ResourceSlices {
			rs := resourcev1.ResourceSlice{Spec: resourcev1.ResourceSliceSpec{
				Driver:   s.Driver,
				NodeName: new(s.NodeName),
				Pool:     resourcev1.ResourcePool{Name: s.Pool, Generation: s.Generation},
			}}
			for _, d := range s.Devices {
				dev := resourcev1.Device{Name: d.Name, Attributes: map[resourcev1.QualifiedName]resourcev1.DeviceAttribute{}}
				for k, v := range d.Attributes {
					dev.Attributes[resourcev1.QualifiedName(k)] = resourcev1.DeviceAttribute{StringValue: new(v)}
				}
				rs.Spec.Devices = append(rs.Spec.Devices, dev)
			}
			reader.slices = append(reader.slices, rs)
		}
		gr := schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}
		switch input.ListError {
		case "":
		case listErrForbidden:
			reader.listErr = apierrors.NewForbidden(gr, "", errors.New("RBAC: access denied"))
		case "notFound":
			reader.listErr = apierrors.NewNotFound(gr, "")
		case "timeout":
			reader.listErr = apierrors.NewTimeoutError("request timed out", 1)
		default:
			return errors.New("unknown listError " + input.ListError)
		}

		res, err := resolveFabric(context.Background(), reader, nvcrev1alpha1.Fabric(input.Field),
			input.Platform, input.GPUArchitecture, nodes)

		out := struct {
			Fabric     string `json:"fabric"`
			Source     string `json:"source"`
			EFAPerNode int32  `json:"efaPerNode"`
			Surface    bool   `json:"surface"`
			// Message is the FabricDetection event text: detection ran and
			// was inconclusive.
			Message string `json:"message,omitempty"`
			// ErrorKind is "validation" for the terminal
			// FabricValidationFailed path and "transient" for a retried list.
			ErrorKind string `json:"errorKind,omitempty"`
			Error     string `json:"error,omitempty"`
		}{
			Fabric: res.Fabric, Source: res.Source, EFAPerNode: res.EFAPerNode,
			Surface: res.Surface, Message: res.Notice,
		}
		if err != nil {
			out.Error = err.Error()
			out.ErrorKind = "transient"
			if _, ok := errors.AsType[*fabricValidationError](err); ok {
				out.ErrorKind = "validation"
			}
		}
		data, mErr := json.MarshalIndent(out, "", "  ")
		if mErr != nil {
			return mErr
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestResolveFabricOverrides pins how a WorkloadRun settles when.fabric in the
// user's overrides before the Workflow, which has no spec.fabric, re-applies
// them: a matching matcher is cleared, a non-matching override is dropped,
// and an override without the matcher is untouched.
func TestResolveFabricOverrides(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "resolve-fabric-overrides",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Fabric    string                       `yaml:"fabric"`
			Overrides []nvcrev1alpha1.OverrideSpec `yaml:"overrides"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}
		before, err := json.Marshal(input.Overrides)
		if err != nil {
			return err
		}
		got := resolveFabricOverrides(input.Overrides, input.Fabric)
		after, err := json.Marshal(input.Overrides)
		if err != nil {
			return err
		}
		if string(before) != string(after) {
			return errors.New("resolveFabricOverrides modified its input")
		}
		data, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

// TestDefaultFabric pins today's platform and architecture mapping, the
// fallback every unset, undetected target renders (ADR-110). A change here
// moves existing renders, which the ADR rules out.
func TestDefaultFabric(t *testing.T) {
	const (
		efa   = string(nvcrev1alpha1.FabricEFA)
		ib    = string(nvcrev1alpha1.FabricIB)
		roce  = string(nvcrev1alpha1.FabricRoCE)
		tcpxo = string(nvcrev1alpha1.FabricTCPXO)
		rtx   = "rtxpro6000"
	)
	for _, tc := range []struct {
		platform, arch, want string
	}{
		{platform.AWS, gpuArchGB300, roce},
		{platform.AWS, gpuArchGB200, efa},
		{platform.AWS, gpuArchH100, efa},
		{platform.AWS, "b300", efa},
		{platform.AWS, rtx, efa},
		{platform.Azure, gpuArchH100, ib},
		{platform.Forge, "l40", ib},
		{platform.TogetherAI, gpuArchH100, ib},
		{platform.Mistral, gpuArchGB300, ib},
		{platform.NScale, gpuArchGB300, ib},
		{platform.OCI, gpuArchGB300, roce},
		{platform.OCI, gpuArchH100, roce},
		{platform.GCP, gpuArchH100, tcpxo},
		{platform.GCP, gpuArchGB200, roce},
		{platform.GCP, gpuArchGB300, roce},
		{platform.GCP, rtx, ""},
		{platform.Akamai, rtx, ""},
		{platform.OnPrem, gpuArchGB300, ""},
		{"", "", ""},
	} {
		if got := defaultFabric(tc.platform, tc.arch); got != tc.want {
			t.Errorf("defaultFabric(%q, %q) = %q, want %q", tc.platform, tc.arch, got, tc.want)
		}
	}
}

// TestWhenFabricMatchesThroughOverrideContext pins that when.fabric is a live
// matcher dimension: the fabric handed to buildOverrideContext reaches
// matchesWhen. when.config shipped without OverrideContext.Config ever being
// assigned and so can never match; this keeps when.fabric from joining it
// (ADR-110).
func TestWhenFabricMatchesThroughOverrideContext(t *testing.T) {
	spec := &nvcrev1alpha1.WorkflowSpec{}
	orch := &nvcrev1alpha1.OrchestrationStatus{DetectedPlatform: testPlatformAWS, DetectedGPUArchitecture: gpuArchGB300}
	when := nvcrev1alpha1.WhenSpec{
		Platform: &nvcrev1alpha1.StringMatchSpec{Equals: testPlatformAWS},
		Fabric:   &nvcrev1alpha1.StringMatchSpec{Equals: string(nvcrev1alpha1.FabricEFA)},
	}
	for _, tc := range []struct {
		fabric string
		want   bool
	}{
		{string(nvcrev1alpha1.FabricEFA), true},
		{string(nvcrev1alpha1.FabricRoCE), false},
		{"", false},
	} {
		got, err := matchesWhen(when, buildOverrideContext(spec, orch, nil, tc.fabric))
		if err != nil {
			t.Fatalf("matchesWhen: %v", err)
		}
		if got != tc.want {
			t.Errorf("when.fabric=efa against fabric %q: matched %v, want %v", tc.fabric, got, tc.want)
		}
	}
}
