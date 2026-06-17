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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeTestConfig writes a minimal broker-config YAML to t.TempDir, loads it
// via LoadConfig (exercising the real validate+canonicalize path), and
// returns the resulting *ServerConfig.
func writeTestConfig(t *testing.T, brokers ...[2]string) *config.ServerConfig {
	t.Helper()
	var b []byte
	b = append(b, []byte("mcp_client_auth:\n  mode: disabled\nbrokers:\n")...)
	for _, kv := range brokers {
		b = append(b, []byte(fmt.Sprintf("  %s:\n    url: %s\n    auth:\n      mode: basic\n      username: admin\n      password: admin\n", kv[0], kv[1]))...)
	}
	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func newTestPool(t *testing.T) *semp.BrokerPool {
	t.Helper()
	cfg := writeTestConfig(t,
		[2]string{"dev", "http://localhost:8081"},
		[2]string{"prod", "http://localhost:8082"},
	)
	return semp.NewBrokerPool(cfg)
}

// stubHandler implements ToolHandler for unit testing the manager.
type stubHandler struct {
	name        string
	description string
	schema      map[string]any
	outputSch   map[string]any
	annotations Annotations
	handleFn    func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error)
}

func (h *stubHandler) Metadata() Metadata {
	return Metadata{
		Name:         h.name,
		Description:  h.description,
		InputSchema:  h.schema,
		OutputSchema: h.outputSch,
		Annotations:  h.annotations,
	}
}

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
		annotations: Annotations{ReadOnly: true},
	}
}

// --- Route tests ---

func TestRoute_ValidTool(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	handler, err := mgr.Route("test-tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name := handler.Metadata().Name; name != "test-tool" {
		t.Errorf("expected handler name 'test-tool', got %q", name)
	}
}

func TestRoute_UnknownTool(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	_, err := mgr.Route("nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error = %v, want 'unknown tool'", err)
	}
}

func TestRegister_DuplicatePanics(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
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
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"msgVpnName": "default",
	}, Identity{})
	if err == nil {
		t.Fatal("expected error for missing broker")
	}
	if !strings.Contains(err.Error(), "broker parameter is required") {
		t.Errorf("error = %v, want 'broker parameter is required'", err)
	}
}

func TestCallTool_UnknownBroker(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "nonexistent",
		"msgVpnName": "default",
	}, Identity{})
	if err == nil {
		t.Fatal("expected error for unknown broker")
	}
	if !strings.Contains(err.Error(), "unknown broker") {
		t.Errorf("error = %v, want 'unknown broker'", err)
	}
}

// TestCallTool_UnknownBroker_PreservesCallerCasing pins the design choice
// documented in plan §11.2: on lookup failure (ErrUnknownBroker), the error
// echoes the operator's raw alias input verbatim, not a lowercased form. This
// asymmetry — display form on success, raw form on failure — exists because
// the operator needs to see their own typo to fix it.
func TestCallTool_UnknownBroker_PreservesCallerCasing(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	const rawAlias = "PRODEAST-DOESNT-EXIST"
	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     rawAlias,
		"msgVpnName": "default",
	}, Identity{})
	if err == nil {
		t.Fatal("expected error for unknown broker")
	}
	if !strings.Contains(err.Error(), rawAlias) {
		t.Errorf("error should preserve operator's original casing %q verbatim, got: %v", rawAlias, err)
	}
}

// TestCallTool_ResolvesBothProtocolClients verifies that the manager
// populates both SEMPv1Client and SEMPv2Client on the ToolContext for every
// invocation, regardless of which protocol the handler ends up using.
func TestCallTool_ResolvesBothProtocolClients(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		if tc.SEMPv1Client == nil {
			t.Error("ToolContext.SEMPv1Client is nil — manager did not resolve v1 client")
		}
		if tc.SEMPv2Client == nil {
			t.Error("ToolContext.SEMPv2Client is nil — manager did not resolve v2 client")
		}
		return &ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
	}
	mgr.Register(handler)

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("CallTool() error: %v", err)
	}
}

// --- Parameter validation tests ---

func TestCallTool_ValidationError_MissingRequired(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	_, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker": "dev",
		// msgVpnName missing
	}, Identity{})
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
	mgr := NewToolManager(newTestPool(t))

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
	}, Identity{})
	if err == nil {
		t.Fatal("expected validation error for wrong type")
	}
	if !strings.Contains(err.Error(), "parameter validation failed") {
		t.Errorf("error = %v, want 'parameter validation failed'", err)
	}
}

