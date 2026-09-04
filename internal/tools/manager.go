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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xeipuuv/gojsonschema"
)

// ToolManager validates parameters, resolves broker connections, routes tool
// calls to handlers, and produces structured MCP responses.
//
// All public methods are safe for concurrent use.
type ToolManager struct {
	pool    *semp.BrokerPool
	mu      sync.RWMutex
	tools   map[string]*registeredTool
	metrics *metrics.ToolMetrics // nil when metrics are disabled; all uses are nil-safe
	// auditLog mirrors audit.Enabled(cfg.Observability) at construction. When
	// false the manager behaves exactly as it did before SOL-152096: the
	// destructive-operation WARN, and no audit record.
	auditLog bool
}

// ManagerOption configures a ToolManager at construction. Variadic so the
// existing constructor calls — including every test that builds a manager —
// keep working unchanged.
type ManagerOption func(*ToolManager)

// WithToolMetrics wires the per-tool RED metrics recorder (SOL-152086, SOL-152091)
// into the manager. Pass nil, or omit this option, when metrics are disabled — all
// uses of the recorder are nil-safe.
func WithToolMetrics(tm *metrics.ToolMetrics) ManagerOption {
	return func(m *ToolManager) { m.metrics = tm }
}

// WithAuditLog turns audit-record emission on for destructive tool calls.
// Pass audit.Enabled(cfg.Observability); the capability is off by default
// (door-closing policy), and off means inert, not degraded.
func WithAuditLog(enabled bool) ManagerOption {
	return func(m *ToolManager) { m.auditLog = enabled }
}

// registeredTool is what Register() stores for one tool: the handler plus
// its precompiled input/output JSON Schema validators and annotations, all
// built once from handler.Metadata() at Register() time. Keeping these
// together means CallTool's hot path is a single map lookup under a single
// lock, rather than one lookup for the handler (Route) and a second for the
// cached validators — and it makes "a handler is registered without its
// validators" structurally impossible, since both are always written by the
// same Register() call.
//
// Caching the compiled validators/annotations here — instead of calling
// handler.Metadata() again on every CallTool invocation — matters for two
// reasons: it avoids recompiling the same JSON Schema on every call
// (SOL-153334), and for a composite write tool it avoids rebuilding the
// output schema from the full SEMPv2 operation catalog on every call
// (SOL-153335) — Metadata() computes that unconditionally as part of the
// struct it returns, so calling it again would pay that cost too even if
// CallTool only wanted the annotations.
type registeredTool struct {
	handler     ToolHandler
	input       *gojsonschema.Schema
	output      *gojsonschema.Schema
	annotations Annotations
}

// NewToolManager creates a ToolManager that resolves broker clients from the
// given pool. Tools are registered via Register() before use.
func NewToolManager(pool *semp.BrokerPool, opts ...ManagerOption) *ToolManager {
	mgr := &ToolManager{
		pool:  pool,
		tools: make(map[string]*registeredTool),
	}
	for _, opt := range opts {
		// A nil ManagerOption panics if called directly, unlike a nil
		// *metrics.ToolMetrics or a false bool — so treat it as a no-op
		// rather than letting every caller guard against it. This is what
		// let `NewToolManagerFromComposite(pool, tools, executor, nil)`
		// panic here at construction time when the parameter grew from one
		// concrete pointer to variadic options: the old call sites' `nil`
		// meant "no metrics" and now means "no option", which should do
		// nothing, not crash.
		if opt != nil {
			opt(mgr)
		}
	}
	return mgr
}

// NewToolManagerFromComposite creates a ToolManager and registers a
// CompositeToolHandler for each composite tool definition. This is the
// standard factory for YAML-driven tools.
func NewToolManagerFromComposite(pool *semp.BrokerPool, tools []composite.CompositeTool, executor *composite.CompositeExecutor, opts ...ManagerOption) *ToolManager {
	mgr := NewToolManager(pool, opts...)
	for i := range tools {
		mgr.Register(NewCompositeToolHandler(tools[i], executor))
	}
	return mgr
}

