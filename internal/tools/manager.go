package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

	handler, err := m.Route(name)
	if err != nil {
		return nil, err
	}

	// Extract and resolve broker.
	brokerAlias, _ := params["broker"].(string)
	if brokerAlias == "" {
		slog.Error("tool invoked",
			slog.String("tool", name),
			slog.String("status", "error"),
			slog.String("error_type", "missing_broker"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("broker parameter is required; available brokers: %s",
			strings.Join(m.pool.Aliases(), ", "))
	}

	client, err := m.pool.GetSempV2(brokerAlias)
	if err != nil {
		slog.Error("tool invoked",
			slog.String("tool", name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "unknown_broker"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("unknown broker %q; available brokers: %s",
			brokerAlias, strings.Join(m.pool.Aliases(), ", "))
	}

	// Strip broker from params before validation and execution — handlers
	// should not see it.
	handlerParams := make(map[string]any, len(params))
	for k, v := range params {
		if k != "broker" {
			handlerParams[k] = v
		}
	}

	// Validate parameters against the handler's input schema.
	if err := ValidateParams(handlerParams, handler.Schema()); err != nil {
		slog.Error("tool invoked",
			slog.String("tool", name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "validation_error"),
			slog.Duration("duration", time.Since(start)))
		return nil, err
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
		m.logError(ctx, name, brokerAlias, start, err)
		return nil, fmt.Errorf("executing tool %q: %w", name, err)
	}

	// Validate output against schema.
	if err := ValidateOutput(result.StructuredContent, handler.OutputSchema()); err != nil {
		slog.Error("tool invoked",
			slog.String("tool", name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "output_validation_error"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("tool %q output validation: %w", name, err)
	}

	// Build MCP result with both StructuredContent and TextContent fallback.
	resultJSON, err := json.MarshalIndent(result.StructuredContent, "", "  ")
	if err != nil {
		slog.Error("tool invoked",
			slog.String("tool", name),
			slog.String("broker", brokerAlias),
			slog.String("status", "error"),
			slog.String("error_type", "marshal_error"),
			slog.Duration("duration", time.Since(start)))
		return nil, fmt.Errorf("marshalling result for %q: %w", name, err)
	}

	slog.Info("tool invoked",
		slog.String("tool", name),
		slog.String("broker", brokerAlias),
		slog.String("status", "success"),
		slog.Duration("duration", time.Since(start)))

	return &mcp.CallToolResult{
		StructuredContent: result.StructuredContent,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		IsError:           result.IsError,
	}, nil
}

// logError logs tool execution failures with structured fields. SEMP errors get
// operation and status code context. Non-SEMP errors (network, DNS, timeout)
// are safe to log as they come from Go stdlib and don't contain credentials.
func (m *ToolManager) logError(ctx context.Context, tool, broker string, start time.Time, err error) {
	logAttrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("broker", broker),
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
		logAttrs = append(logAttrs, slog.String("error", err.Error()))
	}

	slog.LogAttrs(ctx, slog.LevelError, "tool invoked", logAttrs...)
}