// --- Structured output tests ---

func TestCallTool_StructuredOutput(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

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
	}, Identity{})
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
	mgr := NewToolManager(newTestPool(t))

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
	}, Identity{})
	if err != nil {
		t.Fatalf("expected output validation to pass, got: %v", err)
	}
}

func TestCallTool_OutputValidationFails(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

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
	}, Identity{})
	if err == nil {
		t.Fatal("expected output validation error")
	}
	if !strings.Contains(err.Error(), "output validation") {
		t.Errorf("error = %v, want 'output validation'", err)
	}
}

// --- Annotations tests ---

func TestCallTool_AnnotationsReadOnly(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	handler := newStubHandler("monitor-tool")
	mgr.Register(handler)

	h, _ := mgr.Route("monitor-tool")
	ann := h.Metadata().Annotations
	if !ann.ReadOnly {
		t.Error("expected ReadOnly = true for monitoring tool")
	}
}

// --- Destructive tool WARNING tests ---

func TestCallTool_DestructiveWarningLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("destructive-tool")
	destructive := true
	handler.annotations = Annotations{
		Destructive: &destructive,
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
	}, Identity{})
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
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &sempv2.SEMPError{
			Operation:   "getMsgVpnQueue",
			StatusCode:  404,
			Description: "Queue Not Found",
			SEMPCode:    6,
			SEMPStatus:  "NOT_FOUND",
			Body:        `{"meta":{"error":{"code":6,"status":"NOT_FOUND","description":"Queue Not Found"}}}`,
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	sc, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want map[string]any", result.StructuredContent)
	}
	if sc["retryable"] != false {
		t.Errorf("retryable = %v, want false", sc["retryable"])
	}
	if sc["status"] != 404 {
		t.Errorf("status = %v, want 404", sc["status"])
	}
	if sc["operation"] != "getMsgVpnQueue" {
		t.Errorf("operation = %v, want getMsgVpnQueue", sc["operation"])
	}
	if sc["sempStatus"] != "NOT_FOUND" {
		t.Errorf("sempStatus = %v, want NOT_FOUND", sc["sempStatus"])
	}
	if sc["error"] != "Queue Not Found" {
		t.Errorf("error = %v, want 'Queue Not Found'", sc["error"])
	}
}

// TestCallTool_SanitizesResponseButPreservesLogDetail verifies the sanitization
// boundary: the agent-facing result has internal detail (IP, filesystem path)
// redacted, while the full unsanitized error is preserved server-side as the
// "detail" field on the single "tool invoked" error log line for debugging.
func TestCallTool_SanitizesResponseButPreservesLogDetail(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	// The detail is logged on the error line at ERROR level.
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	mgr := NewToolManager(newTestPool(t))

	// A 4xx error whose description carries both an IP and a filesystem path —
	// the two things the sanitizer must strip from the agent-facing message.
	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &sempv2.SEMPError{
			Operation:   "getMsgVpnQueue",
			StatusCode:  400,
			Description: "connect to 10.0.0.5 failed reading /opt/solace/config.json",
			SEMPCode:    11,
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	// Agent-facing content must be redacted: no raw IP or path, with placeholders.
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "10.0.0.5") {
		t.Errorf("response leaked raw IP: %q", text)
	}
	if strings.Contains(text, "/opt/solace") {
		t.Errorf("response leaked raw path: %q", text)
	}
	if !strings.Contains(text, "[ip]") || !strings.Contains(text, "[path]") {
		t.Errorf("response = %q, want redaction placeholders [ip] and [path]", text)
	}

	// Server-side, the full unsanitized detail must survive as the "detail" field
	// on the "tool invoked" error log line for debugging.
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) != nil {
			continue
		}
		if fields["msg"] != "tool invoked" || fields["status"] != "error" {
			continue
		}
		found = true
		detail, _ := fields["detail"].(string)
		if !strings.Contains(detail, "10.0.0.5") {
			t.Errorf("log detail = %q, want it to preserve the raw IP", detail)
		}
		if !strings.Contains(detail, "/opt/solace/config.json") {
			t.Errorf("log detail = %q, want it to preserve the raw path", detail)
		}
	}
	if !found {
		t.Fatalf("did not find a \"tool invoked\" error log line; log:\n%s", buf.String())
	}
}

