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
	// ErrorTypeOther is the sentinel Record coerces any value outside the closed
	// set to, so an unexpected string can never mint a new series.
	ErrorTypeOther ErrorType = "other"
)

// knownErrorTypes is the closed set Record validates against. The empty string
// (non-error outcomes) is valid and handled separately.
var knownErrorTypes = map[ErrorType]bool{
	ErrorTypePanic: true, ErrorTypeBadRequest: true, ErrorTypeUnknownTool: true,
	ErrorTypeMissingBroker: true, ErrorTypeUnknownBroker: true, ErrorTypeBrokerInitError: true,
	ErrorTypeValidationError: true, ErrorTypeExecutionError: true, ErrorTypeNilResult: true,
	ErrorTypeNotFound: true, ErrorTypeOutputValidationError: true, ErrorTypeMarshalError: true,
	ErrorTypeOther: true,
}

// ToolMetrics holds the per-tool RED instruments: an invocation counter, a
// duration histogram, and an unlabelled in-flight gauge. Every method is
// nil-safe, so a disabled server (nil) records nothing.
type ToolMetrics struct {
	invocations    metric.Int64Counter
	duration       metric.Float64Histogram
	activeRequests metric.Int64UpDownCounter
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

	return &ToolMetrics{invocations: invocations, duration: duration, activeRequests: activeRequests}, nil
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
