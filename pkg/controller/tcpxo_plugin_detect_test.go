// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// podReader serves a fixed pod list, filtered by namespace, the way the
// manager cache and the CLI's uncached client both do for InNamespace.
type podReader struct {
	pods    []corev1.Pod
	listErr error
}

func (r podReader) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if r.listErr != nil {
		return r.listErr
	}
	l, ok := list.(*corev1.PodList)
	if !ok {
		return nil
	}
	var lo client.ListOptions
	lo.ApplyOptions(opts)
	for _, p := range r.pods {
		if lo.Namespace == "" || p.Namespace == lo.Namespace {
			l.Items = append(l.Items, p)
		}
	}
	return nil
}

func (r podReader) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return nil
}

// The GCP H100 workload and tcpxo-daemon images follow the TCPXO plugin
// release GKE installed (issues #438, #439). These cases pin the detection
// rule: the gate (gcp + h100) keeps other platforms untouched; only Running,
// non-terminating installer pods in kube-system on a target node count; the
// plugin image is matched by its last path segment and its tag read through
// an optional digest; and a release is reported only when every target node
// runs an installer and all installers on the target nodes name the same
// release, a build suffix aside. Only a mapped release is exact; anything
// else carries the message the controller emits as a TCPXOPluginDetection
// event.
func TestResolveTCPXOPluginVersion(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "detect-tcpxo-plugin-version",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Platform        string   `yaml:"platform"`
			GPUArchitecture string   `yaml:"gpuArchitecture"`
			Nodes           []string `yaml:"nodes"`
			Pods            []struct {
				Name        string   `yaml:"name"`
				Namespace   string   `yaml:"namespace"`
				NodeName    string   `yaml:"nodeName"`
				Phase       string   `yaml:"phase"`
				Terminating bool     `yaml:"terminating"`
				InitImages  []string `yaml:"initImages"`
				Images      []string `yaml:"images"`
			} `yaml:"pods"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input); err != nil {
			return err
		}

		nodes := make([]corev1.Node, 0, len(input.Nodes))
		for _, n := range input.Nodes {
			nodes = append(nodes, corev1.Node{Name: n})
		}
		pods := make([]corev1.Pod, 0, len(input.Pods))
		for _, in := range input.Pods {
			pod := corev1.Pod{
				Name:      in.Name,
				Namespace: in.Namespace,
				Spec:      corev1.PodSpec{NodeName: in.NodeName},
				Status:    corev1.PodStatus{Phase: corev1.PodPhase(in.Phase)},
			}
			if pod.Namespace == "" {
				pod.Namespace = tcpxoInstallerNamespace
			}
			if pod.Status.Phase == "" {
				pod.Status.Phase = corev1.PodRunning
			}
			if in.Terminating {
				now := metav1.Now()
				pod.DeletionTimestamp = &now
			}
			for i, img := range in.InitImages {
				pod.Spec.InitContainers = append(pod.Spec.InitContainers,
					corev1.Container{Name: "init-" + string(rune('a'+i)), Image: img})
			}
			for i, img := range in.Images {
				pod.Spec.Containers = append(pod.Spec.Containers,
					corev1.Container{Name: "c-" + string(rune('a'+i)), Image: img})
			}
			pods = append(pods, pod)
		}

		d := resolveTCPXOPluginVersion(context.Background(), podReader{pods: pods}, nil,
			input.Platform, input.GPUArchitecture, nodes)

		out := struct {
			DetectionRan bool   `json:"detectionRan"`
			Version      string `json:"version"`
			Exact        bool   `json:"exact"`
			// Message is set exactly when the controller emits the
			// TCPXOPluginDetection event / the dry-run note: detection ran
			// and found no mapped release, so a fallback renders.
			Message string `json:"message,omitempty"`
		}{DetectionRan: d.Ran, Version: d.Version, Exact: d.Exact}
		if d.Ran && !d.Exact {
			out.Message = tcpxoPluginDetectionMessage(d)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(b) + "\n"
		return nil
	})
}

// A failed List is a refusal, never an error: the Certification still renders
// with the catalog default images.
func TestResolveTCPXOPluginVersionListError(t *testing.T) {
	listErr := errors.New(`pods is forbidden: cannot list resource "pods" in namespace "kube-system"`)
	d := resolveTCPXOPluginVersion(context.Background(), podReader{listErr: listErr}, nil,
		"gcp", "h100", []corev1.Node{{Name: testNodeA}})
	if !d.Ran || d.Version != "" || !errors.Is(d.ListErr, listErr) {
		t.Fatalf("got Ran=%v Version=%q ListErr=%v, want a refusal carrying the List error", d.Ran, d.Version, d.ListErr)
	}
	if msg := tcpxoPluginDetectionMessage(d); !strings.Contains(msg, "could not list kube-system pods") {
		t.Fatalf("message %q does not name the List failure", msg)
	}
}

// countingPodReader records how often it is listed.
type countingPodReader struct {
	podReader
	lists *int
}

func (r countingPodReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	*r.lists++
	return r.podReader.List(ctx, list, opts...)
}

// A cache that has not caught up with the installer pods must not pin the
// fallback images: the refusal is confirmed against the live reader, and the
// live result wins. A resolved cached read never reaches the live reader.
func TestResolveTCPXOPluginVersionConfirmsRefusalLive(t *testing.T) {
	const version = "v1.0.17"
	nodes := []corev1.Node{{Name: testNodeA}}
	installer := corev1.Pod{
		Name:      "nccl-tcpxo-installer-aaaaa",
		Namespace: tcpxoInstallerNamespace,
		Spec: corev1.PodSpec{
			NodeName: testNodeA,
			InitContainers: []corev1.Container{{
				Name:  "nccl-tcpxo-installer",
				Image: "us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:" + version,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	ctx := context.Background()

	liveLists := 0
	live := countingPodReader{podReader{pods: []corev1.Pod{installer}}, &liveLists}
	d := resolveTCPXOPluginVersion(ctx, podReader{}, live, "gcp", "h100", nodes)
	if d.Version != version || liveLists != 1 {
		t.Fatalf("stale cache: got Version=%q after %d live Lists, want v1.0.17 after 1", d.Version, liveLists)
	}

	liveLists = 0
	d = resolveTCPXOPluginVersion(ctx, podReader{pods: []corev1.Pod{installer}}, live, "gcp", "h100", nodes)
	if d.Version != version || liveLists != 0 {
		t.Fatalf("resolved cache: got Version=%q after %d live Lists, want v1.0.17 after 0", d.Version, liveLists)
	}

	liveLists = 0
	d = resolveTCPXOPluginVersion(ctx, podReader{}, countingPodReader{podReader{}, &liveLists}, "gcp", "h100", nodes)
	if !d.Ran || d.Version != "" || liveLists != 1 || !slices.Equal(d.Missing, []string{testNodeA}) {
		t.Fatalf("absent live: got Ran=%v Version=%q Missing=%v after %d live Lists, want a refusal naming node-a after 1",
			d.Ran, d.Version, d.Missing, liveLists)
	}

	// A failed live List says nothing about the installers, so the cached
	// refusal stands, naming the missing node rather than the List error.
	liveErr := errors.New("etcdserver: request timed out")
	d = resolveTCPXOPluginVersion(ctx, podReader{}, podReader{listErr: liveErr}, "gcp", "h100", nodes)
	if d.ListErr != nil || !slices.Equal(d.Missing, []string{testNodeA}) {
		t.Fatalf("failed live: got ListErr=%v Missing=%v, want the cached refusal naming node-a", d.ListErr, d.Missing)
	}
}

// A cached read that found a release, even one newer than every mapped
// release, is not re-read live: it already renders the right profile, and a
// failed live List must not drop a CUDA 13 plugin to the CUDA 12 minimum.
func TestResolveTCPXOPluginVersionKeepsFoundRelease(t *testing.T) {
	const version = "v1.0.18"
	installer := corev1.Pod{
		Name:      "nccl-tcpxo-installer-aaaaa",
		Namespace: tcpxoInstallerNamespace,
		Spec: corev1.PodSpec{
			NodeName: testNodeA,
			InitContainers: []corev1.Container{{
				Name:  "nccl-tcpxo-installer",
				Image: "us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev:" + version,
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	liveLists := 0
	live := countingPodReader{podReader{listErr: errors.New("etcdserver: request timed out")}, &liveLists}
	d := resolveTCPXOPluginVersion(context.Background(), podReader{pods: []corev1.Pod{installer}}, live,
		"gcp", "h100", []corev1.Node{{Name: testNodeA}})
	if d.Version != version || d.Exact || d.ListErr != nil || liveLists != 0 {
		t.Fatalf("got Version=%q Exact=%v ListErr=%v after %d live Lists, want unmapped %s after 0",
			d.Version, d.Exact, d.ListErr, liveLists, version)
	}
}

func TestTCPXOPluginImageVersion(t *testing.T) {
	const repo = "us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/nccl-plugin-gpudirecttcpx-dev"
	for _, tt := range []struct {
		image   string
		version string
		ok      bool
	}{
		{repo + ":v1.0.17", "v1.0.17", true},
		{repo + ":v1.0.15@sha256:0123abcd", "v1.0.15", true},
		{repo + "@sha256:0123abcd", "", true},
		{"registry.local:5000/mirror/nccl-plugin-gpudirecttcpx-dev:v1.0.16", "v1.0.16", true},
		{"us-docker.pkg.dev/gce-ai-infra/gpudirect-tcpxo/tcpgpudmarxd-dev:v1.0.23", "v1.0.23", false},
		{"ubuntu", "", false},
	} {
		version, ok := tcpxoPluginImageVersion(tt.image)
		if version != tt.version || ok != tt.ok {
			t.Errorf("tcpxoPluginImageVersion(%q) = (%q, %v), want (%q, %v)", tt.image, version, ok, tt.version, tt.ok)
		}
	}
}
