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

package metrics

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Outcome is the result of a tool invocation: a closed set shared by the metric
// label, the log field, the audit event, and the span attribute so they cannot
// disagree.
type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeError   Outcome = "error"
	// OutcomeCancelled is reserved; the classification that produces it is wired
	// by a later story.
	OutcomeCancelled Outcome = "cancelled"
)

// ErrorType is the failure cause on an error outcome, a closed set. Empty on
// non-error outcomes. A named type with consts keeps typos from compiling and
// bounds the label's cardinality.
type ErrorType string

const (
	ErrorTypePanic                 ErrorType = "panic"
	ErrorTypeBadRequest            ErrorType = "bad_request"
	ErrorTypeUnknownTool           ErrorType = "unknown_tool"
	ErrorTypeMissingBroker         ErrorType = "missing_broker"
	ErrorTypeUnknownBroker         ErrorType = "unknown_broker"
	ErrorTypeBrokerInitError       ErrorType = "broker_init_error"
	ErrorTypeValidationError       ErrorType = "validation_error"
	ErrorTypeExecutionError        ErrorType = "execution_error"
	ErrorTypeNilResult             ErrorType = "nil_result"
	ErrorTypeNotFound              ErrorType = "not_found"
	ErrorTypeOutputValidationError ErrorType = "output_validation_error"
	ErrorTypeMarshalError          ErrorType = "marshal_error"
	// ErrorTypeBrokerPermissionDenied marks a hop-2 (broker-side) authorization
	// denial: SEMPv1's ErrorKindPermission or SEMPv2's error code 72, classified
	// at the tools layer (SOL-153332, Story 49). Distinct from
	// ErrorTypeExecutionError so a compliance reviewer can tell "the broker
	// refused the exchanged identity" apart from every other handler failure
	// without inspecting the SEMP error body.
	ErrorTypeBrokerPermissionDenied ErrorType = "broker_permission_denied"
	// ErrorTypeOther is the sentinel Record coerces any value outside the closed
	// set to, so an unexpected string can never mint a new series.
	ErrorTypeOther ErrorType = "other"
)

// allErrorTypes is the vocabulary, and the single place it is enumerated.
// Everything that needs the set — Record's coercion check, the cross-signal
// tests, the doc-table check — derives from here, so a new const cannot be
// added to the type while some consumer silently keeps the old set.
//
// That failure mode is not hypothetical: while this list and the coercion map
// were maintained separately, a const added to the type but missed in the map
// made Record coerce it to `other` while the span attribute carried the real
// value — the same call reading two different ways on two signals, with
// nothing failing in CI. ErrorTypeOther is included because Record legitimately
// emits it; AllErrorTypes documents how to exclude it.
var allErrorTypes = []ErrorType{
	ErrorTypePanic,
	ErrorTypeBadRequest,
	ErrorTypeUnknownTool,
	ErrorTypeMissingBroker,
	ErrorTypeUnknownBroker,
	ErrorTypeBrokerInitError,
	ErrorTypeValidationError,
	ErrorTypeExecutionError,
	ErrorTypeNilResult,
	ErrorTypeNotFound,
	ErrorTypeOutputValidationError,
	ErrorTypeMarshalError,
	ErrorTypeBrokerPermissionDenied,
	ErrorTypeOther,
}

// AllErrorTypes returns the closed error_type vocabulary, including the
// ErrorTypeOther coercion sentinel. Callers that want only the values a
// classifier may legitimately produce should drop ErrorTypeOther: nothing sets
// it deliberately, and a span carrying it means the classifier produced a value
// the set does not cover.
//
// Exported so a test in another package can assert its own copy of the set
// matches this one, rather than drifting from it silently.
func AllErrorTypes() []ErrorType {
	out := make([]ErrorType, len(allErrorTypes))
	copy(out, allErrorTypes)
	return out
}

// knownErrorTypes is the closed set Record validates against, built from
// allErrorTypes so it cannot lag the type. The empty string (non-error
// outcomes) is valid and handled separately.
var knownErrorTypes = func() map[ErrorType]bool {
	m := make(map[ErrorType]bool, len(allErrorTypes))
	for _, et := range allErrorTypes {
		m[et] = true
	}
	return m
}()

// ToolMetrics holds the per-tool RED instruments: an invocation counter, a
// duration histogram, and an unlabelled in-flight gauge. Every method is
// nil-safe, so a disabled server (nil) records nothing.
type ToolMetrics struct {
	invocations       metric.Int64Counter
	duration          metric.Float64Histogram
	activeRequests    metric.Int64UpDownCounter
	brokerAuthzDenied metric.Int64Counter
}

