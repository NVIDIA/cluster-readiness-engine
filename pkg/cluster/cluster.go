// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/kubeconfig"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/render"
)

const outputJSON = "json"

// Network topology label keys per cloud platform.
const (
	topologyKeyAWS   = "topology.k8s.aws/network-node-layer-1"
	topologyKeyGCP   = "cloud.google.com/gce-topology-block"
	topologyKeyAzure = "kubernetes.azure.com/ppg"

	platformAWS   = "aws"
	platformGCP   = "gcp"
	platformAzure = "azure"
)

// ClusterInfo is the JSON/YAML output structure for ncrectl cluster info.
type ClusterInfo struct {
	Platform        string        `json:"platform"`
	GPUArchitecture string        `json:"gpuArchitecture"`
	GPUProduct      string        `json:"gpuProduct"`
	GpusPerNode     int32         `json:"gpusPerNode"`
	TotalNodes      int           `json:"totalNodes"`
	TotalGPUs       int           `json:"totalGPUs"`
	Nodes           []NodeInfo    `json:"nodes"`
	Topology        *TopologyInfo `json:"topology,omitempty"`
}

// NodeInfo describes a single GPU node.
type NodeInfo struct {
	Name  string `json:"name"`
	GPUs  int32  `json:"gpus"`
	Ready bool   `json:"ready"`
	Rack  string `json:"rack,omitempty"`
}

// TopologyInfo describes the network topology grouping.
type TopologyInfo struct {
	Key   string     `json:"key"`
	Racks []RackInfo `json:"racks"`
}

// RackInfo describes a single topology domain (rack / T1 leaf switch).
type RackInfo struct {
	Name      string   `json:"name"`
	Nodes     []string `json:"nodes"`
	NodeCount int      `json:"nodeCount"`
}

// NewCommand returns the "cluster" cobra command.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster inspection commands",
	}
	cmd.AddCommand(newClusterInfoCommand())
	return cmd
}

func newClusterInfoCommand() *cobra.Command {
	var (
		output      string
		topologyKey string
	)

	configFlags := kubeconfig.NewConfigFlags(true)
	configFlags.Namespace = nil

	cmd := &cobra.Command{
		Use:   "info",
		Short: "Discover GPU nodes, platform, and network topology",
		Long: `Discover GPU nodes in the cluster and display platform, GPU architecture,
and network topology (rack/T1 leaf switch grouping).

The topology key is auto-detected per platform:
  AWS:   topology.k8s.aws/network-node-layer-1
  GCP:   cloud.google.com/gce-topology-block
  Azure: kubernetes.azure.com/ppg

Use --topology-key to override.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			c, err := render.NewK8sClient(configFlags)
			if err != nil {
				return err
			}

			nodes, gpuProduct, err := DiscoverGPUNodes(ctx, c, nil)
			if err != nil {
				return err
			}

			platform := controller.DetectPlatform(nodes)
			gpuArch := controller.DetectGPUArchitecture(nodes)
			gpusPerNode := catalog.GPUDefaults(gpuArch, platform).GpusPerNode

			// Auto-detect topology key if not specified.
			if topologyKey == "" {
				topologyKey = DefaultTopologyKey(platform)
			}

			info := buildClusterInfo(nodes, platform, gpuArch, gpuProduct, gpusPerNode, topologyKey)

			switch output {
			case outputJSON:
				return printClusterJSON(info)
			case "yaml":
				return printClusterYAML(info)
			default:
				return printClusterTable(info)
			}
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "table",
		"Output format: table, json, yaml")
	cmd.Flags().StringVar(&topologyKey, "topology-key", "",
		"Node label for topology grouping (auto-detected per platform if omitted)")
	configFlags.AddFlags(cmd.Flags())

	return cmd
}

// DefaultTopologyKey returns the network topology label for the detected platform.
func DefaultTopologyKey(plat string) string {
	switch plat {
	case platformAWS:
		return topologyKeyAWS
	case platformGCP:
		return topologyKeyGCP
	case platformAzure:
		return topologyKeyAzure
	default:
		return ""
	}
}

// buildClusterInfo constructs a ClusterInfo from discovered nodes.
func buildClusterInfo(
	nodes []corev1.Node, platform, gpuArch, gpuProduct string,
	gpusPerNode int32, topologyKey string,
) ClusterInfo {
	info := ClusterInfo{
		Platform:        platform,
		GPUArchitecture: gpuArch,
		GPUProduct:      gpuProduct,
		GpusPerNode:     gpusPerNode,
		TotalNodes:      len(nodes),
		TotalGPUs:       len(nodes) * int(gpusPerNode),
	}

	// Build per-node info and collect topology domains.
	rackNodes := map[string][]string{}
	for _, n := range nodes {
		ni := NodeInfo{
			Name:  n.Name,
			GPUs:  gpusPerNode,
			Ready: isNodeReady(n),
		}
		if topologyKey != "" {
			ni.Rack = n.Labels[topologyKey]
			if ni.Rack != "" {
				rackNodes[ni.Rack] = append(rackNodes[ni.Rack], n.Name)
			}
		}
		info.Nodes = append(info.Nodes, ni)
	}

	// Sort nodes by name for deterministic output.
	sort.Slice(info.Nodes, func(i, j int) bool {
		return info.Nodes[i].Name < info.Nodes[j].Name
	})

	// Build topology if we have a key and found domains.
	if topologyKey != "" && len(rackNodes) > 0 {
		info.Topology = &TopologyInfo{Key: topologyKey}
		for name, nodeNames := range rackNodes {
			sort.Strings(nodeNames)
			info.Topology.Racks = append(info.Topology.Racks, RackInfo{
				Name:      name,
				Nodes:     nodeNames,
				NodeCount: len(nodeNames),
			})
		}
		sort.Slice(info.Topology.Racks, func(i, j int) bool {
			return info.Topology.Racks[i].Name < info.Topology.Racks[j].Name
		})
	}

	return info
}

// isNodeReady returns true if the node has a Ready condition with status True.
func isNodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func printClusterJSON(info ClusterInfo) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(info)
}

func printClusterYAML(info ClusterInfo) error {
	data, err := sigyaml.Marshal(info)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func printClusterTable(info ClusterInfo) error {
	fmt.Printf("Platform:     %s\n", info.Platform)
	fmt.Printf("GPU:          %s (%s, %d GPUs/node)\n", info.GPUProduct, info.GPUArchitecture, info.GpusPerNode)

	readyCount := 0
	for _, n := range info.Nodes {
		if n.Ready {
			readyCount++
		}
	}
	fmt.Printf("Nodes:        %d ready\n", readyCount)

	if info.Topology != nil && len(info.Topology.Racks) > 0 {
		fmt.Printf("\nTOPOLOGY (%s):\n", info.Topology.Key)
		w := tabwriter.NewWriter(os.Stdout, 2, 0, 4, ' ', 0)
		for _, rack := range info.Topology.Racks {
			rackName := rack.Name
			if len(rackName) > 24 {
				rackName = rackName[:21] + "..."
			}
			_, _ = fmt.Fprintf(w, "  %s\t%d nodes\n", rackName, rack.NodeCount)
		}
		_ = w.Flush()
	} else {
		topoNote := ""
		switch info.Platform {
		case platformAWS:
			topoNote = " (expected: " + topologyKeyAWS + ")"
		case platformGCP:
			topoNote = " (expected: " + topologyKeyGCP + ")"
		case platformAzure:
			topoNote = " (expected: " + topologyKeyAzure + ")"
		}
		if topoNote != "" {
			fmt.Printf("\nTopology:     no topology labels found%s\n", topoNote)
		}
	}

	return nil
}
