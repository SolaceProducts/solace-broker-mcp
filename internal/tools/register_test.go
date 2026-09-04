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
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/panics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newRegTestPool(t *testing.T) *semp.BrokerPool {
	t.Helper()
	cfg := writeTestConfig(t,
		[2]string{"dev", "http://localhost:8081"},
		[2]string{"prod", "http://localhost:8082"},
	)
	return semp.NewBrokerPool(cfg, nil)
}

func TestInjectBrokerParam(t *testing.T) {
	pool := newRegTestPool(t)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"msgVpnName": map[string]any{"type": "string"},
		},
		"required": []string{"msgVpnName"},
	}

	result := injectBrokerParam(schema, pool)

	props, ok := result["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map")
	}
	if _, ok := props["broker"]; !ok {
		t.Fatal("broker property not injected")
	}
	if _, ok := props["msgVpnName"]; !ok {
		t.Fatal("original property lost after injection")
	}

	required, ok := result["required"].([]string)
	if !ok {
		t.Fatal("expected required list")
	}
	if required[0] != "broker" {
		t.Errorf("expected broker first in required, got %v", required)
	}
	if len(required) != 2 {
		t.Errorf("expected 2 required fields, got %d", len(required))
	}
}

func TestInjectBrokerParam_BrokerDescriptionListsAliases(t *testing.T) {
	pool := newRegTestPool(t)
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}

	result := injectBrokerParam(schema, pool)
	props := result["properties"].(map[string]any)
	brokerProp := props["broker"].(map[string]any)
	desc := brokerProp["description"].(string)

	for _, alias := range []string{"dev", "prod"} {
		if !containsStr(desc, alias) {
			t.Errorf("broker description %q missing alias %q", desc, alias)
		}
	}
}

func TestInjectBrokerParam_NoRequired(t *testing.T) {
	pool := newRegTestPool(t)
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}

	result := injectBrokerParam(schema, pool)
	required := result["required"].([]string)
	if len(required) != 1 || required[0] != "broker" {
		t.Errorf("expected [broker], got %v", required)
	}
}

func TestRegisterWithServer(t *testing.T) {
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	// No panic = tools registered successfully. The MCP SDK doesn't expose
	// a way to list registered tools directly, so we verify by checking
	// that AddTool was called without error.
}

// TestIsWriteTool pins the gate predicate: a tool is gated behind
// enable_write_tools iff it is not read-only. This deliberately covers BOTH
// destructive and non-destructive write tools — clear-queue-stats mutates
// broker state even though it is non-destructive, so it must be gated too.
func TestIsWriteTool(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		name string
		a    Annotations
		want bool
	}{
		{"read-only monitoring tool", Annotations{ReadOnly: true}, false},
		{"non-destructive write (e.g. clear-stats)", Annotations{ReadOnly: false, Destructive: &fa}, true},
		{"destructive write (e.g. delete-messages)", Annotations{ReadOnly: false, Destructive: &tr}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWriteTool(tc.a); got != tc.want {
				t.Errorf("isWriteTool(%+v) = %v, want %v", tc.a, got, tc.want)
			}
		})
	}
}

// TestRegisterWithServer_WriteGated is a smoke test that exercises both
// branches of the enable_write_tools gate. It registers a read-only stub, a
// non-destructive write stub, and a destructive write stub, then calls
// RegisterWithServer with each flag value. No panics + clean completion is the
// unit-test bar; the stronger "tool didn't appear in tools/list" check lives in
// the integration tests that invoke the SDK's tools/list endpoint.
func TestRegisterWithServer_WriteGated(t *testing.T) {
	tr, fa := true, false
	nonDestructiveWrite := newStubHandler("clear-stats-tool")
	nonDestructiveWrite.annotations = Annotations{ReadOnly: false, Destructive: &fa}
	destructiveWrite := newStubHandler("delete-tool")
	destructiveWrite.annotations = Annotations{ReadOnly: false, Destructive: &tr}

	pool := newRegTestPool(t)

	for _, enableWriteTools := range []bool{false, true} {
		t.Run(fmt.Sprintf("enableWriteTools=%v", enableWriteTools), func(t *testing.T) {
			mgr := NewToolManager(pool)
			mgr.Register(newStubHandler("read-tool"))
			mgr.Register(nonDestructiveWrite)
			mgr.Register(destructiveWrite)

			server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
			RegisterWithServer(mgr, server, pool, enableWriteTools, nil, "")
			// No panic; gate behavior verified at integration layer via tools/list.
		})
	}
}

