// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

func boolPtr(b bool) *bool { return &b }

// mockClient implements sempv2.Client for testing.
type mockClient struct {
	mu        sync.Mutex
	calls     []string
	responses map[string]*sempv2.Result
	errors    map[string]error
}

func newMockClient() *mockClient {
	return &mockClient{
		responses: make(map[string]*sempv2.Result),
		errors:    make(map[string]error),
	}
}

func (m *mockClient) Execute(_ context.Context, op *sempv2.Operation, _ map[string]any) (*sempv2.Result, error) {
	m.mu.Lock()
	m.calls = append(m.calls, op.ID)
	m.mu.Unlock()

	if err, ok := m.errors[op.ID]; ok {
		return nil, err
	}
	if resp, ok := m.responses[op.ID]; ok {
		return resp, nil
	}
	return &sempv2.Result{Data: map[string]any{"status": "ok"}, StatusCode: 200}, nil
}

func testTool() composite.CompositeTool {
	return composite.CompositeTool{
		Name:        "get-queue-metrics",
		Description: "Get detailed metrics for a specific queue.",
		Parameters: []composite.ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true, Description: "The VPN"},
			{Name: "queueName", Type: "string", Required: true, Description: "The queue"},
		},
		Steps: []composite.Step{
			{
				ID:        "queueMetrics",
				Operation: "monitor/getMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
					"queueName":  "{{.Params.queueName}}",
				},
			},
		},
		Result:      composite.ResultStrategy{Strategy: "collect"},
		Annotations: composite.ToolAnnotations{ReadOnly: boolPtr(true)},
	}
}

func testOperations() map[string]*sempv2.Operation {
	return map[string]*sempv2.Operation{
		"monitor/getMsgVpnQueue": {
			ID:     "getMsgVpnQueue",
			Method: "GET",
			Path:   "/SEMP/v2/monitor/msgVpns/{msgVpnName}/queues/{queueName}",
		},
	}
}

func TestCompositeToolHandler_Handle(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpnQueue"] = &sempv2.Result{
		Data:       map[string]any{"queueName": "q1", "msgCount": 42},
		StatusCode: 200,
	}

	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	tc := &ToolContext{SEMPv2Client: client}
	result, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "q1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
		return
	}
	if result.StructuredContent == nil {
		t.Fatal("expected non-nil StructuredContent")
		return
	}

	// Collect strategy: result keyed by step ID.
	stepResult, ok := result.StructuredContent["queueMetrics"]
	if !ok {
		t.Fatal("expected 'queueMetrics' key in result")
	}
	data, ok := stepResult.(map[string]any)
	if !ok {
		t.Fatalf("expected map for step result, got %T", stepResult)
	}
	if data["queueName"] != "q1" {
		t.Errorf("expected queueName 'q1', got %v", data["queueName"])
	}
}

func TestCompositeToolHandler_Schema(t *testing.T) {
	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	schema := handler.Metadata().InputSchema

	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if _, ok := props["msgVpnName"]; !ok {
		t.Error("missing msgVpnName in schema properties")
	}
	if _, ok := props["queueName"]; !ok {
		t.Error("missing queueName in schema properties")
	}
	// Broker param should NOT be in handler schema — manager injects it.
	if _, ok := props["broker"]; ok {
		t.Error("broker should not be in handler schema")
	}

	required, ok := schema["required"].([]string)
	if !ok {
		t.Fatal("expected required list")
	}
	requiredSet := make(map[string]bool)
	for _, r := range required {
		requiredSet[r] = true
	}
	if !requiredSet["msgVpnName"] || !requiredSet["queueName"] {
		t.Errorf("expected msgVpnName and queueName in required, got %v", required)
	}
}

func TestCompositeToolHandler_SchemaOptionalParams(t *testing.T) {
	tool := testTool()
	tool.Parameters = append(tool.Parameters, composite.ParameterDef{
		Name: "optional", Type: "string", Required: false,
	})

	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(tool, executor)

	schema := handler.Metadata().InputSchema
	required, _ := schema["required"].([]string)
	for _, r := range required {
		if r == "optional" {
			t.Error("optional param should not be in required list")
		}
	}
}

func TestCompositeToolHandler_OutputSchema(t *testing.T) {
	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	schema := handler.Metadata().OutputSchema

	if schema["type"] != "object" {
		t.Errorf("output schema type = %v, want object", schema["type"])
	}
	addProps, ok := schema["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatal("expected additionalProperties map")
	}
	if addProps["type"] != "object" {
		t.Errorf("additionalProperties type = %v, want object", addProps["type"])
	}
}

