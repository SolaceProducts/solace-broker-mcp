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
	"strings"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
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

	RegisterListBrokers(server, pool)
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
// detection the success branch emits "status": "success" at INFO for an
// invocation that panicked — misclassifying panics on the surface that feeds
// dashboards and SLOs. The audit line must instead report status=error with
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
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "panic-tool", Arguments: args}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	audits := auditLines(t, &logBuf, "panic-tool")
	if len(audits) != 1 {
		t.Fatalf("expected exactly 1 audit line for panic-tool, got %d: %s", len(audits), logBuf.String())
	}
	if got := audits[0]["status"]; got != "error" {
		t.Errorf("audit status = %v, want %q (a panic must never audit as success)", got, "error")
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
	RegisterListBrokers(server, pool)

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
	if got := audits[0]["status"]; got != "success" {
		t.Errorf("audit status = %v, want %q", got, "success")
	}
	// list-brokers resolves no broker; the audit line must omit the field
	// rather than carry an empty value.
	if _, present := audits[0]["broker"]; present {
		t.Errorf("audit line carries broker field for brokerless tool: %v", audits[0])
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
	RegisterListBrokers(server, pool)

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
