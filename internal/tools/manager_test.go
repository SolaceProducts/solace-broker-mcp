package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newTestPool() *semp.BrokerPool {
	cfg := &config.ServerConfig{
		Brokers: map[string]*config.BrokerConfig{
			"dev": {
				URL:  "http://localhost:8081",
				Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "admin"},
			},
			"prod": {
				URL:  "http://localhost:8082",
				Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "admin"},
			},
		},
		SEMP: config.SEMPConfig{RequestTimeoutDuration: 5 * time.Second},
	}
	return semp.NewBrokerPool(cfg)
}

// stubHandler implements ToolHandler for unit testing the manager.
type stubHandler struct {
	name        string
	description string
	schema      map[string]any
	outputSch   map[string]any
	annotations *mcp.ToolAnnotations
	handleFn    func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error)
}

func (h *stubHandler) Name() string                     { return h.name }
func (h *stubHandler) Description() string              { return h.description }
func (h *stubHandler) Schema() map[string]any           { return h.schema }
func (h *stubHandler) OutputSchema() map[string]any     { return h.outputSch }
func (h *stubHandler) Annotations() *mcp.ToolAnnotations { return h.annotations }
func (h *stubHandler) Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
	if h.handleFn != nil {
		return h.handleFn(ctx, tc, params)
	}
	return &ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
}

func newStubHandler(name string) *stubHandler {
	return &stubHandler{
		name:        name,
		description: "A test tool",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msgVpnName": map[string]any{"type": "string"},
			},
			"required": []string{"msgVpnName"},
		},
		outputSch: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
		annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}
}

// --- Route tests ---

func TestRoute_ValidTool(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	mgr.Register(newStubHandler("test-tool"))

	handler, err := mgr.Route("test-tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handler.Name() != "test-tool" {
		t.Errorf("expected handler name 'test-tool', got %q", handler.Name())
	}
}

func TestRoute_UnknownTool(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	_, err := mgr.Route("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error = %v, want 'unknown tool'", err)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	mgr.Register(newStubHandler("dup"))

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for duplicate registration")
		}
	}()
	mgr.Register(newStubHandler("dup"))
}

// --- Broker resolution tests ---

func TestCallTool_MissingBroker(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	mgr.Register(newStubHandler("test-tool"))

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"msgVpnName": "default",
	})
	if err == nil {
		t.Fatal("expected error for missing broker")
	}
	if !strings.Contains(err.Error(), "broker parameter is required") {
		t.Errorf("error = %v, want 'broker parameter is required'", err)
	}
}

func TestCallTool_UnknownBroker(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	mgr.Register(newStubHandler("test-tool"))

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "nonexistent",
		"msgVpnName": "default",
	})
	if err == nil {
		t.Fatal("expected error for unknown broker")
	}
	if !strings.Contains(err.Error(), "unknown broker") {
		t.Errorf("error = %v, want 'unknown broker'", err)
	}
}

// --- Parameter validation tests ---

func TestCallTool_ValidationError_MissingRequired(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	mgr.Register(newStubHandler("test-tool"))

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker": "dev",
		// msgVpnName missing
	})
	if err == nil {
		t.Fatal("expected validation error for missing required field")
	}
	if !strings.Contains(err.Error(), "parameter validation failed") {
		t.Errorf("error = %v, want 'parameter validation failed'", err)
	}
	if !strings.Contains(err.Error(), "msgVpnName") {
		t.Errorf("error should mention 'msgVpnName', got: %v", err)
	}
}

func TestCallTool_ValidationError_WrongType(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	handler := newStubHandler("typed-tool")
	handler.schema = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "typed-tool", map[string]any{
		"broker": "dev",
		"count":  "not-a-number",
	})
	if err == nil {
		t.Fatal("expected validation error for wrong type")
	}
	if !strings.Contains(err.Error(), "parameter validation failed") {
		t.Errorf("error = %v, want 'parameter validation failed'", err)
	}
}

// --- Structured output tests ---

