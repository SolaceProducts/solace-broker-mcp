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

package tracing

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reason values for mcp_otel_spans_dropped_total{reason} — a closed set
// (ISSUE-029).
const (
	reasonQueueFull     = "queue_full"
	reasonExportTimeout = "export_timeout"
	reasonExportError   = "export_error"
	reasonShutdown      = "shutdown"
)

// instrumentScope names the meter that owns tracing's self-observation
// instruments, matching Story 14's convention (internal/observability/metrics).
const instrumentScope = "github.com/SolaceProducts/solace-broker-mcp"

// exportStats is the single source of truth for span export outcomes: plain
// atomics, so the periodic INFO fallback (Decision #12, selfstats.go) can read
// current totals even when metrics are off and no OTel instrument exists to
// read them back from. registerInstruments additionally mirrors each update
// onto a real OTel counter when metrics are on.
//
// reasonQueueFull is defined for schema completeness but is never incremented
// in v1: go.opentelemetry.io/otel/sdk's BatchSpanProcessor drops
// queue-overflow spans internally against an unexported counter with no
// accessor (verified against sdk@v1.46.0/trace/batch_span_processor.go), so
// there is no supported way to observe that specific reason from outside the
// SDK. Disclosed as a known gap rather than faked; see the PR description.
type exportStats struct {
	exported             atomic.Int64
	droppedQueueFull     atomic.Int64
	droppedExportTimeout atomic.Int64
	droppedExportError   atomic.Int64
	droppedShutdown      atomic.Int64

	// exportedCounter and droppedCounter are nil until registerInstruments
	// runs (metrics enabled). recordExported/recordDropped check for nil so
	// the atomics above stay authoritative either way.
	exportedCounter metric.Int64Counter
	droppedCounter  metric.Int64Counter
}

func newExportStats() *exportStats {
	return &exportStats{}
}

// registerInstruments creates mcp_otel_spans_exported_total and
// mcp_otel_spans_dropped_total{reason} against meterProvider. Call only when
// metrics are enabled (meterProvider non-nil).
func (s *exportStats) registerInstruments(meterProvider *sdkmetric.MeterProvider) error {
	meter := meterProvider.Meter(instrumentScope)

	exportedCounter, err := meter.Int64Counter(
		"mcp.otel.spans.exported",
		metric.WithDescription("Spans successfully exported over OTLP."),
	)
	if err != nil {
		return fmt.Errorf("register mcp_otel_spans_exported_total: %w", err)
	}

	droppedCounter, err := meter.Int64Counter(
		"mcp.otel.spans.dropped",
		metric.WithDescription("Spans dropped before or during OTLP export, by reason."),
	)
	if err != nil {
		return fmt.Errorf("register mcp_otel_spans_dropped_total: %w", err)
	}

	s.exportedCounter = exportedCounter
	s.droppedCounter = droppedCounter
	return nil
}

// recordExported adds n to the exported total and, when registered, to the
// exported counter.
func (s *exportStats) recordExported(ctx context.Context, n int64) {
	if n <= 0 {
		return
	}
	s.exported.Add(n)
	if s.exportedCounter != nil {
		s.exportedCounter.Add(ctx, n)
	}
}

// recordDropped adds n to the total for reason and, when registered, to the
// dropped counter labelled by reason. reason must be one of the four
// constants above; an unrecognized value is dropped silently rather than
// widening the label's cardinality — every call site in this package passes a
// package constant, so that should never happen.
func (s *exportStats) recordDropped(ctx context.Context, reason string, n int64) {
	if n <= 0 {
		return
	}
	switch reason {
	case reasonQueueFull:
		s.droppedQueueFull.Add(n)
	case reasonExportTimeout:
		s.droppedExportTimeout.Add(n)
	case reasonExportError:
		s.droppedExportError.Add(n)
	case reasonShutdown:
		s.droppedShutdown.Add(n)
	default:
		return
	}
	if s.droppedCounter != nil {
		s.droppedCounter.Add(ctx, n, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

// snapshot returns the current totals for the periodic INFO fallback.
func (s *exportStats) snapshot() (exported, queueFull, exportTimeout, exportError, shutdown int64) {
	return s.exported.Load(),
		s.droppedQueueFull.Load(),
		s.droppedExportTimeout.Load(),
		s.droppedExportError.Load(),
		s.droppedShutdown.Load()
}

// countingExporter wraps a trace.SpanExporter, classifying every ExportSpans
// outcome into stats so support can diagnose OTLP export health — from
// /metrics or the periodic INFO fallback — without a live collector or
// customer-side tracing infrastructure in place.
type countingExporter struct {
	next  sdktrace.SpanExporter
	stats *exportStats
}

// ExportSpans classifies the wrapped exporter's outcome: success increments
// the exported total; a deadline-exceeded error increments export_timeout;
// any other error increments export_error. The original error always
// propagates unchanged so the batch span processor's own retry/drop behaviour
// is unaffected.
//
// Both a bare context.DeadlineExceeded AND a gRPC DeadlineExceeded status
// count as a timeout. otlptracegrpc's own export timeout (and most real
// collector-side deadline behaviour) surfaces as the latter — a gRPC status
// error, not a wrapped context error — so checking errors.Is alone
// misclassified every real timeout as export_error and left
// export_timeout permanently at zero. Confirmed against the real exporter
// during review.
func (e *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	n := int64(len(spans))
	if err := e.next.ExportSpans(ctx, spans); err != nil {
		reason := reasonExportError
		if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
			reason = reasonExportTimeout
		}
		e.stats.recordDropped(ctx, reason, n)
		return err
	}
	e.stats.recordExported(ctx, n)
	return nil
}

// Shutdown delegates to the wrapped exporter. Spans that fail to flush during
// shutdown are counted by Provider.Shutdown (provider.go), which is the only
// caller that knows a shutdown is in progress; this method has no visibility
// into WHY a later call might fail, so it must not guess a reason here.
func (e *countingExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}
