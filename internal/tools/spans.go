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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
)

// tracer is this layer's named tracer (SOL-152421), so a backend attributes
// each span to the source that produced it rather than to one server-wide
// scope.
var tracer = otel.Tracer("solace-broker-mcp/tools")

// dispatchSpanName is the span every tool dispatch produces, whichever site
// dispatched it. One name with a `tool` attribute — rather than a span named
// per tool — mirrors how the metric works (`mcp_tool_invocation_total` with a
// `tool` label) and is what lets one filter cover every tool.
const dispatchSpanName = "tools.CallTool"

// endDispatchSpan writes the dispatch-span attributes and closes span.
//
// Shared by FOUR dispatch sites: ToolManager.CallTool, and three that bypass it
// — list-brokers and describe-semp-schema, both registered directly against the
// MCP server, and the argument-parse failure in the instrumented closure in
// register.go. Every one owes a span, for the same reason each already emits
// its own audit line and metric: the metric series carries a `tool` and an
// `error_type`, an operator carries those values into the trace backend
// unchanged, and a missing span means they find nothing.
//
// **Each site must register its deferred call to this BEFORE the defer that
// emits the log line, the metric and the audit record**, so LIFO runs the span
// last — after a recovered panic has been reclassified, so every signal reports
// the same cause. The straight-line argument-parse site has no defer to order
// against and instead starts its span first, threading the returned context
// into the emission calls.
//
// Folding all of it into one seam that owns the ordering was tried and
// reverted: internal/tools/audit_error_type_drift_test.go (SOL-152090) scans
// this package for `logToolResult(..., &errorType, ...)` as the funnel every
// error_type value passes through, and a seam holding the variable behind a
// struct field hides both that call and the `panic` reclassification from it.
// The ordering is enforced behaviourally instead, by the panic case of
// TestRequestPathSpans_SpanAndMetricAgreeOnTheSameCall and by
// TestDispatch_AuditAndMetricSeeTheDispatchSpanInContext.
//
// brokerLabel must be the canonical metric label, never a raw caller-supplied
// alias: for an unresolved broker the raw value is whatever string the caller
// typed, so it is unbounded, untrusted input that would egress to the
// collector, and it breaks the span-to-metric join on caller casing alone.
func endDispatchSpan(ctx context.Context, span trace.Span, tool, brokerLabel string, errorType metrics.ErrorType, toolErr error) {
	// IsRecording guard: a non-recording span still needs End(), but building
	// attributes nothing reads is waste on every tool call.
	if span.IsRecording() {
		outcome := "success"
		if toolErr != nil {
			outcome = "error"
		}
		attrs := []attribute.KeyValue{
			attribute.String("tool", tool),
			attribute.String("outcome", outcome),
			attribute.String("broker", brokerLabel),
		}
		if id := correlation.From(ctx); id != "" {
			attrs = append(attrs, attribute.String("correlation_id", id))
		}
		if toolErr != nil {
			// The same closed set as the audit record's error_type and the
			// metric label — one predicate across all three surfaces. See
			// docs/observability.md.
			attrs = append(attrs, attribute.String("error_type", string(errorType)))
		}
		span.SetAttributes(attrs...)
	}
	if toolErr != nil {
		// No description, no RecordError: that is unvouched text (the same
		// text logToolResult gates behind a type check), and a span exports
		// offsite. error_type carries the classification.
		span.SetStatus(codes.Error, "")
	}
	span.End()
}
