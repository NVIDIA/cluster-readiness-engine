// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

type cleanupFailureClient struct {
	client.WithWatch
	mode     string
	deleting bool
}

func (c *cleanupFailureClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	if _, ok := obj.(*nvcrev1alpha1.Certification); ok {
		c.deleting = true
		if c.mode == "delete-error" {
			return errors.New("deletion denied")
		}
		if c.mode == "read-error" {
			return nil
		}
	}
	return c.WithWatch.Delete(ctx, obj, opts...)
}

func (c *cleanupFailureClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*nvcrev1alpha1.Certification); ok && c.deleting && c.mode == "read-error" {
		return errors.New("API unavailable while waiting for deletion")
	}
	return c.WithWatch.Get(ctx, key, obj, opts...)
}

func TestCertificationCleanupBarrier(t *testing.T) {
	parser := &testutil.TestCaseParser{Subdir: "cleanup-barrier", ExpectedSuffix: testutil.SuffixJSON}
	parser.TestDir(t, func(tc *testutil.TestCase) error {
		var input struct {
			Mode string `json:"mode"`
		}
		require.NoError(tc.T, yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &input))
		base := newCertificationFakeClient(tc.T)
		wc := &cleanupFailureClient{WithWatch: base, mode: input.Mode}
		cert := &nvcrev1alpha1.Certification{Name: "cleanup-test", Namespace: testCertNamespace,
			Spec: nvcrev1alpha1.CertificationSpec{Categories: []nvcrev1alpha1.CertificateCategory{{Domain: testDomainCommunication, Variant: testVariantNCCLAllReduce}}},
		}
		var out bytes.Buffer
		err := executeCertificationRun(&certRunConfig{cert: cert, namespace: testCertNamespace, doCleanup: true, out: &out, watchClient: wc})
		result := struct {
			Error                  string `json:"error,omitempty"`
			NamespacePreserved     bool   `json:"namespacePreserved"`
			CertificationPreserved bool   `json:"certificationPreserved"`
		}{}
		if err != nil {
			result.Error = err.Error()
		}
		result.NamespacePreserved = base.Get(context.Background(), client.ObjectKey{Name: testCertNamespace}, &corev1.Namespace{}) == nil
		result.CertificationPreserved = base.Get(context.Background(), client.ObjectKeyFromObject(cert), &nvcrev1alpha1.Certification{}) == nil
		actual, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(actual) + "\n"
		return nil
	})
}

// A timeout must propagate to the caller instead of authorizing namespace/reset
// cleanup while a controller finalizer is still preserving report inputs.
func TestWaitForDeletionTimeout(t *testing.T) {
	cert := &nvcrev1alpha1.Certification{Name: "held", Namespace: testCertNamespace}
	c := newCertificationFakeClient(t, cert)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	require.ErrorIs(t, waitForDeletion(ctx, c, cert.Name, cert.Namespace, &out), context.DeadlineExceeded)
}
