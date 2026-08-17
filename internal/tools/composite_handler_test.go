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
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
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

// writeTestTool mirrors create-queue's real shape: one config-op step, no
// select:, msgVpnName folded out of the body as a path param.
func writeTestTool() composite.CompositeTool {
	return composite.CompositeTool{
		Name:        "create-queue",
		Description: "Create a queue in a Message VPN",
		Parameters: []composite.ParameterDef{
			{Name: "msgVpnName", Type: "string", Required: true},
			{Name: "queueName", Type: "string", Required: true},
		},
		Steps: []composite.Step{
			{
				ID:        "createQueue",
				Operation: "config/createMsgVpnQueue",
				Args: map[string]string{
					"msgVpnName": "{{.Params.msgVpnName}}",
				},
			},
		},
		Result: composite.ResultStrategy{Strategy: "collect"},
	}
}

// deleteTestTool mirrors delete-queue's real shape: an action/config op with
// no response data (SempMetaOnlyResponse), same as every real delete tool.
func deleteTestTool() composite.CompositeTool {
	return composite.CompositeTool{
		Name:        "delete-queue",
		Description: "Delete a queue",
		Steps: []composite.Step{
			{ID: "deleteQueue", Operation: "config/deleteMsgVpnQueue"},
		},
		Result: composite.ResultStrategy{Strategy: "collect"},
	}
}

func TestCallsConfigOrActionOperation(t *testing.T) {
	if callsConfigOrActionOperation(testTool()) {
		t.Error("a monitor-only tool should not be detected as calling config/action")
	}
	if !callsConfigOrActionOperation(writeTestTool()) {
		t.Error("a tool with a config/ step should be detected as calling config/action")
	}
	if !callsConfigOrActionOperation(deleteTestTool()) {
		t.Error("a tool with a config/ delete step should be detected as calling config/action")
	}
}

// TestCompositeToolHandler_OutputSchema_MonitorToolUnchanged guards against
// this SOL-152947 wiring accidentally widening beyond write tools — a
// monitor tool must keep the exact generic envelope it always had.
func TestCompositeToolHandler_OutputSchema_MonitorToolUnchanged(t *testing.T) {
	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	got := handler.Metadata().OutputSchema
	want := StepKeyedEnvelopeSchema()
	if got["type"] != want["type"] {
		t.Errorf("type = %v, want %v", got["type"], want["type"])
	}
	addProps, ok := got["additionalProperties"].(map[string]any)
	if !ok || addProps["type"] != "object" {
		t.Errorf("expected the unchanged generic additionalProperties shape, got %v", got["additionalProperties"])
	}
	if _, hasRequired := got["required"]; hasRequired {
		t.Error("the generic monitor-tool envelope has never had a required list; it must not gain one")
	}
}