// Register adds a tool handler to the manager, and compiles its input/output
// schema validators once (see registeredTool). Panics if a handler with the
// same name is already registered, or if either schema fails to compile —
// both are configuration errors that should be caught at startup, not on a
// caller's first tool call. The duplicate-name check runs before schema
// compilation, under the same lock, so a duplicate registration fails fast
// with the specific "duplicate tool registration" panic rather than
// potentially compiling a schema first and panicking on that instead.
func (m *ToolManager) Register(handler ToolHandler) {
	meta := handler.Metadata()
	name := meta.Name

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tools[name]; exists {
		panic(fmt.Sprintf("duplicate tool registration: %q", name))
	}

	inputSchema, err := compileSchema(meta.InputSchema)
	if err != nil {
		panic(fmt.Sprintf("tool %q: compiling input schema: %v", name, err))
	}
	outputSchema, err := compileSchema(meta.OutputSchema)
	if err != nil {
		panic(fmt.Sprintf("tool %q: compiling output schema: %v", name, err))
	}

	m.tools[name] = &registeredTool{
		handler:     handler,
		input:       inputSchema,
		output:      outputSchema,
		annotations: meta.Annotations,
	}
}

// lookup returns the registeredTool for name in a single lock/unlock, so a
// caller that needs both the handler and its cached validators (CallTool)
// does one map access instead of two. Returns an error if the tool is not
// registered.
func (m *ToolManager) lookup(name string) (*registeredTool, error) {
	m.mu.RLock()
	rt, ok := m.tools[name]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return rt, nil
}

// Route looks up a handler by tool name. Returns an error if the tool is not
// registered.
func (m *ToolManager) Route(name string) (ToolHandler, error) {
	rt, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	return rt.handler, nil
}