func TestRegisterListBrokers(t *testing.T) {
	pool := newRegTestPool(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)

	RegisterListBrokers(server, pool, nil)
	// No panic = registered successfully.
}

// auditLines decodes every JSON log line in buf with msg "tool invoked" for
// the given tool name. Used to assert on the audit surface that feeds
// dashboards and SLOs.
func auditLines(t *testing.T, buf *bytes.Buffer, tool string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if entry["msg"] == "tool invoked" && entry["tool"] == tool {
			lines = append(lines, entry)
		}
	}
	return lines
}

// TestPanicAuditedAsError covers the audit half of SOL-150685: CallTool's
// deferred audit log runs during panic unwinding, before withRecovery's
// recover fires. toolErr is still nil at that point, so without panic
// detection the success branch emits "outcome": "success" at INFO for an
// invocation that panicked — misclassifying panics on the surface that feeds
// dashboards and SLOs. The audit line must instead report outcome=error with
// error_type=panic, and no success line may appear.
func TestPanicAuditedAsError(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)

	panicking := newStubHandler("panic-tool")
	panicking.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		panic("simulated handler bug")
	}
	mgr.Register(panicking)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "panic-tool", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// The result the agent actually receives. A recovered tool panic is an
	// APPLICATION error, not a protocol error: the call returns HTTP 200 with
	// IsError set, per the MCP spec's convention (architecture-reviewed
	// 2026-09-04, SOL-154037). Asserting it here is what keeps that contract
	// from drifting; the test previously discarded the result entirely.
	assertPanicResultShape(t, res)

	// event="panic_recovered" is the attribute every other recovery site in the
	// codebase emits (recovery/middleware.go, safego, tokenexchange,
	// tracing/selfstats). An alert keyed on it must fire for tool panics too.
	panicLog := findLogLine(t, &logBuf, "tool handler panicked")
	if got := panicLog["event"]; got != "panic_recovered" {
		t.Errorf(`panic log event = %v, want "panic_recovered"`, got)
	}
	if got := panicLog["tool"]; got != "panic-tool" {
		t.Errorf("panic log tool = %v, want %q", got, "panic-tool")
	}

	audits := auditLines(t, &logBuf, "panic-tool")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for panic-tool, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "error" {
		t.Errorf("audit outcome = %v, want %q (a panic must never audit as success)", got, "error")
	}
	if got := audits[0]["error_type"]; got != "panic" {
		t.Errorf("audit error_type = %v, want %q", got, "panic")
	}
	if got := audits[0]["level"]; got != "ERROR" {
		t.Errorf("audit level = %v, want ERROR", got)
	}
	// The raw panic value is unaudited text and must not reach the audit line.
	if containsStr(logBuf.String(), "simulated handler bug") {
		t.Errorf("raw panic value leaked into logs: %s", logBuf.String())
	}
}

