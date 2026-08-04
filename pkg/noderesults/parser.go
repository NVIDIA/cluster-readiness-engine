// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderesults

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	corev1 "k8s.io/api/core/v1"

	burninv1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	gzip "github.com/NVIDIA/cluster-readiness-engine/pkg/controller/compress"
)

const (
	// SucceededNodesConfigMapKey is the binaryData key whose value is a
	// gzip-compressed comma-separated list of nodes that passed.
	SucceededNodesConfigMapKey = "succeeded-nodes.csv.gz"

	// FailedNodesConfigMapKey is the binaryData key whose value is a
	// gzip-compressed JSON array of {name, reason, message} objects.
	FailedNodesConfigMapKey = "failed-nodes.json.gz"
)

// DecodeFailedNodesFromConfigMap parses the failed-nodes entry (a JSON array of
// {name, reason, message} objects) from a node-results ConfigMap's binaryData.
func DecodeFailedNodesFromConfigMap(cm *corev1.ConfigMap) ([]burninv1alpha1.FailedNode, error) {
	if cm == nil {
		return nil, nil
	}
	raw := cm.BinaryData[FailedNodesConfigMapKey]
	if len(raw) == 0 {
		slog.Debug("ConfigMap has no failed-nodes data")
		return nil, nil
	}
	decoded, err := gzip.GunzipString(raw)
	if err != nil {
		slog.Error("Failed to gunzip file",
			"configmap", cm.Name, "error", err, "file", FailedNodesConfigMapKey)
		return nil, fmt.Errorf("failed to decode failed-nodes entry: %w", err)
	}
	return FailedNodesFromJSON([]byte(decoded))
}

// DecodeSucceededNodesFromConfigMap reads the succeeded-nodes.csv.gz from a ConfigMap
// and returns the list of passed node names.
func DecodeSucceededNodesFromConfigMap(cm *corev1.ConfigMap) ([]string, error) {
	if cm == nil {
		return nil, nil
	}

	raw := cm.BinaryData[SucceededNodesConfigMapKey]
	if len(raw) == 0 {
		slog.Debug("ConfigMap has no succeededNodes data")
		return nil, nil
	}

	decoded, err := gzip.GunzipString(raw)
	if err != nil {
		slog.Error("Failed to gunzip file",
			"configmap", cm.Name, "error", err, "file", SucceededNodesConfigMapKey)
		return nil, fmt.Errorf("failed to gunzip file: %w", err)
	}

	return succeededNodesFromCSV(decoded), nil
}

// succeededNodesFromCSV splits a gzip-compressed comma-separated list of node names into a list of strings.
func succeededNodesFromCSV(raw string) []string {
	return strings.Split(raw, ",")
}

// FailedNodesToJSON encodes FailedNode entries as a JSON array of
// {name, reason, message} objects.
func FailedNodesToJSON(nodes []burninv1alpha1.FailedNode) ([]byte, error) {
	if nodes == nil {
		nodes = []burninv1alpha1.FailedNode{}
	}
	b, err := json.Marshal(nodes)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal failed nodes to JSON: %w", err)
	}
	return b, nil
}

// FailedNodesFromJSON parses the JSON array produced by FailedNodesToJSON back into
// FailedNode entries.
func FailedNodesFromJSON(b []byte) ([]burninv1alpha1.FailedNode, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var out []burninv1alpha1.FailedNode
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("failed to unmarshal failed nodes from JSON: %w", err)
	}
	return out, nil
}