// Handlers returns all registered tool handlers. The returned slice is
// unordered — callers should sort if deterministic ordering is needed.
func (m *ToolManager) Handlers() []ToolHandler {
	m.mu.RLock()
	handlers := make([]ToolHandler, 0, len(m.tools))
	for _, rt := range m.tools {
		handlers = append(handlers, rt.handler)
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
	var brokerAlias string
	var errorType metrics.ErrorType
	var toolErr error
	// auditArgsHash is set only for a destructive call with the audit log on,
	// and only once the arguments have been validated. Its presence is what
	// tells the defer to emit the operation record, so a call that never
	// reached the destructive gate emits none.
	var auditArgsHash string

	defer func() {
		// Panic detection: this defer runs during unwinding, before the
		// recover in withRecovery fires. Every error return below sets
		// toolErr and the success return sets result, so both still being
		// nil means the handler panicked — audit it as an error, never as a
		// success (SOL-150685). Invariant for new return paths: set toolErr,
		// or return a non-nil result.
		if toolErr == nil && result == nil {
			errorType = metrics.ErrorTypePanic
			toolErr = panicError{}
		}

		// The log line uses the raw alias for diagnostics; the metric label is
		// canonicalized to a bounded set.
		logToolResult(ctx, name, &brokerAlias, start, &errorType, &toolErr, id)
		recordToolInvocation(ctx, m.metrics, name, canonicalBrokerLabel(m.pool, brokerAlias), start, errorType, toolErr)

		// One operation record per destructive call, emitted here so it
		// carries the outcome — including a recovered panic, which reaches
		// this defer the same way the log line above does. Emitted after the
		// operational line, and from the same place, so the two cannot name
		// different callers or disagree about how the call ended.
		if auditArgsHash != "" {
			// audit.Event.ErrorType is a plain string (its own closed
			// vocabulary, checked against errorTypeVocabulary in
			// internal/observability/audit); metrics.ErrorType is the
			// analogous typed vocabulary for the metrics label. Both are
			// spelled the same way for every value this package emits, but
			// they are two independent types, so the value crosses the
			// package boundary as a string rather than assuming one
			// vocabulary is a subtype of the other.
			emitOperationAudit(ctx, name, brokerAlias, auditArgsHash, start, string(errorType), toolErr)
		}
	}()

	// Deliberately still a protocol-level error, not a buildLocalErrorResult
	// result (SOL-152980): the MCP spec puts an unknown tool name in the
	// protocol-error bucket, not tool-execution-error, and the go-sdk server
	// already rejects an unknown name before this method is ever reached in
	// production (RegisterWithServer's closures only route names taken from
	// Handlers()). Kept as a defensive branch for direct/test callers of
	// CallTool with a name that was never registered.
	//
	// One lookup here gets both the handler and its precompiled
	// validators/annotations (built once at Register() time) — see
	// registeredTool — rather than calling Route() and then a second,
	// separate map lookup for the validators.
	rt, err := m.lookup(name)
	if err != nil {
		errorType = metrics.ErrorTypeUnknownTool
		toolErr = err
		return nil, err
	}
	handler := rt.handler

	// Extract and resolve broker.
	brokerAlias, _ = params["broker"].(string)
	if brokerAlias == "" {
		errorType = metrics.ErrorTypeMissingBroker
		toolErr = fmt.Errorf("broker parameter is required; available brokers: %s",
			strings.Join(m.pool.Aliases(), ", "))
		return buildLocalErrorResult(toolErr), nil
	}

	v1Client, err := m.pool.GetSEMPv1(brokerAlias)
	if err != nil {
		errorType, toolErr = m.classifyBrokerError(brokerAlias, err)
		return m.buildBrokerResolutionErrorResult(string(errorType), toolErr, brokerAlias), nil
	}

	v2Client, err := m.pool.GetSEMPv2(brokerAlias)
	if err != nil {
		errorType, toolErr = m.classifyBrokerError(brokerAlias, err)
		return m.buildBrokerResolutionErrorResult(string(errorType), toolErr, brokerAlias), nil
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

	// Validate parameters against the handler's input schema.
	if _, err := validateAgainstCompiledSchema(handlerParams, rt.input, "parameter validation failed"); err != nil {
		errorType = metrics.ErrorTypeValidationError
		toolErr = err
		return buildLocalErrorResult(toolErr), nil
	}

	// Destructive tools are the audit surface (SOL-152096). This is the same
	// gate the pre-flip WARN stood at — after broker resolution and input
	// validation — so enabling the audit log changes the FORM of the signal,
	// not which calls produce one. Arguments are hashed here, while they are
	// still the validated map the handler is about to receive; the record
	// itself is emitted from the defer, once the outcome is known.
	if rt.annotations.Destructive != nil && *rt.annotations.Destructive {
		if m.auditLog {
			auditArgsHash = hashArgsForAudit(ctx, name, brokerAlias, params)
		} else {
			// Pre-flip behaviour, byte-identical, so the capability is inert
			// when its flag is off.
			slog.Warn("executing destructive operation",
				slog.String("tool", name),
				slog.String("broker", brokerAlias),
				slog.Any("", id))
		}
	}

	// Execute.
	tc := &ToolContext{
		SEMPv1Client: v1Client,
		SEMPv2Client: v2Client,
	}
	toolResult, handleErr := handler.Handle(ctx, tc, handlerParams)
	if handleErr != nil {
		errorType = metrics.ErrorTypeExecutionError
		toolErr = fmt.Errorf("executing tool %q: %w", name, handleErr)
		return m.buildErrorResult(toolErr, brokerAlias), nil
	}

	// Guard against nil results from handler. This and the two checks below
	// fire after handler.Handle has already run — for a destructive tool, any
	// broker-side mutation may have already taken effect — so each returns a
	// structured result (not a bare error) precisely so the agent gets a
	// retryable=false signal instead of a protocol error it might mistake for
	// "nothing happened, safe to retry" (SOL-152980).
	if toolResult == nil || toolResult.StructuredContent == nil {
		errorType = metrics.ErrorTypeNilResult
		toolErr = fmt.Errorf("tool %q returned nil result", name)
		return buildLocalErrorResult(toolErr), nil
	}

	// Validate output against schema. This also gives us the compact JSON
	// encoding of StructuredContent, which we reuse below instead of
	// marshalling it a second time.
	resultJSON, err := validateAgainstCompiledSchema(toolResult.StructuredContent, rt.output, "output validation failed")
	if err != nil {
		errorType = metrics.ErrorTypeOutputValidationError
		toolErr = fmt.Errorf("tool %q output validation: %w", name, err)
		return buildLocalErrorResult(toolErr), nil
	}

	// Re-indent those same bytes for the TextContent fallback — byte-for-byte
	// what json.MarshalIndent(toolResult.StructuredContent, "", "  ") would
	// have produced (MarshalIndent is itself Marshal followed by Indent), but
	// without a second reflection-based marshal of the same value.
	var indented bytes.Buffer
	if err := json.Indent(&indented, resultJSON, "", "  "); err != nil {
		errorType = metrics.ErrorTypeMarshalError
		toolErr = fmt.Errorf("marshalling result for %q: %w", name, err)
		return buildLocalErrorResult(toolErr), nil
	}

	return &mcp.CallToolResult{
		StructuredContent: toolResult.StructuredContent,
		Content:           []mcp.Content{&mcp.TextContent{Text: indented.String()}},
		IsError:           toolResult.IsError,
	}, nil
}

// hashArgsForAudit computes the arguments_hash for a destructive call, or
// returns "" after recording a drop when the arguments cannot be canonicalized.
// An empty return tells CallTool's defer to emit no operation record: a record
// whose arguments_hash stands for nothing is worse than a visible gap.
//
// Extracted from CallTool because the failure branch is unreachable from the
// wire — arguments always arrive from json.Unmarshal, and a value that could
// defeat json.Marshal is rejected by input-schema validation first — so a test
// can only reach it here. Defensive code nothing can exercise is
// indistinguishable from code that does not work.
func hashArgsForAudit(ctx context.Context, tool, broker string, args map[string]any) string {
	hash, err := audit.HashArgs(args)
	if err == nil {
		return hash
	}
	// The Go type only, never the message: the wrapped encoding/json error
	// renders the offending value, and that value is a tool argument — the one
	// thing an audit record must never carry
	// (docs/internal/secure-logging-rules.md).
	slog.ErrorContext(ctx, "audit: could not hash tool arguments; recording a drop instead of an operation record",
		slog.String("tool", tool),
		slog.String("broker", broker),
		slog.String("detail", fmt.Sprintf("%T", err)))
	audit.EmitDrop(ctx)
	return ""
}

// emitOperationAudit builds and emits the single operation audit record for
// one destructive tool call (SOL-152096).
//
// "Exactly one audit event per destructive tool call" bounds THIS record type,
// not the total number of audit records the call can produce: a hop-2 broker
// denial (SOL-153332) adds a broker_authz_denied record that coexists with
// this one, because execution had already started when the broker refused. The
// invariant here is one operation record per call, with no started/completed
// pair to join.
//
// Identity is not passed in. audit.NewEvent reads the principal from ctx, the
// same canonical source logToolResult's Identity was projected from
// (SOL-152087), so the two records name one caller by construction rather than
// by two call sites agreeing.
func emitOperationAudit(ctx context.Context, tool, broker, argsHash string, start time.Time, errorType string, toolErr error) {
	fields := audit.Fields{
		Type:          audit.EventOperation,
		Outcome:       audit.OutcomeSuccess,
		Tool:          tool,
		Broker:        broker,
		ArgumentsHash: argsHash,
		StartedAt:     start,
		Duration:      time.Since(start),
	}
	if toolErr != nil {
		fields.Outcome = audit.OutcomeError
		fields.ErrorType = errorType
		// A recovered panic is outcome=error with error_type=panic; there is
		// no panic outcome. The extra flag is what lets a reviewer separate a
		// crash from a handled failure without parsing the message.
		fields.PanicRecovered = errorType == "panic"
	}

	event, err := audit.NewEvent(ctx, fields)
	if err != nil {
		// The constructor refused the record, so there is nothing valid to
		// write: say the record is missing rather than let its absence read as
		// "no destructive call happened". err.Error() is safe to log — it is
		// written by the audit package and names only field names and
		// closed-vocabulary values, never argument data.
		slog.ErrorContext(ctx, "audit: operation record rejected by the schema constructor; recording a drop",
			slog.String("tool", tool),
			slog.String("broker", broker),
			slog.String("detail", err.Error()))
		audit.EmitDrop(ctx)
		return
	}
	audit.Emit(ctx, event)
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
// (list-brokers in register.go). Brokerless tools log broker=none, matching the
// metric label.
//
// The id argument carries per-invocation audit identity (SOL-149606). It is
// passed through slog.Any so Identity.LogValue is invoked once at emit time
// — in disabled mode the LogValuer returns an empty group, so the JSON
// handler emits no identity key at all (byte-identical to pre-SOL-149606
// log lines).
func logToolResult(ctx context.Context, tool string, broker *string, start time.Time, errorType *metrics.ErrorType, toolErr *error, id Identity) {
	dur := time.Since(start)

	// Brokerless tools and pre-resolution failures carry no alias; the field
	// shows "none" so the log line matches the metric label rather than
	// omitting the field.
	brokerLabel := *broker
	if brokerLabel == "" {
		brokerLabel = brokerLabelNone
	}

	if *toolErr == nil {
		attrs := make([]slog.Attr, 0, 5)
		attrs = append(attrs, slog.String("tool", tool))
		attrs = append(attrs, slog.String("broker", brokerLabel))
		attrs = append(attrs,
			slog.String("outcome", "success"),
			slog.Duration("duration", dur),
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
	var busyErr *resilience.BrokerBusyError
	var exchErr *tokenexchange.ExchangeError
	isV1 := errors.As(*toolErr, &sempv1Err)
	isV2 := errors.As(*toolErr, &sempv2Err)
	isRetries := errors.As(*toolErr, &retriesErr)
	// BrokerBusyError clears the audit gate below by construction: it is built
	// entirely from server-side values (the configured bound, the measured wait,
	// a fixed stage constant) and never touches a broker, so its Error() cannot
	// carry broker, intermediary, or caller text.
	isBusy := errors.As(*toolErr, &busyErr)
	isExchange := errors.As(*toolErr, &exchErr)

	// "detail" carries the unsanitized error so operators can diagnose failures —
	// it is the one place the raw text is kept. For a broker/handler-originated
	// error the agent only ever sees the sanitized message from buildErrorResult;
	// for a local error the agent sees this same err.Error() text verbatim via
	// buildLocalErrorResult, which is safe precisely because that text is
	// something this package wrote itself, never broker- or handler-originated
	// (SOL-152980). Folding "detail" onto this line, rather than a separate
	// emit, keeps one tool error in one record at a single (ERROR) level.
	//
	// We log the raw err.Error() ONLY for the broker error types we've audited:
	// each one's Error() implementation is verified to render only broker- or
	// server-generated text, never unreviewed content from an intermediary
	// (proxy/gateway/WAF) or credentials (auth is applied via headers, not
	// URLs). errors.As above matches an audited type anywhere in the wrap
	// chain, but detail = (*toolErr).Error() below renders the OUTERMOST
	// error's text — so this guarantee also depends on every wrapper in that
	// chain being package-authored (true today). Wrapping an audited type in
	// something unaudited would silently widen this gate. This held for
	// sempv1.Error, RetriesExhaustedError, and ExchangeError from the start;
	// sempv2.SEMPError's Error() used to fall
	// back to the raw, unparsed HTTP response body when the broker's
	// meta.error envelope failed to parse — exactly the intermediary-response
	// case this comment warned about — until SOL-153766 fixed Error() itself
	// to drop that fallback. For any other error — including the
	// unknown/default case that buildErrorResult deliberately hides behind
	// genericInternalMessage — we can't vouch for the contents, so we log only
	// the Go type, never the message. ReplaceAttr can't help here (it keys off
	// field names, and "detail" is a raw string), so this type gate is the
	// actual safeguard.
	detail := fmt.Sprintf("%T", *toolErr)
	if isV1 || isV2 || isRetries || isBusy || isExchange {
		detail = (*toolErr).Error()
	}

	attrs := []slog.Attr{
		slog.String("tool", tool),
		slog.String("broker", brokerLabel),
		slog.String("outcome", "error"),
		slog.String("error_type", string(*errorType)),
		slog.Duration("duration", dur),
		slog.String("detail", detail),
		slog.Any("", id),
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
	case isExchange:
		attrs = append(attrs, exchErr.LogAttrs()...)
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
func (m *ToolManager) classifyBrokerError(alias string, err error) (metrics.ErrorType, error) {
	if errors.Is(err, semp.ErrUnknownBroker) {
		return metrics.ErrorTypeUnknownBroker, fmt.Errorf("unknown broker %q; available brokers: %s",
			alias, strings.Join(m.pool.Aliases(), ", "))
	}
	return metrics.ErrorTypeBrokerInitError, fmt.Errorf("connecting to broker %q: %w", alias, err)
}