// assertPanicResultShape pins what an agent receives when a tool handler panics:
// a sanitized application error, never the panic's own text. This ticket is
// telemetry-only (SOL-154037), so these assertions exist to prove the response
// did NOT change while the counter and the event tag were added.
func assertPanicResultShape(t *testing.T, res *mcp.CallToolResult) {
	t.Helper()
	if res == nil {
		t.Fatal("CallTool result = nil, want a CallToolResult with IsError set")
	}
	if !res.IsError {
		t.Errorf("result.IsError = false, want true (a recovered panic is an application error)")
	}
	if len(res.Content) != 1 {
		t.Fatalf("result.Content = %#v, want exactly one text block", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result.Content[0] = %#v, want *mcp.TextContent", res.Content[0])
	}
	if text.Text != serverInternalErrorMessage {
		t.Errorf("result content text = %q, want the generic server-internal message %q", text.Text, serverInternalErrorMessage)
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("result.StructuredContent = %#v, want a map", res.StructuredContent)
	}
	if got := structured["retryable"]; got != false {
		t.Errorf(`result.StructuredContent["retryable"] = %v, want false (a handler bug does not fix itself on retry)`, got)
	}
	if got := structured["error"]; got != serverInternalErrorMessage {
		t.Errorf(`result.StructuredContent["error"] = %v, want the generic server-internal message`, got)
	}
}

// findLogLine returns the single JSON log record in buf whose msg equals want.
func findLogLine(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if entry["msg"] == want {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 %q log line, got %d: %s", want, len(found), buf.String())
	}
	return found[0]
}

// panicCountsByBoundary collects mcp.panic.recovered from reader and returns its
// value per boundary label. An absent metric yields an empty map.
func panicCountsByBoundary(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mcp.panic.recovered" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("mcp.panic.recovered data = %#v, want a Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value("boundary")
				got[v.AsString()] += dp.Value
			}
		}
	}
	return got
}

// TestPanicIncrementsPanicCounter is the metrics half of SOL-154037 on the tool
// boundary: a panicking handler must increment
// mcp_panic_recovered_total{boundary="tool"} on the same counter the HTTP
// boundary uses, so one alert covers both recovery nets. A tool that returns
// normally must leave the series untouched, which is what makes a flat-zero
// graph mean "nothing panicked" rather than "nothing ran".
func TestPanicIncrementsPanicCounter(t *testing.T) {
	// Not parallel: the panic counter is process-level state (see the package
	// doc on internal/observability/panics). Registering rebinds the global, and
	// the provider is deliberately left running — this package cannot unregister
	// the global, so shutting it down would point the rest of the run at a dead
	// provider. A ManualReader holds no goroutines, so nothing leaks.
	reader := sdkmetric.NewManualReader()
	if err := panics.Register(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))); err != nil {
		t.Fatalf("panics.Register() error = %v", err)
	}

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)

	panicking := newStubHandler("panic-tool")
	panicking.handleFn = func(_ context.Context, _ *ToolContext, _ map[string]any) (*ToolResult, error) {
		panic("simulated handler bug")
	}
	mgr.Register(panicking)
	mgr.Register(newStubHandler("ok-tool"))

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ok-tool", Arguments: args}); err != nil {
		t.Fatalf("CallTool(ok-tool): %v", err)
	}
	if got := panicCountsByBoundary(t, reader); len(got) != 0 {
		t.Fatalf("counts = %v after a successful call, want none", got)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "panic-tool", Arguments: args}); err != nil {
		t.Fatalf("CallTool(panic-tool): %v", err)
	}

	got := panicCountsByBoundary(t, reader)
	if got["tool"] != 1 {
		t.Errorf(`mcp_panic_recovered_total{boundary="tool"} = %d, want 1 (counts = %v)`, got["tool"], got)
	}
	if got["http"] != 0 {
		t.Errorf(`a tool-boundary panic incremented boundary="http" (%d); the two boundaries must stay distinct`, got["http"])
	}
}

// TestListBrokersEmitsAuditLog covers the audit gap on the standalone
// list-brokers tool: its handler does not flow through CallTool, so before
// the fix a successful invocation produced zero audit output. Every tool
// invocation must emit exactly one "tool invoked" audit line.
func TestListBrokersEmitsAuditLog(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	pool := newRegTestPool(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterListBrokers(server, pool, nil)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list-brokers", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("list-brokers returned IsError: %v", res.Content)
	}

	audits := auditLines(t, &logBuf, "list-brokers")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for list-brokers, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "success" {
		t.Errorf("audit outcome = %v, want %q", got, "success")
	}
	// list-brokers resolves no broker; the audit line carries the "none"
	// sentinel, matching the metric label rather than omitting the field.
	if got := audits[0]["broker"]; got != "none" {
		t.Errorf("audit broker = %v, want %q", got, "none")
	}
}

