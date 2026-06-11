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
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInternalErrorMessage is returned to the agent when a tool handler
// panics. Mirrors genericInternalMessage but names the server, not the
// broker, as the failing component; the panic detail stays server-side.
const serverInternalErrorMessage = "The server encountered an internal error executing this tool. Please try again later or contact your administrator."

// withRecovery wraps an SDK tool handler so a panic anywhere below the SDK
// boundary becomes a sanitized error result instead of killing the process.
// The SDK (go-sdk v1.5.0) invokes tool handlers on a goroutine of its own
// with no recover() — net/http's per-connection recovery cannot catch a
// panic on another goroutine — so without this wrapper a single panicking
// handler takes down the whole server (SOL-150685).
func withRecovery(toolName string, h mcp.ToolHandler) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		defer func() {
			if r := recover(); r != nil {
				// Log only the panic value's Go type, never its text: panic
				// values are unaudited and can carry arbitrary strings (the
				// same rule logToolResult applies to non-broker errors). The
				// stack trace pinpoints the panic site without echoing the
				// value, and the agent sees only the generic message below,
				// matching the unknown-error branch of buildErrorMessage.
				slog.Error("tool handler panicked",
					slog.String("tool", toolName),
					slog.String("panic_type", fmt.Sprintf("%T", r)),
					slog.String("stack", string(debug.Stack())))
				result = &mcp.CallToolResult{
					StructuredContent: map[string]any{
						"error":     serverInternalErrorMessage,
						"retryable": false,
					},
					Content: []mcp.Content{&mcp.TextContent{Text: serverInternalErrorMessage}},
					IsError: true,
				}
				err = nil
			}
		}()
		return h(ctx, req)
	}
}

// RegisterWithServer registers all tools from the ToolManager with the MCP
// server. For each handler, it builds an mcp.Tool from the handler's Metadata
// (with the broker parameter injected into the input schema) and creates a
// handler closure that delegates to ToolManager.CallTool.
//
// This function is the only translation boundary between our internal Metadata
// type and the SDK's mcp.Tool. Handlers and the manager work in our own
// vocabulary; this is where it crosses over to the SDK.
func RegisterWithServer(mgr *ToolManager, server *mcp.Server, pool *semp.BrokerPool) {
	type registration struct {
		name    string
		handler ToolHandler
		meta    Metadata
	}

	handlers := mgr.Handlers()
	regs := make([]registration, 0, len(handlers))
	for _, h := range handlers {
		meta := h.Metadata()
		regs = append(regs, registration{name: meta.Name, handler: h, meta: meta})
	}
	// Sort by name for deterministic registration and tools/list ordering.
	sort.Slice(regs, func(i, j int) bool { return regs[i].name < regs[j].name })

	for _, reg := range regs {
		mcpTool := toMCPTool(reg.meta, pool)

		server.AddTool(mcpTool, withRecovery(reg.name, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var params map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
				return nil, fmt.Errorf("parsing tool arguments: %w", err)
			}
			// Extract per-invocation audit identity from the SDK request
			// extras (SOL-149606). Both req.Extra and req.Extra.TokenInfo
			// can be nil in disabled mode (no middleware) or under test
			// scaffolding that constructs a bare CallToolRequest; the
			// constructor handles nil cleanly.
			var info *sdkauth.TokenInfo
			if req.Extra != nil {
				info = req.Extra.TokenInfo
			}
			return mgr.CallTool(ctx, reg.name, params, NewIdentityFromTokenInfo(info))
		}))
	}
}

// toMCPTool converts our Metadata to the SDK's mcp.Tool, injecting the broker
// parameter into the input schema. This is the only place in the codebase
// where our types and SDK types meet.
func toMCPTool(m Metadata, pool *semp.BrokerPool) *mcp.Tool {
	return &mcp.Tool{
		Name:         m.Name,
		Description:  m.Description,
		InputSchema:  injectBrokerParam(m.InputSchema, pool),
		OutputSchema: m.OutputSchema,
		Annotations:  toMCPAnnotations(m.Annotations),
	}
}

// toMCPAnnotations converts our Annotations to the SDK's mcp.ToolAnnotations.
func toMCPAnnotations(a Annotations) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    a.ReadOnly,
		DestructiveHint: a.Destructive,
		IdempotentHint:  a.Idempotent,
		OpenWorldHint:   a.OpenWorld,
	}
}

// RegisterListBrokers registers a list-brokers discovery tool that returns all
// configured broker aliases. This is a standalone tool, not a ToolHandler
// implementation — it does not call SEMP or require broker resolution.
func RegisterListBrokers(server *mcp.Server, pool *semp.BrokerPool) {
	server.AddTool(
		&mcp.Tool{
			Name:        "list-brokers",
			Description: "List all configured broker aliases. Use one of these as the 'broker' parameter on any other tool.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
			OutputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"brokers": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				"required": []string{"brokers"},
			},
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint: true,
			},
		},
		withRecovery("list-brokers", func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
			// This handler does not flow through ToolManager.CallTool, so it
			// emits the "tool invoked" audit line itself — every tool
			// invocation must reach the audit surface (no broker attr: this
			// tool resolves none). Same panic-detection contract as
			// CallTool's defer: error returns set toolErr, the success
			// return sets result, both nil means a panic is unwinding.
			start := time.Now()
			var brokerAlias, errorType string
			var toolErr error
			var info *sdkauth.TokenInfo
			if req.Extra != nil {
				info = req.Extra.TokenInfo
			}
			id := NewIdentityFromTokenInfo(info)
			defer func() {
				if toolErr == nil && result == nil {
					errorType = "panic"
					toolErr = panicError{}
				}
				logToolResult(ctx, "list-brokers", &brokerAlias, start, &errorType, &toolErr, id)
			}()

			aliases := pool.Aliases()
			structured := map[string]any{"brokers": aliases}
			resultJSON, mErr := json.MarshalIndent(structured, "", "  ")
			if mErr != nil {
				errorType = "marshal_error"
				toolErr = fmt.Errorf("marshalling broker list: %w", mErr)
				return nil, toolErr
			}
			return &mcp.CallToolResult{
				StructuredContent: structured,
				Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
			}, nil
		}),
	)
}

// injectBrokerParam clones the given input schema and adds the required broker
// parameter with a description listing all available broker aliases.
//
// Currently performs a shallow clone of the schema map with deep copies of the
// mutated keys (properties, required). If future ToolHandler implementations
// use nested schema keywords (oneOf, if/then/else, $defs), this should be
// replaced with a full recursive deep copy.
func injectBrokerParam(schema map[string]any, pool *semp.BrokerPool) map[string]any {
	aliases := pool.Aliases()

	// Shallow-clone the schema to preserve all existing keywords.
	cloned := make(map[string]any, len(schema)+1)
	for k, v := range schema {
		cloned[k] = v
	}

	// Deep-copy properties (we're adding to this map).
	origProps, _ := schema["properties"].(map[string]any)
	properties := make(map[string]any, len(origProps)+1)
	for k, v := range origProps {
		properties[k] = v
	}
	properties["broker"] = map[string]any{
		"type":        "string",
		"description": fmt.Sprintf("Target broker alias (required). Available brokers: %s", strings.Join(aliases, ", ")),
	}
	cloned["properties"] = properties

	// Deep-copy required list and prepend broker.
	required := []string{"broker"}
	if origRequired, ok := schema["required"].([]string); ok {
		required = append(required, origRequired...)
	}
	cloned["required"] = required

	return cloned
}