func TestCallTool_StructuredOutput(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return &ToolResult{
			StructuredContent: map[string]any{
				"step1": map[string]any{"queueName": "q1", "msgCount": 42},
			},
		}, nil
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify StructuredContent is set.
	if result.StructuredContent == nil {
		t.Fatal("expected StructuredContent to be set")
	}

	// Verify TextContent fallback is present.
	if len(result.Content) == 0 {
		t.Fatal("expected Content (TextContent fallback) to be set")
	}
	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}

	// Verify the JSON in TextContent matches StructuredContent.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(textContent.Text), &parsed); err != nil {
		t.Fatalf("TextContent is not valid JSON: %v", err)
	}
	step1, ok := parsed["step1"].(map[string]any)
	if !ok {
		t.Fatal("expected step1 in parsed TextContent")
	}
	if step1["queueName"] != "q1" {
		t.Errorf("expected queueName 'q1', got %v", step1["queueName"])
	}
}

// --- Output schema validation tests ---

func TestCallTool_OutputValidationPasses(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return &ToolResult{
			StructuredContent: map[string]any{
				"step1": map[string]any{"data": "value"},
			},
		}, nil
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("expected output validation to pass, got: %v", err)
	}
}

func TestCallTool_OutputValidationFails(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return &ToolResult{
			StructuredContent: map[string]any{
				"step1": "not-an-object", // violates envelope schema
			},
		}, nil
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	})
	if err == nil {
		t.Fatal("expected output validation error")
	}
	if !strings.Contains(err.Error(), "output validation") {
		t.Errorf("error = %v, want 'output validation'", err)
	}
}

// --- Annotations tests ---

func TestCallTool_AnnotationsReadOnly(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	handler := newStubHandler("monitor-tool")
	mgr.Register(handler)

	h, _ := mgr.Route("monitor-tool")
	ann := h.Annotations()
	if !ann.ReadOnlyHint {
		t.Error("expected ReadOnlyHint = true for monitoring tool")
	}
}

// --- Destructive tool WARNING tests ---

func TestCallTool_DestructiveWarningLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	mgr := NewToolManager(newTestPool())

	handler := newStubHandler("destructive-tool")
	destructive := true
	handler.annotations = &mcp.ToolAnnotations{
		DestructiveHint: &destructive,
	}
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return &ToolResult{
			StructuredContent: map[string]any{
				"step1": map[string]any{"done": true},
			},
		}, nil
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "destructive-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "executing destructive operation") {
		t.Errorf("expected destructive warning in logs, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "destructive-tool") {
		t.Errorf("expected tool name in warning log, got: %s", logOutput)
	}
}

// --- Error handling tests ---

func TestCallTool_SEMPErrorWrapped(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &sempv2.SEMPError{
			Operation:  "getMsgVpnQueue",
			StatusCode: 404,
			Body:       `{"error": "not found"}`,
		}
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	})
	if err == nil {
		t.Fatal("expected error from SEMP failure")
	}
	if !strings.Contains(err.Error(), "executing tool") {
		t.Errorf("error = %v, want wrapped with 'executing tool'", err)
	}
}

func TestCallTool_BrokerStrippedFromHandlerParams(t *testing.T) {
	mgr := NewToolManager(newTestPool())

	var receivedParams map[string]any
	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		receivedParams = params
		return &ToolResult{
			StructuredContent: map[string]any{
				"step1": map[string]any{"ok": true},
			},
		}, nil
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, hasBroker := receivedParams["broker"]; hasBroker {
		t.Error("broker param should be stripped before reaching handler")
	}
}

// --- Handlers list test ---

func TestHandlers_ReturnsAll(t *testing.T) {
	mgr := NewToolManager(newTestPool())
	mgr.Register(newStubHandler("tool-a"))
	mgr.Register(newStubHandler("tool-b"))

	handlers := mgr.Handlers()
	if len(handlers) != 2 {
		t.Fatalf("expected 2 handlers, got %d", len(handlers))
	}

	names := make(map[string]bool)
	for _, h := range handlers {
		names[h.Name()] = true
	}
	if !names["tool-a"] || !names["tool-b"] {
		t.Errorf("expected tool-a and tool-b, got %v", names)
	}
}
