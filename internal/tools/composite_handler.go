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

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
)

// CompositeToolHandler adapts a YAML-driven composite tool definition to the
// ToolHandler interface. A single instance is created per composite tool at
// startup. All composite tools share this adapter type — each instance wraps
// a different CompositeTool definition and delegates execution to the shared
// CompositeExecutor.
type CompositeToolHandler struct {
	tool     composite.CompositeTool
	executor *composite.CompositeExecutor
}

// NewCompositeToolHandler creates a handler that adapts the given composite tool
// definition for use with the ToolManager.
func NewCompositeToolHandler(tool composite.CompositeTool, executor *composite.CompositeExecutor) *CompositeToolHandler {
	return &CompositeToolHandler{
		tool:     tool,
		executor: executor,
	}
}

// Metadata returns a fresh Metadata value built from the YAML-loaded composite
// tool definition. The input schema is computed from the tool's Parameters; the
// output schema is the generic step-keyed envelope shared by every composite
// tool's collect strategy. Each call returns a freshly allocated value with
// fresh maps inside, so callers cannot mutate shared state.
func (h *CompositeToolHandler) Metadata() Metadata {
	return Metadata{
		Name:         h.tool.Name,
		Description:  h.tool.Description,
		InputSchema:  buildCompositeInputSchema(h.tool.Parameters),
		OutputSchema: StepKeyedEnvelopeSchema(),
		Annotations:  toolAnnotations(h.tool.Annotations),
	}
}

// Handle executes the composite tool's steps against the SEMP client in the
// ToolContext and wraps the combined result in a ToolResult.
func (h *CompositeToolHandler) Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
	result, err := h.executor.Execute(ctx, h.tool, tc.SEMPv2Client, params)
	if err != nil {
		return nil, err
	}
	return &ToolResult{StructuredContent: result}, nil
}

// Executor returns the underlying composite executor. This is exposed for
// testing and for callers that need to interact with the executor directly.
func (h *CompositeToolHandler) Executor() *composite.CompositeExecutor {
	return h.executor
}

// buildCompositeInputSchema builds a JSON Schema object from the composite
// tool's parameter definitions. The broker parameter is injected later by
// register.go — handlers do not declare it.
func buildCompositeInputSchema(params []composite.ParameterDef) map[string]any {
	properties := make(map[string]any, len(params))
	var required []string

	for _, p := range params {
		prop := map[string]any{
			"type": p.Type,
		}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		// Empty is never valid for a required string — in this catalog they are
		// all object identifiers (VPN/queue/client names). Reject it at schema
		// validation rather than let it reach the broker, e.g. templated into a
		// malformed SEMPv2 path.
		if p.Type == "string" && p.Required {
			prop["minLength"] = 1
		}
		properties[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// toolAnnotations converts the YAML-declared annotations to our Annotations
// type. Pointer fields stay nil when YAML omitted them, letting the
// registration translator pass through to spec defaults. The *bool fields are
// CLONED rather than reused: Metadata() promises fresh state per call, and
// reusing the YAML-backed pointers would let a caller mutating *m.Destructive
// leak into the handler's persistent state and every subsequent Metadata() call.
func toolAnnotations(a composite.ToolAnnotations) Annotations {
	out := Annotations{
		Destructive: cloneBoolPtr(a.Destructive),
		OpenWorld:   cloneBoolPtr(a.OpenWorld),
	}
	if a.ReadOnly != nil {
		out.ReadOnly = *a.ReadOnly
	}
	if a.Idempotent != nil {
		out.Idempotent = *a.Idempotent
	}
	return out
}

// cloneBoolPtr returns a freshly-allocated *bool with the same value as p,
// or nil if p is nil. Used so Metadata()'s promise of fresh state per call
// holds even for the *bool fields on Annotations.
func cloneBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