// TestLogToolResult_UnknownErrorLogsTypeNotMessage verifies the type gate on the
// "detail" field: for an error that is none of the audited broker types, we log
// only the Go type — never the raw message — since we can't vouch for its
// contents (the agent-facing path already hides it behind genericInternalMessage).
func TestLogToolResult_UnknownErrorLogsTypeNotMessage(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	mgr := NewToolManager(newTestPool(t))

	// An arbitrary, non-SEMP error whose text contains something we must not log.
	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, fmt.Errorf("dial tcp admin:hunter2@10.0.0.9:943: connection refused")
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	var logFields map[string]any
	if jsonErr := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &logFields); jsonErr != nil {
		t.Fatalf("failed to parse log output as JSON: %v\nlog: %s", jsonErr, buf.String())
	}

	detail, _ := logFields["detail"].(string)
	// Safety: the raw message — credentials, host, port — must not reach the log.
	if strings.Contains(detail, "hunter2") || strings.Contains(detail, "10.0.0.9") {
		t.Errorf("detail leaked raw unknown-error content: %q", detail)
	}
	// But we must still log a breadcrumb (the Go type) — not nothing.
	if detail == "" {
		t.Error("detail is empty; expected the Go type of the unknown error")
	}
}

// TestLogToolResult_V1ErrorEmitsStructuredFields verifies that when a handler
// returns a *sempv1.Error, the manager's logToolResult emits the v1-specific
// structured log fields (kind, http_status, reason_code) and does not emit the
// v2-specific "operation" field.
func TestLogToolResult_V1ErrorEmitsStructuredFields(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &sempv1.Error{
			Kind:       sempv1.ErrorKindExecuteFail,
			StatusCode: 200,
			Message:    "test failure",
			ReasonCode: 431,
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	// Execution errors now return (result, nil) per MCP spec.
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	logOutput := buf.String()

	// Parse the captured log line to verify structured fields are present
	// with the expected types and values.
	var logFields map[string]any
	if jsonErr := json.Unmarshal([]byte(logOutput), &logFields); jsonErr != nil {
		t.Fatalf("failed to parse log output as JSON: %v\nlog: %s", jsonErr, logOutput)
	}

	if got := logFields["kind"]; got != "execute-fail" {
		t.Errorf("expected kind=%q, got %v", "execute-fail", got)
	}
	if got := logFields["http_status"]; got != float64(200) {
		t.Errorf("expected http_status=200, got %v", got)
	}
	if got := logFields["reason_code"]; got != float64(431) {
		t.Errorf("expected reason_code=431, got %v", got)
	}
	if got := logFields["error_type"]; got != "execution_error" {
		t.Errorf("expected error_type=%q, got %v", "execution_error", got)
	}

	// v2-specific field must NOT appear — proves the switch took the v1 branch.
	if _, present := logFields["operation"]; present {
		t.Error("operation field should not be present for v1 errors (would indicate v2 branch fired)")
	}
}

func TestCallTool_SEMPv1Error_IsErrorResult(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &sempv1.Error{
			Kind:       sempv1.ErrorKindPermission,
			StatusCode: 200,
			Message:    "Insufficient user privileges",
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	sc := result.StructuredContent.(map[string]any)
	if sc["retryable"] != false {
		t.Errorf("retryable = %v, want false", sc["retryable"])
	}
	if sc["kind"] != "permission" {
		t.Errorf("kind = %v, want permission", sc["kind"])
	}

	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Insufficient user privileges") {
		t.Errorf("text = %q, want it to contain broker error message", text)
	}
}

func TestCallTool_RetriesExhausted_RetryableTrue(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &resilience.RetriesExhaustedError{
			StatusCode: 503,
			Attempts:   3,
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	sc := result.StructuredContent.(map[string]any)
	if sc["retryable"] != true {
		t.Errorf("retryable = %v, want true", sc["retryable"])
	}
	if sc["status"] != 503 {
		t.Errorf("status = %v, want 503", sc["status"])
	}
	if sc["attempts"] != 3 {
		t.Errorf("attempts = %v, want 3", sc["attempts"])
	}

	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Internal retries exhausted") {
		t.Errorf("text = %q, want it to mention retries exhausted", text)
	}
}

func TestCallTool_RetriesExhausted_NetworkError(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &resilience.RetriesExhaustedError{
			Attempts: 2,
			Err:      fmt.Errorf("dial tcp: connection refused"),
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	sc := result.StructuredContent.(map[string]any)
	if sc["retryable"] != true {
		t.Errorf("retryable = %v, want true", sc["retryable"])
	}
	if _, present := sc["status"]; present {
		t.Error("status should not be present for network errors (StatusCode=0)")
	}

	text := result.Content[0].(*mcp.TextContent).Text
	// The underlying network error (which can carry a host or IP) is never
	// echoed; the agent sees only the generic retries-exhausted message.
	if !strings.Contains(text, "Internal retries exhausted") {
		t.Errorf("text = %q, want it to mention retries exhausted", text)
	}
}

func TestCallTool_PlainError_IsErrorResult(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, fmt.Errorf("something unexpected happened")
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected IsError to be true")
	}

	sc := result.StructuredContent.(map[string]any)
	if sc["retryable"] != false {
		t.Errorf("retryable = %v, want false", sc["retryable"])
	}

	text := result.Content[0].(*mcp.TextContent).Text
	// A plain/unknown error must be replaced with the generic message — the
	// raw error string is never echoed to the agent.
	if !strings.Contains(text, genericInternalMessage) {
		t.Errorf("text = %q, want the generic internal message", text)
	}

	// No protocol-specific fields for plain errors.
	if _, present := sc["status"]; present {
		t.Error("status should not be present for plain errors")
	}
}

func TestCallTool_LimitError_IncludesGuidance(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("test-tool")
	handler.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		return nil, &sempv1.Error{
			Kind:       sempv1.ErrorKindLimit,
			StatusCode: 200,
			Message:    "Response too big",
		}
	}
	mgr.Register(handler)

	result, err := mgr.CallTool(context.Background(), "test-tool", map[string]any{
		"broker":     "dev",
		"msgVpnName": "default",
	}, Identity{})
	if err != nil {
		t.Fatalf("expected nil protocol error, got: %v", err)
	}

	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Reduce the scope of the request") {
		t.Errorf("text = %q, want it to contain limit-error guidance", text)
	}
}

