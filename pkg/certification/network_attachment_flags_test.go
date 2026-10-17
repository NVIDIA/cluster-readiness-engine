// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package certification

import (
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// TestParseNetworkAttachmentFlags pins how the run command's attachment flags
// become a networkAttachments list: network=resource[:count] entries, the
// issue #400 zipped --multus-networks/--extended-resources pair (including
// the length-mismatch error), the two forms being exclusive, and the CRD
// rules applied before anything is created.
func TestParseNetworkAttachmentFlags(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "parse-network-attachment-flags",
		ExpectedSuffix: testutil.SuffixJSON,
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		var in struct {
			NetworkAttachments []string `json:"networkAttachments"`
			MultusNetworks     []string `json:"multusNetworks"`
			ExtendedResources  []string `json:"extendedResources"`
		}
		if err := yaml.Unmarshal([]byte(tc.Inputs["input.yaml"]), &in); err != nil {
			return err
		}
		atts, err := parseNetworkAttachmentFlags(in.NetworkAttachments, in.MultusNetworks, in.ExtendedResources)
		result := struct {
			NetworkAttachments []nvcrev1alpha1.NetworkAttachment `json:"networkAttachments,omitempty"`
			Error              string                            `json:"error,omitempty"`
		}{NetworkAttachments: atts}
		if err != nil {
			result.Error = err.Error()
		}
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		tc.Actual = string(data) + "\n"
		return nil
	})
}
