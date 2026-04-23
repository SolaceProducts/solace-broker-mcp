package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
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

	tc := &ToolContext{SEMPClient: client}
	result, err := handler.Handle(context.Background(), tc, map[string]any{
		"msgVpnName": "default",
		"queueName":  "q1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.StructuredContent == nil {
		t.Fatal("expected non-nil StructuredContent")
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

	schema := handler.Schema()

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

	schema := handler.Schema()
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

	schema := handler.OutputSchema()

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

	ann := handler.Annotations()
	if ann == nil {
		t.Fatal("expected non-nil annotations")
	}
	if !ann.ReadOnlyHint {
		t.Error("expected ReadOnlyHint = true for monitoring tool")
	}
	if ann.DestructiveHint != nil {
		t.Error("expected DestructiveHint = nil (omitted in YAML) for monitoring tool")
	}
}

func TestCompositeToolHandler_NameAndDescription(t *testing.T) {
	executor := composite.NewCompositeExecutor(testOperations())
	handler := NewCompositeToolHandler(testTool(), executor)

	if handler.Name() != "get-queue-metrics" {
		t.Errorf("Name() = %q, want %q", handler.Name(), "get-queue-metrics")
	}
	if handler.Description() != "Get detailed metrics for a specific queue." {
		t.Errorf("Description() = %q, want correct description", handler.Description())
	}
}