// callToolTestHarness spins up a real MCP server+client session over an
// in-memory transport with a single stub tool registered, mirroring
// TestPanicAuditedAsError / TestListBrokersEmitsAuditLog. The SOL-153765
// regression only reproduces at the wire level — req.Params.Arguments'
// omitempty behavior and the SDK's any-typed CallToolParams.Arguments both
// need a real client-to-server round trip, not a direct call into the
// unexported closure. clientMiddleware, if given, is installed on the client
// via AddSendingMiddleware before Connect.
func callToolTestHarness(t *testing.T, clientMiddleware ...mcp.Middleware) *mcp.ClientSession {
	t.Helper()
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	if len(clientMiddleware) > 0 {
		client.AddSendingMiddleware(clientMiddleware...)
	}
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// forceOmitArguments is client-sending middleware that clears
// CallToolParams.Arguments back to nil immediately before the request is
// marshaled and sent. It exists because ClientSession.CallTool itself
// defaults a nil Arguments to map[string]any{} ("avoid sending nil over the
// wire" — see the SDK's client.go) before any middleware runs, which means a
// plain call through the typed client can never actually omit the
// "arguments" field. Undoing that default here, after the SDK's own
// defaulting but before the wire marshal, is the only way to reproduce the
// exact condition in the ticket: a tools/call request with no "arguments"
// field on the wire at all.
func forceOmitArguments(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tools/call" {
			if p, ok := req.GetParams().(*mcp.CallToolParams); ok {
				p.Arguments = nil
			}
		}
		return next(ctx, method, req)
	}
}

// TestCallToolHandler_OmittedArguments_AuditedAndValidated covers SOL-153765:
// a tools/call request with no "arguments" field at all is legal MCP
// (CallToolParams.Arguments carries omitempty), and must be treated the same
// as an empty object rather than failing json.Unmarshal on a nil RawMessage.
// Pre-fix, this closure returned a bare protocol error before ever calling
// ToolManager.CallTool, so zero audit lines were emitted. Post-fix, the
// request flows into normal validation (missing_broker, since no broker
// parameter is present either) and is fully audited.
func TestCallToolHandler_OmittedArguments_AuditedAndValidated(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	session := callToolTestHarness(t, forceOmitArguments)
	ctx := context.Background()

	// forceOmitArguments strips Arguments back to nil right before send, so
	// the wire request carries no "arguments" key at all — reproducing the
	// exact condition in the ticket
	// ({"method":"tools/call","params":{"name":"test-tool"}}).
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "test-tool"})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error instead of a tool result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected an error result (missing required broker), got success: %v", res.StructuredContent)
	}

	audits := auditLines(t, &logBuf, "test-tool")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for test-tool, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "error" {
		t.Errorf("audit outcome = %v, want %q", got, "error")
	}
	if got := audits[0]["error_type"]; got != "missing_broker" {
		t.Errorf("audit error_type = %v, want %q (omitted arguments must reach normal validation, not a bespoke parse-error type)", got, "missing_broker")
	}
}

// TestCallToolHandler_NullArguments_AuditedAndValidated covers a third legal
// wire shape distinct from "omitted entirely": arguments present but JSON
// null. json.Unmarshal(null, &map) already succeeds with a nil map even on
// unfixed code (unlike the nil/empty-RawMessage case this ticket fixes), so
// this test passes both before and after the fix — it is a regression pin
// for this wire shape, not a red/green case, and is kept so a future change
// to the unmarshal guard can't regress it.
func TestCallToolHandler_NullArguments_AuditedAndValidated(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	session := callToolTestHarness(t)
	ctx := context.Background()

	// json.RawMessage("null") is a non-empty []byte, so the client SDK's
	// omitempty does not drop it — the literal `"arguments":null` reaches
	// the server, unlike a plain nil interface (which omitempty would
	// suppress, collapsing this into the omitted case above).
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "test-tool", Arguments: json.RawMessage("null")})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error instead of a tool result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected an error result (missing required broker), got success: %v", res.StructuredContent)
	}

	audits := auditLines(t, &logBuf, "test-tool")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for test-tool, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["error_type"]; got != "missing_broker" {
		t.Errorf("audit error_type = %v, want %q", got, "missing_broker")
	}
}

