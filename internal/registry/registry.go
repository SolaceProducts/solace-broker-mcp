// Package registry bridges composite tool definitions to the MCP SDK. It
// creates MCP tools with JSON schemas from composite parameter definitions,
// injects the broker parameter into every tool, and wires handlers that
// resolve the broker client before calling the composite executor.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registry creates MCP tools from composite definitions, registers them with
// the MCP server, and handles broker resolution per tool call. Each tool gets
// a handler closure that extracts the broker parameter, resolves the client
// from the pool, and delegates to the composite executor.
type Registry struct {
	server   *mcp.Server
	pool     *semp.BrokerPool
	executor *composite.CompositeExecutor
}

// NewRegistry creates a Registry that will register tools on the given MCP
// server, resolve broker clients from the pool, and execute tools via the
// composite executor.
func NewRegistry(server *mcp.Server, pool *semp.BrokerPool, executor *composite.CompositeExecutor) *Registry {
	return &Registry{
		server:   server,
		pool:     pool,
		executor: executor,
	}
}

// RegisterAll registers all composite tools with the MCP server. Each tool
// gets an MCP schema (with broker parameter injected) and a handler that
// resolves the broker and calls the executor. Returns an error on the first
// registration failure — the server should not start with partially registered
// tools.
func (r *Registry) RegisterAll(tools []composite.CompositeTool) error {
	for i := range tools {
		if err := r.registerTool(&tools[i]); err != nil {
			return fmt.Errorf("registering tool %q: %w", tools[i].Name, err)
		}
	}
	return nil
}

// registerTool builds an MCP tool from a composite tool definition and
// registers it with the server. It builds the input schema from the tool's
// parameters, injects the broker parameter, creates a handler closure, and
// calls server.AddTool.
func (r *Registry) registerTool(tool *composite.CompositeTool) error {
	schema := r.buildInputSchema(tool.Parameters)

	mcpTool := &mcp.Tool{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: schema,
	}

	toolCopy := *tool
	handler := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return r.handleToolCall(ctx, req, &toolCopy)
	}

	r.server.AddTool(mcpTool, handler)
	return nil
}

// RegisterListBrokers registers a list-brokers discovery tool that returns all
// configured broker aliases. This is a hardcoded tool, not a composite tool —
// it does not call SEMP.
func (r *Registry) RegisterListBrokers() {
	r.server.AddTool(
		&mcp.Tool{
			Name:        "list-brokers",
			Description: "List all configured broker aliases. Use one of these as the 'broker' parameter on any other tool.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			aliases := r.pool.Aliases()
			result := map[string]any{"brokers": aliases}
			resultJSON, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return nil, fmt.Errorf("marshalling broker list: %w", err)
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
			}, nil
		},
	)
}

// handleToolCall is the per-tool handler that extracts the broker, resolves the
// client, and delegates to the composite executor. It is called for every MCP
// tool call.
func (r *Registry) handleToolCall(ctx context.Context, req *mcp.CallToolRequest, tool *composite.CompositeTool) (*mcp.CallToolResult, error) {
	start := time.Now()

	var params map[string]any
	if err := json.Unmarshal(req.Params.Arguments, &params); err != nil {
		return nil, fmt.Errorf("parsing tool arguments: %w", err)
	}

	brokerAlias, _ := params["broker"].(string)
	if brokerAlias == "" {
		slog.Error("tool invoked",
			slog.String("tool", tool.Name),
			slog.String("status", "error"),
			slog.String("error_type", "missing_broker"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("broker parameter is required; available brokers: %s", strings.Join(r.pool.Aliases(), ", "))
	}
	delete(params, "broker")

	client, err := r.pool.GetSempV2(brokerAlias)
	if err != nil {
		slog.Error("tool invoked",
			slog.String("tool", tool.Name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "unknown_broker"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("unknown broker %q; available brokers: %s", brokerAlias, strings.Join(r.pool.Aliases(), ", "))
	}

	result, err := r.executor.Execute(ctx, *tool, client, params)
	if err != nil {
		logAttrs := []slog.Attr{
			slog.String("tool", tool.Name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "execution_error"),
			slog.Duration("duration", time.Since(start)),
		}
		var sempErr *sempv2.SEMPError
		if errors.As(err, &sempErr) {
			logAttrs = append(logAttrs,
				slog.Int("http_status", sempErr.StatusCode),
				slog.String("operation", sempErr.Operation))
		} else {
			// Non-SEMP errors (network, DNS, timeout) are safe to log —
			// they come from Go stdlib and don't contain credentials.
			logAttrs = append(logAttrs, slog.String("error", err.Error()))
		}
		slog.LogAttrs(ctx, slog.LevelError, "tool invoked", logAttrs...)
		return nil, fmt.Errorf("executing tool %q: %w", tool.Name, err)
	}

	resultJSON, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		slog.Error("tool invoked",
			slog.String("tool", tool.Name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "marshal_error"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("marshalling result for %q: %w", tool.Name, err)
	}

	slog.Info("tool invoked",
		slog.String("tool", tool.Name),
		slog.String("broker", brokerAlias),
		slog.String("status", "success"),
		slog.Duration("duration", time.Since(start)))

	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
	}, nil
}

// buildInputSchema creates a JSON Schema object from composite tool parameters
// and injects the broker parameter. The broker parameter is required, with a
// description that lists all available broker aliases from the pool.
func (r *Registry) buildInputSchema(params []composite.ParameterDef) map[string]any {
	properties := make(map[string]any, len(params)+1)
	var required []string

	// Inject broker parameter.
	aliases := r.pool.Aliases()
	properties["broker"] = map[string]any{
		"type":        "string",
		"description": fmt.Sprintf("Target broker alias (required). Available brokers: %s", strings.Join(aliases, ", ")),
	}
	required = append(required, "broker")

	for _, p := range params {
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

	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}
