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
func (m *ToolManager) CallTool(ctx context.Context, name string, params map[string]any) (*mcp.CallToolResult, error) {
	start := time.Now()
	var brokerAlias, errorType string
	var toolErr error

	defer m.logToolResult(ctx, name, &brokerAlias, start, &errorType, &toolErr)

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
			slog.String("broker", brokerAlias))
	}

	// Execute.
	tc := &ToolContext{
		SEMPv1Client: v1Client,
		SEMPv2Client: v2Client,
	}
	result, err := handler.Handle(ctx, tc, handlerParams)
	if err != nil {
		errorType = "execution_error"
		toolErr = fmt.Errorf("executing tool %q: %w", name, err)
		return buildErrorResult(toolErr), nil
	}

	// Guard against nil results from handler.
	if result == nil || result.StructuredContent == nil {
		errorType = "nil_result"
		toolErr = fmt.Errorf("tool %q returned nil result", name)
		return nil, toolErr
	}

	// Validate output against schema.
	if err := ValidateOutput(result.StructuredContent, meta.OutputSchema); err != nil {
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

	// Type-switch on the concrete error types instead of a shared interface.
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
	switch {
	case errors.As(*toolErr, &sempv1Err):
		attrs = append(attrs,
			slog.String("kind", sempv1Err.Kind.String()),
			slog.Int("http_status", sempv1Err.StatusCode),
			slog.Int("reason_code", sempv1Err.ReasonCode))
	case errors.As(*toolErr, &sempv2Err):
		attrs = append(attrs,
			slog.Int("http_status", sempv2Err.StatusCode),
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

// buildErrorResult converts a tool execution error into an MCP-compliant
// CallToolResult with IsError: true. Per the MCP spec, tool execution errors
// should be returned as results (not protocol-level errors) so the LLM can see
// them and self-correct. StructuredContent carries machine-readable fields
// (retryable, status, protocol-specific data); Content carries a human-readable
// text message.
func buildErrorResult(err error) *mcp.CallToolResult {
	msg := buildErrorMessage(err)
	retryable := isRetryable(err)

	structured := map[string]any{
		"error":     msg,
		"retryable": retryable,
	}

	var sempv2Err *sempv2.SEMPError
	var sempv1Err *sempv1.Error
	var retriesErr *resilience.RetriesExhaustedError

	switch {
	case errors.As(err, &sempv2Err):
		structured["status"] = sempv2Err.StatusCode
		structured["operation"] = sempv2Err.Operation
		if sempv2Err.SEMPStatus != "" {
			structured["sempStatus"] = sempv2Err.SEMPStatus
		}
		if sempv2Err.SEMPCode != 0 {
			structured["sempCode"] = sempv2Err.SEMPCode
		}
	case errors.As(err, &sempv1Err):
		structured["status"] = sempv1Err.StatusCode
		structured["kind"] = sempv1Err.Kind.String()
		if sempv1Err.ReasonCode != 0 {
			structured["reasonCode"] = sempv1Err.ReasonCode
		}
	case errors.As(err, &retriesErr):
		if retriesErr.StatusCode != 0 {
			structured["status"] = retriesErr.StatusCode
		}
		structured["attempts"] = retriesErr.Attempts
	}

	return &mcp.CallToolResult{
		StructuredContent: structured,
		Content:           []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError:           true,
	}
}

// isRetryable returns true for errors that represent transient conditions where
// the same request might succeed later. Only RetriesExhaustedError qualifies —
// the resilience layer's checkRetry already filters out non-retryable statuses,
// so anything that exhausts retries was genuinely transient (429, 503, 5xx,
// connection error). All SEMPError (4xx) and sempv1.Error (envelope errors) are
// deterministic and non-retryable.
func isRetryable(err error) bool {
	var retriesErr *resilience.RetriesExhaustedError
	return errors.As(err, &retriesErr)
}

// buildErrorMessage produces a human-readable error string for the Content text
// block. It prefers the broker's own description when available.
func buildErrorMessage(err error) string {
	var retriesErr *resilience.RetriesExhaustedError
	var sempv2Err *sempv2.SEMPError
	var sempv1Err *sempv1.Error

	switch {
	case errors.As(err, &retriesErr):
		if retriesErr.Err != nil {
			return fmt.Sprintf(
				"Request failed after %d attempts due to network error: %v. Internal retries exhausted.",
				retriesErr.Attempts, retriesErr.Err)
		}
		return fmt.Sprintf(
			"Request failed after %d attempts (HTTP %d). Internal retries exhausted.",
			retriesErr.Attempts, retriesErr.StatusCode)

	case errors.As(err, &sempv2Err):
		if sempv2Err.Description != "" {
			return sempv2Err.Description
		}
		return fmt.Sprintf("%s returned HTTP %d", sempv2Err.Operation, sempv2Err.StatusCode)

	case errors.As(err, &sempv1Err):
		if sempv1Err.Message != "" {
			msg := fmt.Sprintf("SEMPv1 %s error: %s", sempv1Err.Kind, sempv1Err.Message)
			if sempv1Err.Kind == sempv1.ErrorKindLimit {
				msg += ". Reduce the scope of the request."
			}
			return msg
		}
		if sempv1Err.Kind == sempv1.ErrorKindHTTP {
			return fmt.Sprintf("SEMPv1 HTTP %d error", sempv1Err.StatusCode)
		}
		return fmt.Sprintf("SEMPv1 %s error (status=%d)", sempv1Err.Kind, sempv1Err.StatusCode)

	default:
		return err.Error()
	}
}