// TestCallToolHandler_MalformedArguments_AuditedAsError covers the other half
// of SOL-153765: arguments present but not a JSON object (genuinely
// malformed from this closure's point of view, since CallToolParams.Arguments
// is typed `any` client-side and can legally hold a non-object value). Before
// the fix this closure returned a bare protocol error and never audited the
// call. After the fix it must audit the failure itself (this path never
// reaches ToolManager.CallTool) and return the manager's normal
// structured/retryable error shape instead of a protocol error — and it must
// never echo the raw stdlib decode error text (which can contain client-
// supplied bytes) into the client-facing message or the audit "detail" field.
func TestCallToolHandler_MalformedArguments_AuditedAsError(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	session := callToolTestHarness(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "test-tool", Arguments: "not-an-object"})
	if err != nil {
		t.Fatalf("CallTool returned a protocol-level error instead of a structured tool result: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected an error result for malformed arguments, got success: %v", res.StructuredContent)
	}

	structured, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("expected structured content to be a JSON object, got %T: %v", res.StructuredContent, res.StructuredContent)
	}
	errMsg, _ := structured["error"].(string)
	if errMsg == "" {
		t.Fatalf("expected a non-empty client-facing error message, got: %v", structured)
	}
	if strings.Contains(errMsg, "not-an-object") || strings.Contains(errMsg, "cannot unmarshal") {
		t.Errorf("client-facing error echoes the raw stdlib decode error instead of a static message: %q", errMsg)
	}
	if retryable, ok := structured["retryable"].(bool); !ok || retryable {
		t.Errorf("expected retryable=false, got %v", structured["retryable"])
	}

	audits := auditLines(t, &logBuf, "test-tool")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for test-tool, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["outcome"]; got != "error" {
		t.Errorf("audit outcome = %v, want %q", got, "error")
	}
	if got := audits[0]["error_type"]; got != "bad_request" {
		t.Errorf("audit error_type = %v, want %q", got, "bad_request")
	}
	// secure-logging-rules.md: unvouched error text is logged by Go type
	// only, never verbatim — the audit "detail" field must not carry the raw
	// client-supplied bytes from the failed decode.
	if detail, _ := audits[0]["detail"].(string); strings.Contains(detail, "not-an-object") {
		t.Errorf("audit detail leaked raw decode error text: %q", detail)
	}
}

