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
	return semp.NewBrokerPool(cfg)
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
	RegisterWithServer(mgr, server, pool)

	// No panic = tools registered successfully. The MCP SDK doesn't expose
	// a way to list registered tools directly, so we verify by checking
	// that AddTool was called without error.
}

func TestRegisterListBrokers(t *testing.T) {
	pool := newRegTestPool(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)

	RegisterListBrokers(server, pool)
	// No panic = registered successfully.
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
