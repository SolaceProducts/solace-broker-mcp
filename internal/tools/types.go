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

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolContext holds request-scoped state for tool execution. The resolved SEMP
// client is provided by the ToolManager after broker resolution — handlers
// never access the broker pool directly.
type ToolContext struct {
	SEMPClient sempv2.Client
	// Future: UserIdentity, RequestID, etc.
}

// ToolResult holds the structured output from a tool execution. The
// StructuredContent is a JSON-serializable map returned as the MCP response's
// structuredContent field and also serialized as a TextContent fallback.
type ToolResult struct {
	StructuredContent map[string]any
	IsError           bool
}

// ToolHandler is the interface that all tool implementations must satisfy.
// Composite YAML-driven tools implement this via CompositeToolHandler. Future
// Go-native tool handlers (write, action) implement it directly.
type ToolHandler interface {
	// Handle executes the tool with the given context and parameters. The
	// ToolContext carries the resolved SEMP client for the target broker.
	// Parameters have already been validated against Schema() by the manager.
	Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error)

	// Schema returns the JSON Schema (Draft 7) for the tool's input parameters.
	// This does not include the broker parameter — the manager injects that.
	Schema() map[string]any

	// OutputSchema returns the JSON Schema for the tool's structured output.
	// The manager validates results against this before returning to the agent.
	OutputSchema() map[string]any

	// Annotations returns MCP tool annotations (readOnly, destructive, etc.)
	// used by clients to understand tool behavior.
	Annotations() *mcp.ToolAnnotations

	// Description returns a human-readable tool description used by LLMs for
	// tool selection. See tools.yaml header comment for description guidelines.
	Description() string

	// Name returns the tool's unique identifier used for routing.
	Name() string
}