func TestCallTool_BrokerStrippedFromHandlerParams(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

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
	}, Identity{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, hasBroker := receivedParams["broker"]; hasBroker {
		t.Error("broker param should be stripped before reaching handler")
	}
}

// --- Handlers list test ---

func TestHandlers_ReturnsAll(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("tool-a"))
	mgr.Register(newStubHandler("tool-b"))

	handlers := mgr.Handlers()
	if len(handlers) != 2 {
		t.Fatalf("expected 2 handlers, got %d", len(handlers))
	}

	names := make(map[string]bool)
	for _, h := range handlers {
		names[h.Metadata().Name] = true
	}
	if !names["tool-a"] || !names["tool-b"] {
		t.Errorf("expected tool-a and tool-b, got %v", names)
	}
}

func TestToolManager_ConcurrentRegisterAndRoute(t *testing.T) {
	const workers = 20
	mgr := NewToolManager(newTestPool(t))

	// Pre-register tools that readers will look up.
	for i := range workers {
		mgr.Register(newStubHandler(fmt.Sprintf("pre-tool-%d", i)))
	}

	var wg sync.WaitGroup

	// Writers: register new tools concurrently.
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mgr.Register(newStubHandler(fmt.Sprintf("new-tool-%d", i)))
		}(i)
	}

	// Readers: call Route, Handlers, and CallTool concurrently with writers.
	for i := range workers {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			_, _ = mgr.Route(fmt.Sprintf("pre-tool-%d", i))
		}(i)
		go func() {
			defer wg.Done()
			_ = mgr.Handlers()
		}()
		go func(i int) {
			defer wg.Done()
			// CallTool exercises Route internally; broker resolution will fail
			// (no real broker) but the important thing is no data race.
			_, _ = mgr.CallTool(context.Background(), fmt.Sprintf("pre-tool-%d", i),
				map[string]any{"broker": "no-such-broker"}, Identity{})
		}(i)
	}

	wg.Wait()
}

// --- SOL-149606 identity-in-audit-log tests --------------------------------

// idFixture builds an Identity equivalent to what the OAuth-mode shim would
// produce for a representative JWT — used by the per-line audit-log tests.
func idFixture() Identity {
	return NewIdentityFromTokenInfo(&sdkauth.TokenInfo{
		UserID: "auth0|abc123",
		Extra: map[string]any{
			"iss":       "https://example.auth0.com/",
			"client_id": "cursor-ide",
			"jti":       "jti-xyz",
		},
	})
}