// TestListBrokers_ResponseContainsOnlyAliases closes a Quality Plan gap
// (SOL-147161 §3.3): list-brokers is the one tool response an MCP client
// sees before it has picked a broker, so it's the one place a future change
// (e.g. "let's also return the URL for convenience") could leak credentials
// or broker addresses to the calling LLM/client with no per-tool review to
// catch it. TestCredentialsAreIsolatedPerBroker (test/integration) guards the
// outbound-wire side of this; this test guards the inbound tool-response side.
//
// Today RegisterListBrokers only ever marshals pool.Aliases() (see
// register.go), so this can't fail yet — it's a regression guard, not a
// currently-failing gap.
func TestListBrokers_ResponseContainsOnlyAliases(t *testing.T) {
	cfgYAML := "mcp_client_auth:\n  mode: disabled\nbrokers:\n" +
		"  broker-a:\n    url: https://broker-a.example.com:8443\n    auth:\n      mode: basic\n      username: alice\n      password: super-secret-passA\n" +
		"  broker-b:\n    url: https://broker-b.example.com:8443\n    auth:\n      mode: bearer\n      token: super-secret-token-B\n"
	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterListBrokers(server, pool, nil)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() {
		_ = server.Run(ctx, serverTransport)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list-brokers", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("list-brokers returned IsError: %v", res.Content)
	}

	raw := res.StructuredContent
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}

	// Shape check: the response is exactly {"brokers": [...alias strings...]},
	// nothing else.
	var decoded map[string]any
	if err := json.Unmarshal(rawJSON, &decoded); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if len(decoded) != 1 {
		t.Errorf("response has %d top-level keys, want exactly 1 (\"brokers\"): %s", len(decoded), rawJSON)
	}
	brokers, ok := decoded["brokers"].([]any)
	if !ok {
		t.Fatalf("brokers field is not an array: %s", rawJSON)
	}
	wantAliases := map[string]bool{"broker-a": true, "broker-b": true}
	seen := map[string]int{}
	for _, b := range brokers {
		s, ok := b.(string)
		if !ok || !wantAliases[s] {
			t.Errorf("unexpected broker entry %v (not a plain known alias)", b)
			continue
		}
		seen[s]++
	}
	for alias := range wantAliases {
		if seen[alias] != 1 {
			t.Errorf("alias %q appeared %d times in %v, want exactly once", alias, seen[alias], brokers)
		}
	}

	// Leakage check: none of the configured secrets, hostnames, or
	// credentials appear anywhere in the raw response bytes, regardless of
	// where in a future response shape they might get added.
	leaked := []string{
		"super-secret-passA", "super-secret-token-B", "alice",
		"broker-a.example.com", "broker-b.example.com", "https://",
	}
	for _, s := range leaked {
		if strings.Contains(string(rawJSON), s) {
			t.Errorf("list-brokers response leaked %q: %s", s, rawJSON)
		}
	}
}

// TestToMCPAnnotations exercises the translation boundary between our
// Annotations type and the SDK's *mcp.ToolAnnotations. The contract the
// refactor depends on is preserving nil-vs-explicit-false semantics on the
// pointer fields: nil means "unspecified, apply spec defaults", *false
// means "explicitly disabled". A future change that conflated these would
// silently flip default behavior on every tool.
func TestToMCPAnnotations(t *testing.T) {
	tr := func(b bool) *bool { return &b }

	cases := []struct {
		name string
		in   Annotations
		// pointer-equality check is too strict (toMCPAnnotations clones not
		// required by the boundary contract); we compare values via custom
		// matchers below.
		wantReadOnlyHint    bool
		wantIdempotentHint  bool
		wantDestructiveHint *bool // nil-vs-*false matters
		wantOpenWorldHint   *bool
	}{
		{
			name:                "empty annotations passes nil pointers + zero values through",
			in:                  Annotations{},
			wantReadOnlyHint:    false,
			wantIdempotentHint:  false,
			wantDestructiveHint: nil,
			wantOpenWorldHint:   nil,
		},
		{
			name:                "ReadOnly true sets ReadOnlyHint",
			in:                  Annotations{ReadOnly: true},
			wantReadOnlyHint:    true,
			wantDestructiveHint: nil,
			wantOpenWorldHint:   nil,
		},
		{
			name:                "Destructive *true preserved as *true",
			in:                  Annotations{Destructive: tr(true)},
			wantDestructiveHint: tr(true),
		},
		{
			name:                "Destructive *false preserved as *false (NOT nil)",
			in:                  Annotations{Destructive: tr(false)},
			wantDestructiveHint: tr(false),
		},
		{
			name:              "OpenWorld *true preserved as *true",
			in:                Annotations{OpenWorld: tr(true)},
			wantOpenWorldHint: tr(true),
		},
		{
			name:                "all four fields populated",
			in:                  Annotations{ReadOnly: true, Idempotent: true, Destructive: tr(false), OpenWorld: tr(true)},
			wantReadOnlyHint:    true,
			wantIdempotentHint:  true,
			wantDestructiveHint: tr(false),
			wantOpenWorldHint:   tr(true),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toMCPAnnotations(tc.in)
			if got == nil {
				t.Fatal("toMCPAnnotations returned nil")
			}
			if got.ReadOnlyHint != tc.wantReadOnlyHint {
				t.Errorf("ReadOnlyHint = %v, want %v", got.ReadOnlyHint, tc.wantReadOnlyHint)
			}
			if got.IdempotentHint != tc.wantIdempotentHint {
				t.Errorf("IdempotentHint = %v, want %v", got.IdempotentHint, tc.wantIdempotentHint)
			}
			if !boolPtrEqual(got.DestructiveHint, tc.wantDestructiveHint) {
				t.Errorf("DestructiveHint = %v, want %v", fmtBoolPtr(got.DestructiveHint), fmtBoolPtr(tc.wantDestructiveHint))
			}
			if !boolPtrEqual(got.OpenWorldHint, tc.wantOpenWorldHint) {
				t.Errorf("OpenWorldHint = %v, want %v", fmtBoolPtr(got.OpenWorldHint), fmtBoolPtr(tc.wantOpenWorldHint))
			}
		})
	}
}

