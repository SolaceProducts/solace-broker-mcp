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

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// Handle executes the composite tool's steps against the SEMP client in the
// ToolContext and wraps the combined result in a ToolResult.
func (h *CompositeToolHandler) Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
	result, err := h.executor.Execute(ctx, h.tool, tc.SEMPv2Client, params)
	if err != nil {
		return nil, err
	}
	return &ToolResult{StructuredContent: result}, nil
}

// Schema builds a JSON Schema object from the composite tool's parameter
// definitions. This does not include the broker parameter — the ToolManager
// injects that during registration and validation.
func (h *CompositeToolHandler) Schema() map[string]any {
	properties := make(map[string]any, len(h.tool.Parameters))
	var required []string

	for _, p := range h.tool.Parameters {
		prop := map[string]any{
			"type": p.Type,
		}
		if p.Description != "" {
			prop["description"] = p.Description
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

// OutputSchema returns a generic envelope schema for composite tool results.
// The collect strategy returns an object keyed by step ID, where each value is
// a SEMP response object. This validates the envelope structure without
// constraining individual SEMP response fields.
func (h *CompositeToolHandler) OutputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "object"},
	}
}

// Annotations converts the composite tool's YAML-declared annotations to MCP
// SDK ToolAnnotations. Nil pointers from YAML (field omitted) are passed
// through as nil, letting the SDK defaults apply.
func (h *CompositeToolHandler) Annotations() *mcp.ToolAnnotations {
	a := h.tool.Annotations
	ann := &mcp.ToolAnnotations{
		DestructiveHint: a.Destructive,
		OpenWorldHint:   a.OpenWorld,
	}
	if a.ReadOnly != nil {
		ann.ReadOnlyHint = *a.ReadOnly
	}
	if a.Idempotent != nil {
		ann.IdempotentHint = *a.Idempotent
	}
	return ann
}

// Description returns the tool's human-readable description for LLM tool selection.
func (h *CompositeToolHandler) Description() string {
	return h.tool.Description
}

// Name returns the tool's unique identifier.
func (h *CompositeToolHandler) Name() string {
	return h.tool.Name
}

// Executor returns the underlying composite executor. This is exposed for
// testing and for callers that need to interact with the executor directly.
func (h *CompositeToolHandler) Executor() *composite.CompositeExecutor {
	return h.executor
}