// captureLog runs fn with a fresh JSON slog handler installed as the default
// logger and returns the captured (single-line) JSON object.
func captureLog(t *testing.T, level slog.Level, fn func()) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	fn()

	// Each test produces one log line we care about; if multiple lines were
	// emitted (e.g., a destructive WARN plus the trailing INFO), return the
	// LAST one — that's the tool-invocation result line. Callers that need
	// the WARN explicitly use captureAllLogs.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no log lines captured")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &parsed); err != nil {
		t.Fatalf("parse log: %v\n%s", err, buf.String())
	}
	return parsed
}

// captureAllLogs returns every emitted JSON log object in order.
func captureAllLogs(t *testing.T, level slog.Level, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	fn()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, parsed)
	}
	return out
}

func assertIdentityFields(t *testing.T, logged map[string]any, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, ok := logged[k]
		if !ok {
			t.Errorf("expected identity field %q in log line, got %v", k, logged)
			continue
		}
		if got != v {
			t.Errorf("log[%q] = %v, want %q", k, got, v)
		}
	}
}

func TestCallTool_logsIdentityFields_success(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	logged := captureLog(t, slog.LevelInfo, func() {
		_, err := mgr.CallTool(context.Background(), "test-tool",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture())
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	if logged["status"] != "success" {
		t.Errorf("status = %v, want success", logged["status"])
	}
	assertIdentityFields(t, logged, map[string]string{
		"sub":       "auth0|abc123",
		"iss":       "https://example.auth0.com/",
		"client_id": "cursor-ide",
		"jti":       "jti-xyz",
	})
}

func TestCallTool_logsIdentityFields_error(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	logged := captureLog(t, slog.LevelError, func() {
		// Missing broker → triggers the error-path logToolResult emit.
		_, _ = mgr.CallTool(context.Background(), "test-tool",
			map[string]any{"msgVpnName": "default"}, idFixture())
	})

	if logged["status"] != "error" {
		t.Errorf("status = %v, want error", logged["status"])
	}
	assertIdentityFields(t, logged, map[string]string{
		"sub":       "auth0|abc123",
		"iss":       "https://example.auth0.com/",
		"client_id": "cursor-ide",
		"jti":       "jti-xyz",
	})
}

func TestCallTool_logsIdentity_destructiveWarn(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))

	handler := newStubHandler("destructive-tool")
	destructive := true
	handler.annotations = Annotations{Destructive: &destructive}
	mgr.Register(handler)

	logs := captureAllLogs(t, slog.LevelWarn, func() {
		_, err := mgr.CallTool(context.Background(), "destructive-tool",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, idFixture())
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	var warn map[string]any
	for _, l := range logs {
		if l["msg"] == "executing destructive operation" {
			warn = l
			break
		}
	}
	if warn == nil {
		t.Fatalf("destructive WARN not captured; logs: %v", logs)
	}
	assertIdentityFields(t, warn, map[string]string{
		"sub":       "auth0|abc123",
		"iss":       "https://example.auth0.com/",
		"client_id": "cursor-ide",
		"jti":       "jti-xyz",
	})
}

// TestCallTool_disabledMode_emitsNoIdentityFields pins the empty-group
// behavior: Identity{present:false} produces a log line with no identity,
// sub, iss, client_id, or jti keys. If slog ever changes how it emits an
// empty GroupValue this test will catch it.
func TestCallTool_disabledMode_emitsNoIdentityFields(t *testing.T) {
	mgr := NewToolManager(newTestPool(t))
	mgr.Register(newStubHandler("test-tool"))

	logged := captureLog(t, slog.LevelInfo, func() {
		_, err := mgr.CallTool(context.Background(), "test-tool",
			map[string]any{"broker": "dev", "msgVpnName": "default"}, Identity{})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	})

	for _, k := range []string{"identity", "sub", "iss", "client_id", "jti"} {
		if _, present := logged[k]; present {
			t.Errorf("disabled-mode log line must not carry %q, got %v", k, logged[k])
		}
	}
	// Sanity: existing fields are still emitted.
	if logged["status"] != "success" {
		t.Errorf("status = %v, want success", logged["status"])
	}
	if logged["tool"] != "test-tool" {
		t.Errorf("tool = %v, want test-tool", logged["tool"])
	}
}
