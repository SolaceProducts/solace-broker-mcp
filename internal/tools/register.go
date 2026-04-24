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
	"sort"
	"strings"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RegisterWithServer registers all tools from the ToolManager with the MCP
// server. For each handler, it builds an mcp.Tool with the broker parameter
// injected into the input schema, sets the output schema and annotations, and
// creates a handler closure that delegates to ToolManager.CallTool.
func RegisterWithServer(mgr *ToolManager, server *mcp.Server, pool *semp.BrokerPool) {
	// Sort handlers by name for deterministic registration and tools/list ordering.
	handlers := mgr.Handlers()
	sort.Slice(handlers, func(i, j int) bool {
		return handlers[i].Name() < handlers[j].Name()
	})

	for _, handler := range handlers {
		h := handler // capture for closure
		schema := injectBrokerParam(h.Schema(), pool)

		mcpTool := &mcp.Tool{
			Name:         h.Name(),
			Description:  h.Description(),
			InputSchema:  schema,
			OutputSchema: h.OutputSchema(),
			Annotations:  h.Annotations(),
		}

		server.AddTool(mcpTool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var params map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
				return nil, fmt.Errorf("parsing tool arguments: %w", err)
			}
			return mgr.CallTool(ctx, h.Name(), params)
		})
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
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			aliases := pool.Aliases()
			result := map[string]any{"brokers": aliases}
			resultJSON, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshalling broker list: %w", err)
			}
			return &mcp.CallToolResult{
				StructuredContent: result,
				Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
			}, nil
		},
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
