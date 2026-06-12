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
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolManager validates parameters, resolves broker connections, routes tool
// calls to handlers, and produces structured MCP responses.
//
// All public methods are safe for concurrent use.
type ToolManager struct {
	pool     *semp.BrokerPool
	mu       sync.RWMutex
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
	name := handler.Metadata().Name
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.handlers[name]; exists {
		panic(fmt.Sprintf("duplicate tool registration: %q", name))
	}
	m.handlers[name] = handler
}

// Route looks up a handler by tool name. Returns an error if the tool is not
// registered.
func (m *ToolManager) Route(name string) (ToolHandler, error) {
	m.mu.RLock()
	handler, ok := m.handlers[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return handler, nil
}

// Handlers returns all registered tool handlers. The returned slice is
// unordered — callers should sort if deterministic ordering is needed.
func (m *ToolManager) Handlers() []ToolHandler {
	m.mu.RLock()
	handlers := make([]ToolHandler, 0, len(m.handlers))
	for _, h := range m.handlers {
		handlers = append(handlers, h)
	}
	m.mu.RUnlock()
	return handlers
}

// CallTool is the main entry point for tool execution. It routes to the
// handler, resolves the broker, validates parameters, executes the tool, and
// returns a structured MCP result with both StructuredContent and TextContent.
//
// The id parameter carries per-invocation audit identity (SOL-149606). Pass
// Identity{} (zero value, present=false) from non-HTTP call sites — tests,
// internal composite-tool steps — to mirror disabled-mode log shape.
func (m *ToolManager) CallTool(ctx context.Context, name string, params map[string]any, id Identity) (result *mcp.CallToolResult, err error) {
	start := time.Now()
	var brokerAlias, errorType string
	var toolErr error

	defer func() {
		// Panic detection: this defer runs during unwinding, before the
		// recover in withRecovery fires. Every error return below sets
		// toolErr and the success return sets result, so both still being
		// nil means the handler panicked — audit it as an error, never as a
		// success (SOL-150685). Invariant for new return paths: set toolErr,
		// or return a non-nil result.
		if toolErr == nil && result == nil {
			errorType = "panic"
			toolErr = panicError{}
		}
		logToolResult(ctx, name, &brokerAlias, start, &errorType, &toolErr, id)
	}()

	handler, err := m.Route(name)
	if err != nil {
		errorType = "unknown_tool"
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

	v1Client, err := m.pool.GetSEMPv1(brokerAlias)
	if err != nil {
		errorType, toolErr = m.classifyBrokerError(brokerAlias, err)
		return nil, toolErr
	}

	v2Client, err := m.pool.GetSEMPv2(brokerAlias)
	if err != nil {
		errorType, toolErr = m.classifyBrokerError(brokerAlias, err)
		return nil, toolErr
	}

	// Resolution succeeded — switch the locally-tracked alias to the display
	// (original-casing) form so all downstream log sites (destructive warn,
	// success/error tool-invoked logs) report the configured identifier
	// rather than whatever case the caller happened to type.
	if bc, ok := m.pool.BrokerConfig(brokerAlias); ok {
		brokerAlias = bc.DisplayName()
	}

	// Strip broker from params before validation and execution — handlers
	// should not see it.
	handlerParams := stripBrokerParam(params)

	// Read metadata once. Each call returns a fresh value with fresh maps,
	// so caching here keeps allocations down without aliasing risk.
	meta := handler.Metadata()

	// Validate parameters against the handler's input schema.
	if err := ValidateParams(handlerParams, meta.InputSchema); err != nil {
		errorType = "validation_error"
		toolErr = err
		return nil, toolErr
	}

	// Log warning for destructive tools.
	if meta.Annotations.Destructive != nil && *meta.Annotations.Destructive {
		slog.Warn("executing destructive operation",
			slog.String("tool", name),
			slog.String("broker", brokerAlias),
			slog.Any("", id))
	}

	// Execute.
	tc := &ToolContext{
		SEMPv1Client: v1Client,
		SEMPv2Client: v2Client,
	}
	toolResult, handleErr := handler.Handle(ctx, tc, handlerParams)
	if handleErr != nil {
		errorType = "execution_error"
		toolErr = fmt.Errorf("executing tool %q: %w", name, handleErr)
		return m.buildErrorResult(toolErr), nil
	}

	// Guard against nil results from handler.
	if toolResult == nil || toolResult.StructuredContent == nil {
		errorType = "nil_result"
		toolErr = fmt.Errorf("tool %q returned nil result", name)
		return nil, toolErr
	}

	// Validate output against schema.
	if err := ValidateOutput(toolResult.StructuredContent, meta.OutputSchema); err != nil {
		errorType = "output_validation_error"
		toolErr = fmt.Errorf("tool %q output validation: %w", name, err)
		return nil, toolErr
	}

	// Build MCP result with both StructuredContent and TextContent fallback.
	resultJSON, err := json.MarshalIndent(toolResult.StructuredContent, "", "  ")
	if err != nil {
		errorType = "marshal_error"
		toolErr = fmt.Errorf("marshalling result for %q: %w", name, err)
		return nil, toolErr
	}

	return &mcp.CallToolResult{
		StructuredContent: toolResult.StructuredContent,
		Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		IsError:           toolResult.IsError,
	}, nil
}

// panicError is the sentinel toolErr recorded when an invocation ends in a
// recovered panic. logToolResult logs unaudited errors type-only, so the
// audit line shows this type plus error_type=panic; the panic value itself
// is never logged (withRecovery logs its Go type and the stack separately).
type panicError struct{}

func (panicError) Error() string { return "tool handler panicked" }

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
//
// A free function rather than a ToolManager method so every tool emits the
// same audit line, including standalone tools registered outside the manager
// (list-brokers in register.go). The broker attr is omitted when *broker is
// empty, which also covers brokerless tools.
//
// The id argument carries per-invocation audit identity (SOL-149606). It is
// passed through slog.Any so Identity.LogValue is invoked once at emit time
// — in disabled mode the LogValuer returns an empty group, so the JSON
// handler emits no identity key at all (byte-identical to pre-SOL-149606
// log lines).
func logToolResult(ctx context.Context, tool string, broker *string, start time.Time, errorType *string, toolErr *error, id Identity) {
	if *toolErr == nil {
		attrs := make([]slog.Attr, 0, 5)
		attrs = append(attrs, slog.String("tool", tool))
		// Omit the broker attr when empty (brokerless tools like
		// list-brokers), mirroring the error branch below. CallTool
		// successes always carry a resolved broker, keeping the existing
		// line shape byte-identical.
		if *broker != "" {
			attrs = append(attrs, slog.String("broker", *broker))
		}
		attrs = append(attrs,
			slog.String("status", "success"),
			slog.Duration("duration", time.Since(start)),
			slog.Any("", id))
		slog.LogAttrs(ctx, slog.LevelInfo, "tool invoked", attrs...)
		return
	}

	// Match the concrete error type once, up front: it gates both the structured
	// fields below and what we put in "detail".
	//
	// A common SempError interface was considered but rejected: see drift
	// D7 in docs/semp/sempv1-client-design.md. The two protocols' error
	// shapes are semantically different (HTTPStatus means different things
	// in v1's envelope-error case vs. v2's HTTP-error case), and forcing a
	// unified contract would mislead consumers. Both this logging branch
	// and buildErrorResult() use the same type-switch pattern — the
	// duplication is small and keeps each consumer's field extraction
	// independent.
	var sempv1Err *sempv1.Error
	var sempv2Err *sempv2.SEMPError
	var retriesErr *resilience.RetriesExhaustedError
	isV1 := errors.As(*toolErr, &sempv1Err)
	isV2 := errors.As(*toolErr, &sempv2Err)
	isRetries := errors.As(*toolErr, &retriesErr)

	// "detail" carries the unsanitized error so operators can diagnose failures —
	// it is the one place the raw text is kept (the agent only ever sees the
	// sanitized message from buildErrorResult). Folding it onto this line, rather
	// than a separate emit, keeps one tool error in one record at a single (ERROR)
	// level.
	//
	// We log the raw err.Error() ONLY for the broker error types we've audited:
	// their text is broker-generated and carries no credentials (auth is applied
	// via headers, not URLs). For any other error — including the unknown/default
	// case that buildErrorResult deliberately hides behind genericInternalMessage
	// — we can't vouch for the contents, so we log only the Go type, never the
	// message. ReplaceAttr can't help here (it keys off field names, and "detail"
	// is a raw string), so this type gate is the actual safeguard.
	detail := fmt.Sprintf("%T", *toolErr)
	if isV1 || isV2 || isRetries {
		detail = (*toolErr).Error()
	}

	attrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("status", "error"),
		slog.String("error_type", *errorType),
		slog.Duration("duration", time.Since(start)),
		slog.String("detail", detail),
		slog.Any("", id),
	}
	if *broker != "" {
		attrs = append(attrs, slog.String("broker", *broker))
	}

	switch {
	case isV1:
		attrs = append(attrs,
			slog.String("kind", sempv1Err.Kind.String()),
			slog.Int("http_status", sempv1Err.StatusCode),
			slog.Int("reason_code", sempv1Err.ReasonCode))
	case isV2:
		attrs = append(attrs,
			slog.Int("http_status", sempv2Err.StatusCode),
			slog.Int("semp_code", sempv2Err.SEMPCode),
			slog.String("operation", sempv2Err.Operation))
	}

	slog.LogAttrs(ctx, slog.LevelError, "tool invoked", attrs...)
}

// classifyBrokerError translates a BrokerPool resolution failure into the
// errorType label and user-facing error the manager logs and returns.
//
// It distinguishes two cases:
//
//   - The alias isn't configured (semp.ErrUnknownBroker): produces an
//     "unknown_broker" label and a message listing the available aliases
//     so an operator can spot a typo.
//   - Anything else (transport setup, future OAuth handshake, etc.):
//     produces a "broker_init_error" label and wraps the original error
//     so the underlying cause survives in logs and through errors.Is/As.
//
// The dispatch keeps the manager honest as the pool's failure modes grow —
// today only the unknown-alias case is reachable, but Story 5 (rate limit /
// retry decorators) and future OAuth token-exchange will introduce real
// init failures that should not be reported as "unknown broker".
func (m *ToolManager) classifyBrokerError(alias string, err error) (string, error) {
	if errors.Is(err, semp.ErrUnknownBroker) {
		return "unknown_broker", fmt.Errorf("unknown broker %q; available brokers: %s",
			alias, strings.Join(m.pool.Aliases(), ", "))
	}
	return "broker_init_error", fmt.Errorf("connecting to broker %q: %w", alias, err)
}
