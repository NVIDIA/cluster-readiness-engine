// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// These fixtures exercise the public strict path and its normal report output.
// Read failures must never turn into a successful report with empty metrics;
// early execution failures, however, legitimately have no workload or metrics.
func TestBuildSnapshot(t *testing.T) {
	p := testutil.TestCaseParser{Subdir: "build-snapshot", ExpectedSuffix: testutil.SuffixJSON}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			return err
		}
		if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
			return err
		}
		objs, _, err := tc.GetObjects(scheme)
		if err != nil {
			return err
		}
		var config snapshotTestConfig
		if err := yaml.Unmarshal([]byte(tc.Inputs["input_config.yaml"]), &config); err != nil {
			return err
		}
		var cert *nvcrev1alpha1.Certification
		for _, obj := range objs {
			if candidate, ok := obj.(*nvcrev1alpha1.Certification); ok {
				cert = candidate.DeepCopy()
			}
		}
		if cert == nil {
			return errors.New("fixture has no Certification")
		}
		if config.SourceUID != "" {
			cert.UID = config.SourceUID
		}
		if config.SourceResourceVersion != "" {
			cert.ResourceVersion = config.SourceResourceVersion
		}
		c := &snapshotTestClient{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
			config: config,
			reads:  make(map[string]int),
		}
		result, buildErr := BuildSnapshot(context.Background(), c, cert)
		out := struct {
			Report         *CertReport    `json:"report,omitempty"`
			Error          string         `json:"error,omitempty"`
			NotReady       bool           `json:"notReady"`
			SourceNotFinal bool           `json:"sourceNotFinal"`
			Reads          map[string]int `json:"reads,omitempty"`
		}{Report: result, NotReady: errors.Is(buildErr, ErrSnapshotNotReady), SourceNotFinal: errors.Is(buildErr, ErrSourceNotFinal)}
		if buildErr != nil {
			out.Error = buildErr.Error()
		}
		if config.RejectRepeatedReads {
			out.Reads = c.reads
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}

type snapshotTestConfig struct {
	SourceUID             types.UID `json:"sourceUID"`
	SourceResourceVersion string    `json:"sourceResourceVersion"`
	GetErrorKind          string    `json:"getErrorKind"`
	ListErrorKind         string    `json:"listErrorKind"`
	RejectRepeatedReads   bool      `json:"rejectRepeatedReads"`
}

type snapshotTestClient struct {
	client.Client
	config snapshotTestConfig
	reads  map[string]int
}

func (c *snapshotTestClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	kind := reflect.TypeOf(obj).Elem().Name()
	if _, ok := obj.(*batchv1.Job); ok {
		kind = "BatchJob"
	}
	read := fmt.Sprintf("get %s %s", kind, key)
	c.reads[read]++
	if kind == c.config.GetErrorKind || (c.config.RejectRepeatedReads && c.reads[read] > 1) {
		return fmt.Errorf("injected read failure: %s", read)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *snapshotTestClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	kind := reflect.TypeOf(list).Elem().Name()
	read := "list " + kind
	c.reads[read]++
	if kind == c.config.ListErrorKind || (c.config.RejectRepeatedReads && c.reads[read] > 1) {
		return fmt.Errorf("injected read failure: %s", read)
	}
	return c.Client.List(ctx, list, opts...)
}