func TestCompositeToolHandler_OutputSchema_WriteToolIsStrict(t *testing.T) {
	operations := map[string]*sempv2.Operation{
		"config/createMsgVpnQueue": {
			ID:             "createMsgVpnQueue",
			ResponseFields: map[string]string{"msgVpnName": "string", "queueName": "string", "accessType": "string"},
		},
	}
	executor := composite.NewCompositeExecutor(operations)
	handler := NewCompositeToolHandler(writeTestTool(), executor)

	schema := handler.Metadata().OutputSchema
	if schema["additionalProperties"] != false {
		t.Fatalf("top-level additionalProperties = %v, want false for a write tool", schema["additionalProperties"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected top-level properties map")
	}
	step, ok := props["createQueue"].(map[string]any)
	if !ok {
		t.Fatal("expected createQueue step schema")
	}
	if step["additionalProperties"] != false {
		t.Errorf("step additionalProperties = %v, want false", step["additionalProperties"])
	}
	// The step's runtime value is the whole SEMP envelope ({"data": ...,
	// "meta": ...}), not the item alone — the identifier fields live one
	// level down, under properties.data.
	stepProps, ok := step["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected the step schema to have a properties map (the envelope wrapper)")
	}
	dataSchema, ok := stepProps["data"].(map[string]any)
	if !ok {
		t.Fatal("expected a 'data' property on the step schema (the envelope's payload)")
	}
	required, ok := dataSchema["required"].([]string)
	if !ok {
		t.Fatal("expected the data schema to require its identifier fields")
	}
	wantRequired := map[string]bool{"msgVpnName": true, "queueName": true}
	if len(required) != len(wantRequired) {
		t.Errorf("required = %v, want %v", required, wantRequired)
	}
	for _, r := range required {
		if !wantRequired[r] {
			t.Errorf("unexpected required field %q", r)
		}
	}
}

// TestCompositeToolHandler_OutputSchema_DeleteToolStaysUsable confirms a
// write tool whose operation has no ResponseFields (every real delete/action
// tool) still gets a valid, non-crashing schema — permissive at the step
// level (nothing to constrain) but still additionalProperties: false at the
// top level, since the set of step IDs itself is still known.
func TestCompositeToolHandler_OutputSchema_DeleteToolStaysUsable(t *testing.T) {
	operations := map[string]*sempv2.Operation{
		"config/deleteMsgVpnQueue": {ID: "deleteMsgVpnQueue", ResponseFields: nil},
	}
	executor := composite.NewCompositeExecutor(operations)
	handler := NewCompositeToolHandler(deleteTestTool(), executor)

	schema := handler.Metadata().OutputSchema
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected top-level properties map")
	}
	step, ok := props["deleteQueue"].(map[string]any)
	if !ok {
		t.Fatal("expected deleteQueue step schema")
	}
	if _, hasAddProps := step["additionalProperties"]; hasAddProps {
		t.Errorf("a nil-ResponseFields step must stay fully permissive, got additionalProperties=%v", step["additionalProperties"])
	}
}

// TestCompositeToolHandler_OutputSchema_RealCatalogWiring is the end-to-end
// proof for SOL-152947: load the real embedded tools.yaml and specs, build
// handlers the way NewToolManagerFromComposite does, and confirm write tools
// get the strict schema while a monitor tool alongside them keeps the
// generic envelope — proving the wiring is genuinely connected, not just
// correct against synthetic fixtures.
func TestCompositeToolHandler_OutputSchema_RealCatalogWiring(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	executor := composite.NewCompositeExecutor(operations)

	find := func(name string) composite.CompositeTool {
		t.Helper()
		for _, tool := range realTools {
			if tool.Name == name {
				return tool
			}
		}
		t.Fatalf("tool %q not found in the real catalog", name)
		return composite.CompositeTool{}
	}

	writeSchema := NewCompositeToolHandler(find("create-queue"), executor).Metadata().OutputSchema
	if writeSchema["additionalProperties"] != false {
		t.Errorf("create-queue (real catalog): additionalProperties = %v, want false", writeSchema["additionalProperties"])
	}

	monitorSchema := NewCompositeToolHandler(find("list-queues"), executor).Metadata().OutputSchema
	addProps, ok := monitorSchema["additionalProperties"].(map[string]any)
	if !ok || addProps["type"] != "object" {
		t.Errorf("list-queues (real catalog): expected the unchanged generic envelope, got additionalProperties=%v", monitorSchema["additionalProperties"])
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
	prop := func(name string) map[string]any {
		p, ok := props[name].(map[string]any)
		if !ok {
			t.Fatalf("property %q missing or not an object", name)
		}
		return p
	}

	for _, name := range []string{"msgVpnName", "queueName"} {
		if got := prop(name)["minLength"]; got != 1 {
			t.Errorf("%s minLength = %v, want 1", name, got)
		}
	}
	if _, ok := prop("filterText")["minLength"]; ok {
		t.Error("optional string filterText should not have minLength")
	}
	if _, ok := prop("maxResults")["minLength"]; ok {
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