func TestCompositeToolHandler_Annotations(t *testing.T) {
	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	ann := handler.Metadata().Annotations
	if !ann.ReadOnly {
		t.Error("expected ReadOnly = true for monitoring tool")
	}
	if ann.Destructive != nil {
		t.Error("expected Destructive = nil (omitted in YAML) for monitoring tool")
	}
}

func TestCompositeToolHandler_NameAndDescription(t *testing.T) {
	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	meta := handler.Metadata()
	if meta.Name != "get-queue-metrics" {
		t.Errorf("Name = %q, want %q", meta.Name, "get-queue-metrics")
	}
	if meta.Description != "Get detailed metrics for a specific queue." {
		t.Errorf("Description = %q, want correct description", meta.Description)
	}
}

// TestCompositeToolHandler_Metadata_FreshPointersAcrossCalls verifies the
// Metadata() contract: every call returns freshly-allocated state, including
// the *bool fields on Annotations. Without this guarantee, a caller mutating
// *Metadata().Annotations.Destructive would silently corrupt the YAML-backed
// handler state and leak into every subsequent registration/call.
//
// This is a regression test for a real bug caught in PR review where
// toolAnnotations was passing the YAML-backed *bool pointers straight through
// instead of cloning them.
func TestCompositeToolHandler_Metadata_FreshPointersAcrossCalls(t *testing.T) {
	tool := testTool()
	destructive, openWorld := true, true
	tool.Annotations.Destructive = &destructive
	tool.Annotations.OpenWorld = &openWorld

	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(tool, executor)

	m1 := handler.Metadata()
	m2 := handler.Metadata()

	if m1.Annotations.Destructive == m2.Annotations.Destructive {
		t.Error("Metadata() returned the same Destructive *bool address across calls; must be a fresh allocation per call")
	}
	if m1.Annotations.OpenWorld == m2.Annotations.OpenWorld {
		t.Error("Metadata() returned the same OpenWorld *bool address across calls; must be a fresh allocation per call")
	}

	// Mutating a returned *bool must not affect later Metadata() calls or
	// the underlying handler state.
	*m1.Annotations.Destructive = false
	m3 := handler.Metadata()
	if m3.Annotations.Destructive == nil || *m3.Annotations.Destructive != true {
		t.Errorf("after mutating m1.Annotations.Destructive=false, m3.Annotations.Destructive = %v; expected fresh pointer to true",
			m3.Annotations.Destructive)
	}
}

// TestCompositeToolHandler_SchemaMinLength: only required string params carry
// minLength:1; optional strings and required non-strings do not.
func TestCompositeToolHandler_SchemaMinLength(t *testing.T) {
	tool := composite.CompositeTool{
		Name:        "create-queue",
		Description: "Create a queue.",
		Parameters: []composite.ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "queueName", Type: "string", Required: true},
			{Name: "filterText", Type: "string"},
			{Name: "maxResults", Type: "integer", Required: true},
		},
	}

	handler := NewCompositeToolHandler(tool, composite.NewCompositeExecutor(nil))
	props, ok := handler.Metadata().InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}

	for _, name := range []string{"msgVpnName", "queueName"} {
		if got := props[name].(map[string]any)["minLength"]; got != 1 {
			t.Errorf("%s minLength = %v, want 1", name, got)
		}
	}
	if _, ok := props["filterText"].(map[string]any)["minLength"]; ok {
		t.Error("optional string filterText should not have minLength")
	}
	if _, ok := props["maxResults"].(map[string]any)["minLength"]; ok {
		t.Error("required integer maxResults should not have minLength")
	}
}

// TestCloneBoolPtr exercises cloneBoolPtr directly: nil-passes-through, and
// non-nil produces a fresh allocation with the same value.
func TestCloneBoolPtr(t *testing.T) {
	if got := cloneBoolPtr(nil); got != nil {
		t.Errorf("cloneBoolPtr(nil) = %v, want nil", got)
	}

	src := true
	got := cloneBoolPtr(&src)
	if got == nil {
		t.Fatal("cloneBoolPtr(&true) returned nil")
	}
	if *got != true {
		t.Errorf("cloneBoolPtr(&true) value = %v, want true", *got)
	}
	if got == &src {
		t.Error("cloneBoolPtr returned the same pointer as input; must be a fresh allocation")
	}
}