// TestToMCPTool checks that every Metadata field reaches the corresponding
// mcp.Tool field, that InputSchema is run through injectBrokerParam, and
// that Annotations are translated via toMCPAnnotations.
func TestToMCPTool(t *testing.T) {
	pool := newRegTestPool(t)
	tr := func(b bool) *bool { return &b }

	meta := Metadata{
		Name:        "test-tool",
		Description: "describe me",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"foo": map[string]any{"type": "string"}},
			"required":   []string{"foo"},
		},
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
		Annotations: Annotations{ReadOnly: true, Destructive: tr(false)},
	}

	got := toMCPTool(meta, pool)
	if got == nil {
		t.Fatal("toMCPTool returned nil")
	}
	if got.Name != "test-tool" {
		t.Errorf("Name = %q, want %q", got.Name, "test-tool")
	}
	if got.Description != "describe me" {
		t.Errorf("Description = %q, want %q", got.Description, "describe me")
	}

	// InputSchema must have broker injected.
	in, ok := got.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("InputSchema is not map[string]any: %T", got.InputSchema)
	}
	props, _ := in["properties"].(map[string]any)
	if _, hasBroker := props["broker"]; !hasBroker {
		t.Error("InputSchema.properties missing injected 'broker' parameter")
	}
	if _, hasFoo := props["foo"]; !hasFoo {
		t.Error("InputSchema.properties lost original 'foo' parameter")
	}

	// OutputSchema passes through unchanged.
	out, ok := got.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("OutputSchema is not map[string]any: %T", got.OutputSchema)
	}
	if out["type"] != "object" {
		t.Errorf("OutputSchema lost type field, got %v", out["type"])
	}

	// Annotations went through toMCPAnnotations: ReadOnlyHint=true,
	// DestructiveHint=*false (preserved, NOT nil).
	if got.Annotations == nil {
		t.Fatal("Annotations is nil")
	}
	if !got.Annotations.ReadOnlyHint {
		t.Error("ReadOnlyHint = false, want true")
	}
	if got.Annotations.DestructiveHint == nil || *got.Annotations.DestructiveHint != false {
		t.Errorf("DestructiveHint = %v, want explicit *false", fmtBoolPtr(got.Annotations.DestructiveHint))
	}
}

// boolPtrEqual returns true when both pointers are nil OR both point to the
// same bool value. Used by TestToMCPAnnotations to assert the preserved
// nil-vs-*false semantics.
func boolPtrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// fmtBoolPtr renders a *bool for error messages: "nil", "*true", or "*false".
func fmtBoolPtr(p *bool) string {
	if p == nil {
		return "nil"
	}
	if *p {
		return "*true"
	}
	return "*false"
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Composition-site wiring tests (tool-authorization wrapper) ---
//
// Each test drives a real client→server call via mcp.NewInMemoryTransports
// and asserts on observable outputs (log stream, tool result), not internal
// composition state.

// hasAuthzAuditLine reports whether buf contains a "tool authorization" log
// record for the given tool name.
func hasAuthzAuditLine(t *testing.T, buf *bytes.Buffer, tool string) bool {
	t.Helper()
	for _, raw := range strings.Split(buf.String(), "\n") {
		if raw == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("non-JSON log line %q: %v", raw, err)
		}
		if entry["msg"] == "tool authorization" && entry["tool"] == tool {
			return true
		}
	}
	return false
}

