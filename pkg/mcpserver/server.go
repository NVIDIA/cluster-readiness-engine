// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcpserver exposes NVCRE certification state to MCP (Model Context
// Protocol) agents as a set of read-only tools.
//
// The server answers "did this certification pass, and which nodes failed?"
// from the same typed data sources the nvcrectl report command reads —
// catalog.List, report.Build, and the failed-nodes ConfigMaps — so an agent
// never has to scrape CLI output.
//
// Authentication flows strictly through the caller's kubeconfig (standard
// client-go loading rules: --kubeconfig/--context flags, KUBECONFIG env, then
// ~/.kube/config). The server can therefore never exceed the permissions of
// whoever runs it, and holds no credentials of its own.
//
// The tool set is read-only by design (issue #242): no tool creates, mutates,
// or deletes a resource, and nothing triggers a run — runs consume real GPU
// time. Every tool also carries the MCP readOnlyHint annotation so clients
// can treat the whole surface as non-mutating.
package mcpserver

import (
	"context"
	"fmt"
	"sort"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/controller"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/report"
)

// defaultNamespace matches the nvcrectl default: tools that omit the
// namespace argument resolve against "default".
const defaultNamespace = "default"

// Store holds the cluster-backed data sources behind the MCP tools. Tests
// inject a fake client; production builds the client from the kubeconfig
// flags (see NewCommand).
type Store struct {
	// Client reads Certification, Workflow, and node-results ConfigMap
	// objects. Only read paths are exercised — the handlers call
	// report.Build, report.FailedNodesFromRef, and plain Gets, all of which
	// issue reads against the API server.
	Client client.Client
	// Catalog lists the registered certification categories. A nil Catalog
	// defaults to catalog.List.
	Catalog func() []catalog.CategoryInfo
}

// New returns an MCP server with the four read-only NVCRE tools registered.
// An empty version resolves to "dev".
func New(store *Store, version string) *mcp.Server {
	if store.Catalog == nil {
		store.Catalog = catalog.List
	}
	if version == "" {
		version = "dev"
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "nvcre",
		Title:   "NVIDIA Cluster Readiness Engine",
		Version: version,
	}, &mcp.ServerOptions{
		Instructions: "Read-only access to NVCRE certification state. " +
			"Authentication uses the kubeconfig of whoever launched the server.",
	})

	// Generic mcp.AddTool derives the input/output JSON schemas from the
	// typed handler arguments, validates tool input before it reaches the
	// handler, and packs handler errors into the tool result (IsError=true)
	// instead of failing the protocol call.
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	add0 := func(tool *mcp.Tool, handler mcp.ToolHandlerFor[struct{}, any]) {
		tool.Annotations = readOnly
		mcp.AddTool(s, tool, handler)
	}
	add1 := func(tool *mcp.Tool, handler mcp.ToolHandlerFor[certRef, any]) {
		tool.Annotations = readOnly
		mcp.AddTool(s, tool, handler)
	}

	add0(listCategoriesTool(), listCategoriesHandler(store))
	add1(getCertStatusTool(), getCertStatusHandler(store))
	add1(getCertReportTool(), getCertReportHandler(store))
	add1(listFailedNodesTool(), listFailedNodesHandler(store))
	return s
}

// Run serves the MCP server over stdio until the context is cancelled or the
// client disconnects. Stdio keeps the transport local: the agent spawns the
// server itself, so kubeconfig credentials never leave the machine.
func Run(ctx context.Context, store *Store, version string) error {
	return New(store, version).Run(ctx, &mcp.StdioTransport{})
}

// ---------------------------------------------------------------------------
// Tool descriptors
// ---------------------------------------------------------------------------

func listCategoriesTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "list_categories",
		Description: "List the certification catalog: every registered category (domain/variant) a Certification can run.",
	}
}

func getCertStatusTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "get_certification_status",
		Description: "Get the status of one Certification: overall result (PASSED/FAILED/RUNNING), conditions, per-category state, and failed node names.",
	}
}

func getCertReportTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "get_certification_report",
		Description: "Fetch the full certification report: categories with metrics, bandwidth, cliques, diagnose results, and per-node results.",
	}
}

func listFailedNodesTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "list_failed_nodes",
		Description: "List the failed nodes for a Certification with per-node failure reason and message.",
	}
}

// ---------------------------------------------------------------------------
// Tool handlers — thin adapters over the shared NVCRE data access
// ---------------------------------------------------------------------------

func listCategoriesHandler(store *Store) mcp.ToolHandlerFor[struct{}, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		out := listCategoriesOutput{Categories: []categorySummary{}}
		for _, c := range store.Catalog() {
			out.Categories = append(out.Categories, categorySummary{
				Domain:  c.Domain,
				Variant: c.Variant,
			})
		}
		return textResult(out)
	}
}

