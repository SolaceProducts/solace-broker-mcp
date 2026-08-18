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
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// metadataCountingHandler wraps a real ToolHandler and counts Metadata()
// calls without modifying the wrapped handler or any production code, so a
// test can assert how many times Metadata() was actually invoked through the
// normal ToolManager.Register()/CallTool path.
type metadataCountingHandler struct {
	ToolHandler
	calls int
}

func (h *metadataCountingHandler) Metadata() Metadata {
	h.calls++
	return h.ToolHandler.Metadata()
}

// TestCallTool_CompositeWriteTool_OutputSchemaCachedNotRebuilt is the
// verification SOL-153335 asked for, against the real create-queue tool
// rather than a test stub: CompositeToolHandler.Metadata() — which rebuilds
// the strict output schema from the full SEMPv2 operation catalog via
// CompositeExecutor.Operations() and composite.BuildStrictOutputSchema — is
// called exactly once, at Register(), and never again across many CallTool
// invocations.
//
// This was already true once SOL-153334 landed (CallTool stopped calling
// handler.Metadata() at all, for a different reason — reusing a cached
// compiled JSON-schema validator instead of recompiling it per call); this
// test is the standalone proof SOL-153335 itself asked for, so a future
// regression that reintroduces a handler.Metadata() call inside CallTool is
// caught here even if it doesn't touch the JSON-schema validation path.
func TestCallTool_CompositeWriteTool_OutputSchemaCachedNotRebuilt(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	var tool composite.CompositeTool
	found := false
	for _, tl := range realTools {
		if tl.Name == "create-queue" {
			tool = tl
			found = true
			break
		}
	}
	if !found {
		t.Fatal("create-queue not found in the real catalog")
	}

	executor := composite.NewCompositeExecutor(operations)
	real := NewCompositeToolHandler(tool, executor)
	counted := &metadataCountingHandler{ToolHandler: real}

	mgr := NewToolManager(newTestPool(t))
	mgr.Register(counted)

	callsAfterRegister := counted.calls
	if callsAfterRegister == 0 {
		t.Fatal("expected Register() to call Metadata() at least once")
	}

	// The test pool's "dev" broker has no real listener, so each call fails
	// at the SEMP request — expected, and irrelevant to what this test
	// checks. Metadata() must not be invoked again regardless of whether the
	// underlying broker call succeeds: CallTool must return a structured
	// result either way (SOL-152980), never a bare protocol error.
	for i := 0; i < 5; i++ {
		result, err := mgr.CallTool(context.Background(), "create-queue", map[string]any{
			"broker":     "dev",
			"msgVpnName": "default",
			"queueName":  "test-queue",
		}, Identity{})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if result == nil {
			t.Fatal("expected a non-nil CallToolResult")
		}
	}

	if got := counted.calls - callsAfterRegister; got != 0 {
		t.Errorf("handler.Metadata() called %d more time(s) across 5 CallTool invocations on the real create-queue tool; want 0 (SOL-153335: output schema/operation catalog must not be rebuilt per call)", got)
	}
}