// NewToolMetrics registers the RED instruments. The exporter derives the
// published Prometheus names from the instrument name and unit (ADR-008).
func NewToolMetrics(meter metric.Meter) (*ToolMetrics, error) {
	invocations, err := meter.Int64Counter(
		"mcp.tool.invocation",
		metric.WithDescription("Number of tool invocations."),
		metric.WithUnit("1"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_tool_invocation_total: %w", err)
	}

	duration, err := meter.Float64Histogram(
		"mcp.tool.invocation.duration",
		metric.WithDescription("Duration of tool invocation in seconds"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10),
		metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_tool_invocation_duration_seconds: %w", err)
	}

	activeRequests, err := meter.Int64UpDownCounter(
		"mcp.http.active_requests",
		metric.WithDescription("Number of in-flight HTTP requests"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_http_active_requests: %w", err)
	}

	// mcp_broker_authz_denied_total (SOL-153332, Story 49): a hop-2 counterpart
	// to hop-1's mcp_authz_denied_total, counting a broker-side permission
	// denial rather than an MCP-server-side one.
	brokerAuthzDenied, err := meter.Int64Counter(
		"mcp.broker.authz_denied",
		metric.WithDescription("Number of tool calls denied by broker-side (hop-2) authorization."),
		metric.WithUnit("1"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_broker_authz_denied_total: %w", err)
	}

	return &ToolMetrics{
		invocations:       invocations,
		duration:          duration,
		activeRequests:    activeRequests,
		brokerAuthzDenied: brokerAuthzDenied,
	}, nil
}

// Record observes one invocation on the counter and histogram with matching
// labels. An errorType outside the closed set is coerced to ErrorTypeOther.
// No-op on a nil receiver.
func (t *ToolMetrics) Record(ctx context.Context, tool, broker string, outcome Outcome, errorType ErrorType, dur time.Duration) {
	if t == nil {
		return
	}
	if errorType != "" && !knownErrorTypes[errorType] {
		errorType = ErrorTypeOther
	}
	opts := metric.WithAttributes(
		attribute.String("tool", tool),
		attribute.String("broker", broker),
		attribute.String("outcome", string(outcome)),
		attribute.String("error_type", string(errorType)),
	)

	t.invocations.Add(ctx, 1, opts)
	t.duration.Record(ctx, dur.Seconds(), opts)
}

// IncActive increments the in-flight HTTP request gauge. No-op on a nil receiver.
func (t *ToolMetrics) IncActive(ctx context.Context) {
	if t == nil {
		return
	}
	t.activeRequests.Add(ctx, 1)
}

// DecActive decrements the in-flight HTTP request gauge. No-op on a nil receiver.
func (t *ToolMetrics) DecActive(ctx context.Context) {
	if t == nil {
		return
	}
	t.activeRequests.Add(ctx, -1)
}

// DenialReason is the closed set mcp_broker_authz_denied_total's reason label
// can carry (SOL-153332, Story 49). Mirrors ErrorType: a bare string here
// would let a value threaded from a SEMP description, or a second reason
// added later without updating the coercion, mint an unbounded series —
// exactly what ErrorType's own closed-set handling exists to prevent.
type DenialReason string

const (
	// DenialReasonPermissionDenied is the only reason a hop-2 denial carries
	// today: the broker refused the exchanged identity.
	DenialReasonPermissionDenied DenialReason = "permission_denied"
	// DenialReasonOther is the sentinel RecordBrokerAuthzDenied coerces any
	// value outside knownDenialReasons to, so an unexpected string can never
	// mint a new series.
	DenialReasonOther DenialReason = "other"
)

// knownDenialReasons is the closed set RecordBrokerAuthzDenied validates
// against.
var knownDenialReasons = map[DenialReason]bool{
	DenialReasonPermissionDenied: true,
}

// RecordBrokerAuthzDenied increments mcp_broker_authz_denied_total for one
// hop-2 (broker-side) authorization denial (SOL-153332, Story 49). No-op on a
// nil receiver — metrics disabled records nothing, same as every other method
// here.
func (t *ToolMetrics) RecordBrokerAuthzDenied(ctx context.Context, tool, broker string, reason DenialReason) {
	if t == nil {
		return
	}
	if !knownDenialReasons[reason] {
		reason = DenialReasonOther
	}
	t.brokerAuthzDenied.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tool", tool),
		attribute.String("broker", broker),
		attribute.String("reason", string(reason)),
	))
}

// SEMPMetrics holds the two SEMP instruments: a request counter and a duration
// histogram, both written once per attempt. Methods are safe to call on nil.
type SEMPMetrics struct {
	requests metric.Int64Counter
	duration metric.Float64Histogram
}

// SEMPRequest is the set of label values for one SEMP request attempt, one
// field per metric label.
type SEMPRequest struct {
	API       string // "v1" or "v2"
	Broker    string // operator's configured alias
	Operation string // SEMP operation name
	Method    string // HTTP request method
	Status    string // HTTP status as a string; empty on no response (semconv types this int; string carries the no-response case)
	Address   string // server host
	Attempt   int    // retry attempt, 1-based
}

// NewSEMPMetrics registers the two SEMP instruments. The exporter builds their
// Prometheus names from the instrument name and unit. Buckets start higher than
// the tool histogram: a SEMP call is a network round-trip.
func NewSEMPMetrics(meter metric.Meter) (*SEMPMetrics, error) {
	requests, err := meter.Int64Counter(
		"mcp.semp.request",
		metric.WithDescription("Number of SEMP request attempts."),
		metric.WithUnit("1"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_semp_request_total: %w", err)
	}

	duration, err := meter.Float64Histogram(
		"mcp.semp.request.duration",
		metric.WithDescription("Duration of a SEMP request attempt in seconds"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
		metric.WithUnit("s"))
	if err != nil {
		return nil, fmt.Errorf("register mcp_semp_request_duration_seconds: %w", err)
	}

	return &SEMPMetrics{requests: requests, duration: duration}, nil
}

// Record writes one attempt to both instruments. HTTP labels use OTel
// semantic-convention keys; the exporter turns the dots into underscores. Safe
// to call on nil.
func (s *SEMPMetrics) Record(ctx context.Context, r SEMPRequest, dur time.Duration) {
	if s == nil {
		return
	}
	// attempt goes on the counter only; off the histogram it would multiply the
	// bucket series by the retry cap.
	base := []attribute.KeyValue{
		attribute.String("http.request.method", r.Method),
		attribute.String("http.response.status_code", r.Status),
		attribute.String("server.address", r.Address),
		attribute.String("broker", r.Broker),
		attribute.String("api", r.API),
		attribute.String("operation", r.Operation),
	}

	s.requests.Add(ctx, 1, metric.WithAttributes(append(base, attribute.String("attempt", strconv.Itoa(r.Attempt)))...))
	s.duration.Record(ctx, dur.Seconds(), metric.WithAttributes(base...))
}

// SecurityMetrics holds the two security counters (SOL-152099):
// mcp_auth_failure_total{reason} and mcp_authz_denied_total{tool,reason}.
// Every method is nil-safe, so a disabled server (nil) records nothing.
//
// The auth-failure counter carries no tool or broker label because
// authentication fails before either is selected. Cardinality is |reason| for
// the first and |tool| x 2 for the second.
type SecurityMetrics struct {
	authFailures metric.Int64Counter
	authzDenials metric.Int64Counter
}

// NewSecurityMetrics registers both counters and seeds every
// schema.AuthFailureReasons() series at zero so increase() fires on a process's
// first failure (see panics.Register for the rationale). mcp_authz_denied_total
// is not seeded: its tool dimension is only known at registration, and the
// series is documented as absent where tool authorization is off.
func NewSecurityMetrics(meter metric.Meter) (*SecurityMetrics, error) {
	authFailures, err := meter.Int64Counter(
		"mcp.auth.failure",
		metric.WithDescription("Number of credentials rejected at the HTTP boundary, by reason."))
	if err != nil {
		return nil, fmt.Errorf("register mcp_auth_failure_total: %w", err)
	}

	authzDenials, err := meter.Int64Counter(
		"mcp.authz.denied",
		metric.WithDescription("Number of tool calls refused by tool authorization, by tool and reason."))
	if err != nil {
		return nil, fmt.Errorf("register mcp_authz_denied_total: %w", err)
	}

	s := &SecurityMetrics{authFailures: authFailures, authzDenials: authzDenials}
	ctx := context.Background()
	for _, reason := range schema.AuthFailureReasons() {
		s.authFailures.Add(ctx, 0, metric.WithAttributes(attribute.String("reason", string(reason))))
	}
	return s, nil
}

// RecordAuthFailure counts one rejected credential. Typed to the vocabulary so
// the only string widening is CountingAuthHook's, at the interface boundary.
// No-op on a nil receiver.
func (s *SecurityMetrics) RecordAuthFailure(ctx context.Context, reason schema.AuthFailureReason) {
	if s == nil {
		return
	}
	s.authFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", string(reason))))
}

// RecordAuthzDenied counts one tool call refused by tool authorization. reason
// is one of the decision_reason constants in internal/tools/authorization.go.
// No-op on a nil receiver.
func (s *SecurityMetrics) RecordAuthzDenied(ctx context.Context, tool, reason string) {
	if s == nil {
		return
	}
	s.authzDenials.Add(ctx, 1, metric.WithAttributes(
		attribute.String("tool", tool),
		attribute.String("reason", reason),
	))
}