// Nil policy → pre-RBAC "tool invoked" audit still fires; no
// "tool authorization" line (wrapper is not composed).
func TestRegisterWithServer_NilPolicy_ByteIdenticalToPreRBAC(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "test-tool", Arguments: args}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if len(auditLines(t, &logBuf, "test-tool")) != 1 {
		t.Errorf("pre-RBAC path must still emit exactly 1 tool-invoked audit line; got %d: %s",
			len(auditLines(t, &logBuf, "test-tool")), logBuf.String())
	}
	if hasAuthzAuditLine(t, &logBuf, "test-tool") {
		t.Error("nil-policy composition emitted a tool-authorization audit line; the wrapper must NOT be composed when policy is nil (silent RBAC on non-RBAC deployment)")
	}
}

// Non-nil policy → wrapper is composed AND consulted. The audit line fires
// AND the decision value matches the caller-facing outcome (asserting only
// on line presence would miss a wrapper emitting unconditional logs).
// No OAuth middleware here, so TokenInfo is nil → missing-claim branch.
func TestRegisterWithServer_NonNilPolicy_AuthorizationAuditFires(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))

	policy := policyGranting(t, []string{"Ops"}, "test-tool")

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, policy, "groups")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "test-tool", Arguments: args})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	// Missing-claim branch → tool-level deny result.
	if !res.IsError {
		t.Errorf("expected IsError=true from missing-claim path; the wrapper was not composed or did not consult policy")
	}

	// Audit decision must match the outcome — catches a wrapper that emits
	// an unconditional "allowed" log while denying on the result path.
	lines := authzLogLines(t, &logBuf)
	toolLines := make([]map[string]any, 0, 1)
	for _, l := range lines {
		if l["tool"] == "test-tool" {
			toolLines = append(toolLines, l)
		}
	}
	if len(toolLines) != 1 {
		t.Fatalf("expected exactly 1 tool-authorization audit line for test-tool, got %d: %s", len(toolLines), logBuf.String())
	}
	decision, _ := toolLines[0]["decision"].(string)
	if decision == "" {
		t.Fatalf("audit line missing decision field: %v", toolLines[0])
	}
	// The audit's decision value must not indicate allow — that would
	// mean the wrapper is either not consulting policy or emitting an
	// unconditional log that says the call was permitted despite the
	// deny result the caller received above. The exact decision string
	// schema is finalized in the audit-refinement follow-up ticket;
	// this test locks the composition contract (wrapper ran, decision
	// matches the deny-side outcome), not the literal string.
	if strings.Contains(strings.ToLower(decision), "allow") {
		t.Errorf("audit decision %q indicates allow but the caller-facing result was IsError=true; wrapper is inconsistent with its own outcome", decision)
	}
}

// list-brokers is structurally exempt. Even under a policy that grants it
// nothing, the call succeeds and no "tool authorization" audit line fires.
func TestRegisterListBrokers_NeverComposesWithAuthorization(t *testing.T) {
	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(oldLogger)

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))

	// If the wrapper were accidentally composed on list-brokers, this
	// policy (which grants nothing to it) would deny the call.
	policy := policyGranting(t, []string{"Ops"}, "test-tool")

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, policy, "groups")
	RegisterListBrokers(server, pool, nil)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list-brokers", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Errorf("list-brokers returned IsError=true under a policy-enabled server; the exemption is broken. Content=%v", res.Content)
	}
	if hasAuthzAuditLine(t, &logBuf, "list-brokers") {
		t.Errorf("list-brokers emitted a tool-authorization audit line; the wrapper must not be composed on the exempt tool: %s", logBuf.String())
	}
}
