package tools

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

// ToolManager validates parameters, resolves broker connections, routes tool
// calls to handlers, and produces structured MCP responses.
type ToolManager struct {
	pool     *semp.BrokerPool
	handlers map[string]ToolHandler
}

// NewToolManager creates a ToolManager that resolves broker clients from the
// given pool. Tools are registered via Register() before use.
func NewToolManager(pool *semp.BrokerPool) *ToolManager {
	return &ToolManager{
		pool:     pool,
		handlers: make(map[string]ToolHandler),
	}
}

// NewToolManagerFromComposite creates a ToolManager and registers a
// CompositeToolHandler for each composite tool definition. This is the
// standard factory for YAML-driven tools.
func NewToolManagerFromComposite(pool *semp.BrokerPool, tools []composite.CompositeTool, executor *composite.CompositeExecutor) *ToolManager {
	mgr := NewToolManager(pool)
	for i := range tools {
		mgr.Register(NewCompositeToolHandler(tools[i], executor))
	}
	return mgr
}

// Register adds a tool handler to the manager. Panics if a handler with the
// same name is already registered — duplicate tool names indicate a
// configuration error that should be caught at startup.
func (m *ToolManager) Register(handler ToolHandler) {
	name := handler.Name()
	if _, exists := m.handlers[name]; exists {
		panic(fmt.Sprintf("duplicate tool registration: %q", name))
	}
	m.handlers[name] = handler
}

// Route looks up a handler by tool name. Returns an error if the tool is not
// registered.
func (m *ToolManager) Route(name string) (ToolHandler, error) {
	handler, ok := m.handlers[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return handler, nil
}

// Handlers returns all registered tool handlers. The returned slice is
// unordered — callers should sort if deterministic ordering is needed.
func (m *ToolManager) Handlers() []ToolHandler {
	handlers := make([]ToolHandler, 0, len(m.handlers))
	for _, h := range m.handlers {
		handlers = append(handlers, h)
	}
	return handlers
}

// CallTool is the main entry point for tool execution. It routes to the
// handler, resolves the broker, validates parameters, executes the tool, and
// returns a structured MCP result with both StructuredContent and TextContent.
func (m *ToolManager) CallTool(ctx context.Context, name string, params map[string]any) (*mcp.CallToolResult, error) {
	start := time.Now()
	var brokerAlias, errorType string
	var toolErr error

	defer m.logToolResult(ctx, name, &brokerAlias, start, &errorType, &toolErr)

	handler, err := m.Route(name)
	if err != nil {
		toolErr = err
		return nil, err
	}

	// Extract and resolve broker.
	brokerAlias, _ = params["broker"].(string)
	if brokerAlias == "" {
		errorType = "missing_broker"
		toolErr = fmt.Errorf("broker parameter is required; available brokers: %s",
			strings.Join(m.pool.Aliases(), ", "))
		return nil, toolErr
	}

	client, err := m.pool.GetSempV2(brokerAlias)
	if err != nil {
		errorType = "unknown_broker"
		toolErr = fmt.Errorf("unknown broker %q; available brokers: %s",
			brokerAlias, strings.Join(m.pool.Aliases(), ", "))
		return nil, toolErr
	}

	// Strip broker from params before validation and execution — handlers
	// should not see it.
	handlerParams := stripBrokerParam(params)

	// Validate parameters against the handler's input schema.
	if err := ValidateParams(handlerParams, handler.Schema()); err != nil {
		errorType = "validation_error"
		toolErr = err
		return nil, toolErr
	}

	// Log warning for destructive tools.
	if ann := handler.Annotations(); ann != nil && ann.DestructiveHint != nil && *ann.DestructiveHint {
		slog.Warn("executing destructive operation",
			slog.String("tool", name),
			slog.String("broker", brokerAlias))
	}

	// Execute.
	tc := &ToolContext{SEMPClient: client}
	result, err := handler.Handle(ctx, tc, handlerParams)
	if err != nil {
		errorType = "execution_error"
		toolErr = fmt.Errorf("executing tool %q: %w", name, err)
		return nil, toolErr
	}

	// Guard against nil results from handler.
	if result == nil || result.StructuredContent == nil {
		errorType = "nil_result"
		toolErr = fmt.Errorf("tool %q returned nil result", name)
		return nil, toolErr
	}

	// Validate output against schema.
	if err := ValidateOutput(result.StructuredContent, handler.OutputSchema()); err != nil {
		errorType = "output_validation_error"
		toolErr = fmt.Errorf("tool %q output validation: %w", name, err)
		return nil, toolErr
	}

	// Build MCP result with both StructuredContent and TextContent fallback.
	resultJSON, err := json.MarshalIndent(result.StructuredContent, "", "  ")
	if err != nil {
		errorType = "marshal_error"
		toolErr = fmt.Errorf("marshalling result for %q: %w", name, err)
		return nil, toolErr
	}

	return &mcp.CallToolResult{
		StructuredContent: result.StructuredContent,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		IsError:           result.IsError,
	}, nil
}

// stripBrokerParam returns a copy of params without the broker key.
func stripBrokerParam(params map[string]any) map[string]any {
	handlerParams := make(map[string]any, len(params))
	for k, v := range params {
		if k != "broker" {
			handlerParams[k] = v
		}
	}
	return handlerParams
}

// logToolResult is called via defer to log every tool invocation. On success
// (toolErr is nil) it logs at INFO. On failure it logs at ERROR with the
// error type and, for SEMP errors, the HTTP status and operation.
func (m *ToolManager) logToolResult(ctx context.Context, tool string, broker *string, start time.Time, errorType *string, toolErr *error) {
	if *toolErr == nil {
		slog.Info("tool invoked",
			slog.String("tool", tool),
			slog.String("broker", *broker),
			slog.String("status", "success"),
			slog.Duration("duration", time.Since(start)))
		return
	}

	attrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("status", "error"),
		slog.String("error_type", *errorType),
		slog.Duration("duration", time.Since(start)),
	}
	if *broker != "" {
		attrs = append(attrs, slog.String("broker", *broker))
	}

	var sempErr *sempv2.SEMPError
	if errors.As(*toolErr, &sempErr) {
		attrs = append(attrs,
			slog.Int("http_status", sempErr.StatusCode),
			slog.String("operation", sempErr.Operation))
	}

	slog.LogAttrs(ctx, slog.LevelError, "tool invoked", attrs...)
}
