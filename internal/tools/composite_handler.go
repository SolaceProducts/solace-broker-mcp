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
	"strings"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
)

// writeToolIdentifierFields names, per write tool, the response field(s) that
// identify the resource being created/updated — required in the generated
// output schema, because a response that doesn't name its own resource is
// broken, not sparse (see composite.BuildStrictOutputSchema). Every entry
// here is a create/update tool; delete and action tools have no response
// data (SempMetaOnlyResponse) and fall back to the generator's permissive
// per-step schema automatically — confirmed directly against the embedded
// spec for every write tool (SOL-152947), not assumed.
var writeToolIdentifierFields = map[string][]string{
	"create-message-vpn":    {"msgVpnName"},
	"update-message-vpn":    {"msgVpnName"},
	"create-queue":          {"msgVpnName", "queueName"},
	"update-queue":          {"msgVpnName", "queueName"},
	"create-topic-endpoint": {"msgVpnName", "topicEndpointName"},
	"update-topic-endpoint": {"msgVpnName", "topicEndpointName"},
	"create-rdp":            {"msgVpnName", "restDeliveryPointName"},
	"update-rdp":            {"msgVpnName", "restDeliveryPointName"},
}

// callsConfigOrActionOperation reports whether any step of tool calls a
// SEMPv2 config or action operation (as opposed to monitor). Purely
// structural — no separate flag to keep in sync with tools.yaml. Distinct
// from register.go's isWriteTool(Annotations), which checks !ReadOnly for
// the enable_write_tools gate — this checks the step's operation prefix
// specifically, for output-schema generation.
func callsConfigOrActionOperation(tool composite.CompositeTool) bool {
	for _, step := range tool.Steps {
		if strings.HasPrefix(step.Operation, "config/") || strings.HasPrefix(step.Operation, "action/") {
			return true
		}
	}
	return false
}

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
// tool definition. The input schema is computed from the tool's Parameters.
// The output schema is the generic step-keyed envelope for monitor (read)
// tools; for write tools (SOL-152947) it's generated from the operation's
// resolved response fields instead — monitor tools keep the generic envelope
// for now (SOL-150785's equivalent generator work for read tools is a
// separate, not-yet-scheduled follow-up). Each call returns a freshly
// allocated value with fresh maps inside, so callers cannot mutate shared
// state.
func (h *CompositeToolHandler) Metadata() Metadata {
	return Metadata{
		Name:         h.tool.Name,
		Description:  h.tool.Description,
		InputSchema:  buildCompositeInputSchema(h.tool.Parameters),
		OutputSchema: h.outputSchema(),
		Annotations:  toolAnnotations(h.tool.Annotations),
	}
}

// outputSchema returns the strict, spec-derived schema for a write tool, or
// the generic step-keyed envelope for everything else.
func (h *CompositeToolHandler) outputSchema() map[string]any {
	if !callsConfigOrActionOperation(h.tool) {
		return StepKeyedEnvelopeSchema()
	}
	var required map[string][]string
	if fields, ok := writeToolIdentifierFields[h.tool.Name]; ok && len(h.tool.Steps) > 0 {
		required = map[string][]string{h.tool.Steps[0].ID: fields}
	}
	return composite.BuildStrictOutputSchema(h.tool, h.executor.Operations(), required)
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
		// Every required string in this catalog is an identifier or selector
		// where empty is never valid (and, as a path segment, produces a
		// malformed SEMPv2 URL). Reject it at schema validation.
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
