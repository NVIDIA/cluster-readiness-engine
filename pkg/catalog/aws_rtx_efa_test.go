// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestAWSRTXEFARequestScalesWithGPUs(t *testing.T) {
	for _, tc := range []struct {
		gpus int32
		efa  string
	}{{1, "1"}, {2, "1"}, {3, "2"}, {4, "2"}, {7, "4"}, {8, "4"}} {
		t.Run(strconv.Itoa(int(tc.gpus)), func(t *testing.T) {
			rendered, err := makeLibTemplate(TemplateFuncsWithLib())(
				"deps/aws-rtxpro6000-efa-resources.yaml", TemplateData{EntryName: "test", GpusPerNode: tc.gpus})
			require.NoError(t, err)
			var deps []map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(rendered), &deps))
			jobs, found, err := unstructured.NestedSlice(deps[0], "spec", "template", "spec", "replicatedJobs")
			require.NoError(t, err)
			require.True(t, found)
			containers, found, err := unstructured.NestedSlice(jobs[0].(map[string]any),
				"template", "spec", "template", "spec", "containers")
			require.NoError(t, err)
			require.True(t, found)
			for _, kind := range []string{"requests", "limits"} {
				count, found, err := unstructured.NestedString(containers[0].(map[string]any), "resources", kind, "vpc.amazonaws.com/efa")
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, tc.efa, count)
			}
		})
	}
}
