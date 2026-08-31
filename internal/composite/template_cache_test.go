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

package composite

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// TestLoadTools_CompilesStepTemplatesOnce guards SOL-153764's core ask: every
// step's Args/ForEachIf templates are parsed once at load time and cached on
// Step, rather than re-parsed on every call. It runs against the real
// embedded tools.yaml (not just a synthetic fixture) so it pins actual
// production definitions, e.g. list-vpns' real-clients fan-out step.
func TestLoadTools_CompilesStepTemplatesOnce(t *testing.T) {
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	checked := 0
	for _, tool := range tools {
		for _, step := range tool.Steps {
			for key := range step.Args {
				checked++
				if step.compiledArgs[key] == nil {
					t.Errorf("tool %s step %s: arg %q was not precompiled", tool.Name, step.ID, key)
				}
			}
			if step.ForEachIf != "" {
				checked++
				if step.compiledForEachIf == nil {
					t.Errorf("tool %s step %s: forEachIf was not precompiled", tool.Name, step.ID)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no templated args/forEachIf found in embedded tools.yaml; test assumption is stale")
	}
}

// TestResolveArgs_UsesCompiledTemplate_IgnoresLaterSourceMutation proves
// ResolveArgs executes the precompiled template rather than re-parsing
// step.Args on every call. It compiles the step, then corrupts the raw
// source string afterward: if ResolveArgs re-parsed step.Args on every call
// (the pre-fix behavior), this would now fail to parse. It succeeding, with
// the pre-corruption value, is only possible if the compiled template — not
// the mutated source — is what actually executed.
func TestResolveArgs_UsesCompiledTemplate_IgnoresLaterSourceMutation(t *testing.T) {
	step := Step{
		ID:   "s1",
		Args: map[string]string{"foo": "{{.Params.x}}"},
	}
	if err := compileStepTemplates(&step); err != nil {
		t.Fatalf("compileStepTemplates: %v", err)
	}

	// Corrupt the raw source after compiling.
	step.Args["foo"] = "{{ .broken"

	execCtx := &ExecuteContext{Params: map[string]any{"x": "resolved-value"}}
	resolved, err := ResolveArgs(step, execCtx)
	if err != nil {
		t.Fatalf("ResolveArgs errored, meaning it re-parsed the corrupted source instead of using the compiled template: %v", err)
	}
	if resolved["foo"] != "resolved-value" {
		t.Errorf(`resolved["foo"] = %v, want %q`, resolved["foo"], "resolved-value")
	}
}

// TestFanOut_ForEachIf_UsesCompiledTemplate_IgnoresLaterSourceMutation is the
// ForEachIf analogue of the ResolveArgs test above, exercised through the
// full fetchFanOut path (resolveTemplateString's compiled-template argument).
func TestFanOut_ForEachIf_UsesCompiledTemplate_IgnoresLaterSourceMutation(t *testing.T) {
	client := newMockClient()
	client.responses["getMsgVpns"] = &sempv2.Result{
		Data: vpnRows(map[string]any{"msgVpnName": "a", "enabled": true}),
	}

	tool := fanOutTool()
	tool.Steps[1].ForEachIf = "{{.Item.enabled}}"
	if err := compileStepTemplates(&tool.Steps[1]); err != nil {
		t.Fatalf("compileStepTemplates: %v", err)
	}

	// Corrupt the raw source after compiling.
	tool.Steps[1].ForEachIf = "{{ .broken"

	ce := NewCompositeExecutor(testOperations())
	out, err := ce.Execute(context.Background(), tool, client, nil)
	if err != nil {
		t.Fatalf("Execute errored, meaning forEachIf was re-parsed from the corrupted source instead of using the compiled template: %v", err)
	}
	clients, ok := out["clients"].(map[string]any)
	if !ok {
		t.Fatalf("clients step result missing or wrong type: %T", out["clients"])
	}
	byKey, ok := clients["byKey"].(map[string]any)
	if !ok {
		t.Fatalf("byKey missing or wrong type: %T", clients["byKey"])
	}
	if len(byKey) != 1 {
		t.Errorf("byKey size: got %d, want 1 (enabled=true row from the pre-corruption predicate)", len(byKey))
	}
}

// TestLoadTools_RejectsMalformedArgTemplate documents a deliberate behavior
// change: compiling templates at load time means a syntactically invalid arg
// template now fails fast at server startup (LoadTools) instead of only
// surfacing on the first tool invocation that reaches it.
func TestLoadTools_RejectsMalformedArgTemplate(t *testing.T) {
	yaml := `
tools:
  - name: t
    description: d
    steps:
      - id: s1
        operation: monitor/getMsgVpnQueue
        args:
          msgVpnName: "{{.Params.msgVpnName"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(yaml)}}
	if _, err := LoadTools(fsys, "tools.yaml"); err == nil {
		t.Fatal("expected LoadTools to reject a malformed arg template, got nil error")
	}
}

// TestLoadTools_RejectsMalformedForEachIfTemplate is the ForEachIf analogue
// of TestLoadTools_RejectsMalformedArgTemplate.
func TestLoadTools_RejectsMalformedForEachIfTemplate(t *testing.T) {
	yaml := `
tools:
  - name: t
    description: d
    steps:
      - id: vpns
        operation: monitor/getMsgVpns
      - id: clients
        operation: monitor/getMsgVpnClients
        forEach: vpns
        forEachKey: msgVpnName
        forEachIf: "{{.Item.enabled"
    result:
      strategy: collect
`
	fsys := fstest.MapFS{"tools.yaml": &fstest.MapFile{Data: []byte(yaml)}}
	if _, err := LoadTools(fsys, "tools.yaml"); err == nil {
		t.Fatal("expected LoadTools to reject a malformed forEachIf template, got nil error")
	}
}
