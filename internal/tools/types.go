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

// Package tools provides the Tool Manager foundation for the Solace Broker MCP
// Server. It defines the ToolHandler interface, manages tool registration and
// routing, validates parameters and output against JSON schemas, resolves broker
// connections, and produces structured MCP responses.
//
// NOTE: Broker resolution, parameter/output validation, structured output
// serialization, and error handling in this package are generic plumbing shared
// by any MCP primitive. When MCP resources are introduced, extract this shared
// plumbing into a common package to avoid duplication between tools and
// resources.
package tools

import (
	"context"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// ToolContext holds request-scoped state for tool execution. The resolved SEMP
// clients are provided by the ToolManager after broker resolution — handlers
// never access the broker pool directly. Both protocol clients are populated
// for every tool call; handlers read whichever one their tool requires
// (a tool may also use both for mixed-protocol operations).
type ToolContext struct {
	SEMPv1Client sempv1.Client
	SEMPv2Client sempv2.Client
	// Future: UserIdentity, RequestID, etc.
}

// ToolResult holds the structured output from a tool execution. The
// StructuredContent is a JSON-serializable map returned as the MCP response's
// structuredContent field and also serialized as a TextContent fallback.
type ToolResult struct {
	StructuredContent map[string]any
	IsError           bool
}

// Annotations carries the behavioral hints a tool advertises to MCP clients.
// It mirrors the subset of the MCP spec's tool annotations this server cares
// about, but stays decoupled from the SDK type. Translation to the SDK shape
// happens once, at registration, in register.go. Pointer fields use nil to
// mean "unspecified — apply spec defaults"; value fields use Go's zero value.
// The pointer/value split mirrors the SDK so the translator is a 1:1 copy.
type Annotations struct {
	// ReadOnly indicates the tool performs no state changes. Default false.
	ReadOnly bool

	// Destructive indicates the tool may perform destructive updates. Only
	// meaningful when ReadOnly is false. Nil means unspecified.
	Destructive *bool

	// Idempotent indicates that repeated calls with the same arguments produce
	// the same result and have no additional effect. Default false.
	Idempotent bool

	// OpenWorld indicates the tool interacts with systems outside this server's
	// closed world. Nil means unspecified.
	OpenWorld *bool
}

// Metadata describes a tool to the MCP layer: its identity, schemas, and
// behavioral hints. Each ToolHandler returns a fresh Metadata value (and fresh
// maps inside it) per call so callers cannot mutate shared state. Translation
// to the SDK's mcp.Tool happens at the registration boundary.
type Metadata struct {
	// Name is the tool's unique identifier used for routing.
	Name string

	// Description is the human-readable description used by LLMs for tool
	// selection. See internal/composite/definitions/tools.yaml for guidelines.
	Description string

	// InputSchema is the JSON Schema (Draft 7) for the tool's parameters. The
	// broker parameter is injected by the ToolManager during registration —
	// handlers do not declare it.
	InputSchema map[string]any

	// OutputSchema is the JSON Schema for the tool's structured output. The
	// manager validates results against this before returning to the agent.
	OutputSchema map[string]any

	// Annotations are the behavioral hints (read-only, destructive, etc.).
	Annotations Annotations
}

// ToolHandler is the interface every tool implementation satisfies. Composite
// YAML-driven tools implement this via CompositeToolHandler; native Go
// handlers implement it directly.
type ToolHandler interface {
	// Metadata returns a fresh Metadata value describing the tool. Handlers
	// must allocate fresh maps inside the returned value so callers cannot
	// mutate shared state.
	Metadata() Metadata

	// Handle executes the tool with the given context and parameters. The
	// ToolContext carries resolved SEMP clients for the target broker.
	// Parameters have already been validated against Metadata().InputSchema
	// by the manager.
	Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error)
}

// EmptyObjectSchema returns the JSON Schema for an object with no properties.
// Used by tools that take no parameters beyond the broker (which the manager
// injects). Each call returns a freshly allocated map.
func EmptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// StepKeyedEnvelopeSchema returns the JSON Schema for a step-keyed envelope:
// a top-level object whose keys are step IDs and whose values are SEMP
// response objects. Used by every SEMPv1 tool returning the broker's full
// response without curation. Each call returns a freshly allocated map.
func StepKeyedEnvelopeSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": map[string]any{"type": "object"},
	}
}

// ReadOnlyAnnotations returns the standard annotations for a read-only,
// non-destructive tool. Each call returns a freshly allocated value so
// callers can mutate it without affecting other tools.
func ReadOnlyAnnotations() Annotations {
	destructive := false
	return Annotations{
		ReadOnly:    true,
		Destructive: &destructive,
	}
}
