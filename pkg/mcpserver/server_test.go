// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nvcrev1alpha1 "github.com/NVIDIA/cluster-readiness-engine/api/v1alpha1"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/catalog"
	"github.com/NVIDIA/cluster-readiness-engine/pkg/testutil"
)

// connect builds a server on top of a fake client and drives a full MCP
// session against it over in-memory transports, so the tests exercise the
// same protocol surface an agent sees (initialize, tools/list, tools/call).
func connect(t *testing.T, store *Store) *mcp.ClientSession {
	t.Helper()
	server := New(store, "test")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// fakeStore builds a Store backed by a controller-runtime fake client holding
// the test case's input objects, mirroring the pkg/report golden tests.
func fakeStore(t *testing.T, tc *testutil.TestCase) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objs, _, err := tc.GetObjects(scheme)
	if err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		builder = builder.WithObjects(o)
	}
	return &Store{Client: builder.Build()}
}

// callTool invokes the named tool with args and returns the parsed JSON of
// its text content.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned tool error: %v", name, res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: want 1 content item, got %d", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s: content is %T, want TextContent", name, res.Content[0])
	}
	// Round-trip through a generic map so the golden file records the parsed
	// JSON object rather than an escaped string.
	var v any
	if err := json.Unmarshal([]byte(text.Text), &v); err != nil {
		t.Fatalf("%s: text content is not JSON: %v", name, err)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(pretty)
}

// TestMCPTools exercises every tool against the golden files under
// testdata/mcp-tools. Each case directory holds the input cluster objects
// (input_client_objects.yaml) and the tool calls to run in input_calls.json;
// the golden file records the JSON responses in order.
func TestMCPTools(t *testing.T) {
	p := testutil.TestCaseParser{
		Subdir:         "mcp-tools",
		ExpectedSuffix: ".txt",
	}
	p.TestDir(t, func(tc *testutil.TestCase) error {
		store := fakeStore(t, tc)
		session := connect(t, store)

		var calls []struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments,omitempty"`
		}
		if err := json.Unmarshal([]byte(tc.Inputs["input_calls.json"]), &calls); err != nil {
			return fmt.Errorf("parse input_calls.json: %w", err)
		}

		var out strings.Builder
		for _, c := range calls {
			fmt.Fprintf(&out, "### %s\n", c.Tool)
			fmt.Fprintf(&out, "%s\n", callTool(t, session, c.Tool, c.Arguments))
		}
		tc.Actual = out.String()
		return nil
	})
}

// emptyStore returns a Store backed by a fake client holding no objects and
// no registered catalog categories.
func emptyStore() *Store {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := nvcrev1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	return &Store{
		Client:  fake.NewClientBuilder().WithScheme(scheme).Build(),
		Catalog: func() []catalog.CategoryInfo { return nil },
	}
}

// TestListTools pins the tool surface: exactly the four read-only tools, all
// carrying the readOnlyHint annotation. An extra or mutating tool added by
// mistake fails here.
func TestListTools(t *testing.T) {
	store := emptyStore()
	session := connect(t, store)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q is missing the readOnlyHint annotation", tool.Name)
		}
		got[tool.Name] = true
	}
	for _, want := range []string{
		"list_categories",
		"get_certification_status",
		"get_certification_report",
		"list_failed_nodes",
	} {
		if !got[want] {
			t.Errorf("tool %q is not exposed", want)
		}
	}
	if len(res.Tools) != 4 {
		t.Errorf("got %d tools, want exactly the 4 read-only tools: %v",
			len(res.Tools), res.Tools)
	}
}

// TestNotFound verifies the not-found path of the certification-scoped tools
// returns a tool error mentioning the name and namespace.
func TestNotFound(t *testing.T) {
	store := emptyStore()
	session := connect(t, store)

	for _, tool := range []string{"get_certification_status", "get_certification_report", "list_failed_nodes"} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      tool,
			Arguments: map[string]any{"name": "missing", "namespace": "ns1"},
		})
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !res.IsError {
			t.Fatalf("%s: want tool error for missing certification", tool)
		}
		text, ok := res.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("%s: content is %T, want TextContent", tool, res.Content[0])
		}
		want := `certification "missing" not found in namespace "ns1"`
		if !strings.Contains(text.Text, want) {
			t.Errorf("%s: error %q does not contain %q", tool, text.Text, want)
		}
	}
}