func getCertStatusHandler(store *Store) mcp.ToolHandlerFor[certRef, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in certRef) (*mcp.CallToolResult, any, error) {
		cert, err := store.certification(ctx, in)
		if err != nil {
			return nil, nil, err
		}

		out := &getCertStatusOutput{
			Name:        cert.Name,
			Namespace:   cert.Namespace,
			Result:      certResult(cert),
			Conditions:  []conditionInfo{},
			Categories:  []categoryState{},
			FailedNodes: report.CertFailedNodes(ctx, store.Client, cert),
		}
		for _, c := range cert.Status.Conditions {
			out.Conditions = append(out.Conditions, conditionInfo{
				Type:    c.Type,
				Status:  string(c.Status),
				Reason:  c.Reason,
				Message: c.Message,
			})
		}
		for _, cs := range cert.Status.CategoryStatuses {
			out.Categories = append(out.Categories, categoryState{
				Domain:  cs.Domain,
				Variant: cs.Variant,
				Status:  cs.Status,
			})
		}
		return textResult(out)
	}
}

func getCertReportHandler(store *Store) mcp.ToolHandlerFor[certRef, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in certRef) (*mcp.CallToolResult, any, error) {
		cert, err := store.certification(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		// report.Build is the same builder 'nvcrectl certification report'
		// uses; the JSON shape matches --results-file output.
		return textResult(&getCertReportOutput{
			Report: report.Build(ctx, store.Client, cert),
		})
	}
}

func listFailedNodesHandler(store *Store) mcp.ToolHandlerFor[certRef, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in certRef) (*mcp.CallToolResult, any, error) {
		cert, err := store.certification(ctx, in)
		if err != nil {
			return nil, nil, err
		}

		seen := map[string]bool{}
		details := []failedNodeDetail{}
		for _, cat := range cert.Status.CategoryStatuses {
			for _, n := range report.FailedNodesFromRef(ctx, store.Client, cert.Namespace, cat.FailedNodesRef) {
				key := n.Name + "|" + string(n.Reason) + "|" + n.Message
				if n.Name == "" || seen[key] {
					continue
				}
				seen[key] = true
				details = append(details, failedNodeDetail{
					Name:    n.Name,
					Reason:  string(n.Reason),
					Message: n.Message,
				})
			}
		}
		// Deterministic order: node name, then reason, then message.
		sort.Slice(details, func(i, j int) bool {
			a, b := details[i], details[j]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			if a.Reason != b.Reason {
				return a.Reason < b.Reason
			}
			return a.Message < b.Message
		})
		return textResult(&listFailedNodesOutput{
			Name:        cert.Name,
			Namespace:   cert.Namespace,
			FailedNodes: details,
		})
	}
}

// ---------------------------------------------------------------------------
// Shared handler plumbing
// ---------------------------------------------------------------------------

// certRef is the shared input of the three certification-scoped tools.
type certRef struct {
	Name string `json:"name" jsonschema:"name of the Certification resource"`
	// Namespace defaults to "default" when omitted.
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace of the Certification (default: default)"`
}

// listCategoriesOutput lists every registered certification category. The
// catalog is static, so the tool takes no arguments.
type listCategoriesOutput struct {
	Categories []categorySummary `json:"categories"`
}

type categorySummary struct {
	Domain  string `json:"domain"`
	Variant string `json:"variant"`
}

// getCertStatusOutput summarizes a Certification's overall and per-category
// state without pulling measurement data.
type getCertStatusOutput struct {
	Name        string          `json:"name"`
	Namespace   string          `json:"namespace"`
	Result      string          `json:"result"` // "PASSED", "FAILED", or "RUNNING"
	Conditions  []conditionInfo `json:"conditions,omitempty"`
	Categories  []categoryState `json:"categories,omitempty"`
	FailedNodes []string        `json:"failedNodes,omitempty"`
}

type conditionInfo struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type categoryState struct {
	Domain  string `json:"domain"`
	Variant string `json:"variant"`
	Status  string `json:"status"`
}

// getCertReportOutput wraps the shared report model, so agents parse the
// same JSON shape nvcrectl writes with --results-file.
type getCertReportOutput struct {
	Report *report.CertReport `json:"report"`
}

// listFailedNodesOutput returns per-node failure details.
type listFailedNodesOutput struct {
	Name        string             `json:"name"`
	Namespace   string             `json:"namespace"`
	FailedNodes []failedNodeDetail `json:"failedNodes"`
}

type failedNodeDetail struct {
	Name    string `json:"name"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// certification fetches the named Certification, mirroring the nvcrectl
// report command's error messages.
func (s *Store) certification(ctx context.Context, ref certRef) (*nvcrev1alpha1.Certification, error) {
	ns := ref.Namespace
	if ns == "" {
		ns = defaultNamespace
	}
	cert := &nvcrev1alpha1.Certification{}
	key := client.ObjectKey{Name: ref.Name, Namespace: ns}
	if err := s.Client.Get(ctx, key, cert); err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("certification %q not found in namespace %q", ref.Name, ns)
		}
		return nil, fmt.Errorf("get certification %q: %w", ref.Name, err)
	}
	return cert, nil
}

// certResult maps Certification conditions to the report's result strings,
// matching report.Build.
func certResult(cert *nvcrev1alpha1.Certification) string {
	switch {
	case controller.CondIsTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationFailed):
		return "FAILED"
	case controller.CondIsTrue(cert.Status.Conditions, nvcrev1alpha1.CertificationSucceeded):
		return "PASSED"
	default:
		return "RUNNING"
	}
}
