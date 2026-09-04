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

// ErrorType is the failure cause carried on an error outcome, a closed set.
// Empty on any non-error outcome. Keeping it a named type with exported consts
// lets the compiler catch a mistyped value and bounds the metric label's
// cardinality by construction.
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
// duration histogram, and an unlabelled in-flight HTTP request gauge. Every
// method is nil-receiver-safe, so a disabled server (nil *ToolMetrics) records
// nothing at the cost of one nil check.
type ToolMetrics struct {
	invocations    metric.Int64Counter
	duration       metric.Float64Histogram
	activeRequests metric.Int64UpDownCounter
}

// NewToolMetrics registers the RED instruments against meter. The published
// Prometheus names are derived by the exporter from the instrument name and
// unit (ADR-008): mcp_tool_invocation_total, mcp_tool_invocation_duration_seconds,
// and mcp_http_active_requests.
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

// Record observes one tool invocation on the counter and the duration histogram
// with matching labels. A non-empty errorType outside the closed set is coerced
// to ErrorTypeOther so a caller cannot mint an unbounded series. No-op on a nil
// receiver.
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
