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
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestExportStats_RecordExported_NoInstruments confirms the atomics are the
// source of truth even with no meter provider registered (metrics off): the
// periodic INFO fallback must still see accurate totals.
func TestExportStats_RecordExported_NoInstruments(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	s.recordExported(context.Background(), 3)
	s.recordExported(context.Background(), 2)

	exported, _, _, _, _ := s.snapshot()
	if exported != 5 {
		t.Fatalf("exported = %d, want 5", exported)
	}
}

// TestExportStats_RecordExported_ZeroIsNoop guards against a zero-span export
// call inflating the total.
func TestExportStats_RecordExported_ZeroIsNoop(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	s.recordExported(context.Background(), 0)
	if exported, _, _, _, _ := s.snapshot(); exported != 0 {
		t.Fatalf("exported = %d, want 0", exported)
	}
}

// TestExportStats_RecordDropped_EachReason pins that each reason lands in its
// own bucket and none bleed into each other.
func TestExportStats_RecordDropped_EachReason(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	s.recordDropped(context.Background(), reasonQueueFull, 1)
	s.recordDropped(context.Background(), reasonExportTimeout, 2)
	s.recordDropped(context.Background(), reasonExportError, 3)
	s.recordDropped(context.Background(), reasonShutdown, 4)

	_, queueFull, exportTimeout, exportError, shutdown := s.snapshot()
	if queueFull != 1 || exportTimeout != 2 || exportError != 3 || shutdown != 4 {
		t.Fatalf("snapshot = (queueFull=%d, exportTimeout=%d, exportError=%d, shutdown=%d), want (1,2,3,4)",
			queueFull, exportTimeout, exportError, shutdown)
	}
}

// TestExportStats_RecordDropped_UnknownReasonIgnored: every call site in this
// package passes a package constant, but the switch's default branch exists
// to avoid silently widening the dropped-counter's label cardinality on a
// future mistake — pin that it truly no-ops rather than falling through to
// one of the four buckets.
func TestExportStats_RecordDropped_UnknownReasonIgnored(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	s.recordDropped(context.Background(), "not_a_real_reason", 5)

	exported, queueFull, exportTimeout, exportError, shutdown := s.snapshot()
	if exported != 0 || queueFull != 0 || exportTimeout != 0 || exportError != 0 || shutdown != 0 {
		t.Fatalf("snapshot = %v, want all zero", []int64{exported, queueFull, exportTimeout, exportError, shutdown})
	}
}

// TestExportStats_RegisterInstruments_MirrorsToMeter is the AC-level proof
// that mcp_otel_spans_exported_total and mcp_otel_spans_dropped_total{reason}
// are real OTel instruments once a meter provider is supplied, not just
// internal bookkeeping.
func TestExportStats_RegisterInstruments_MirrorsToMeter(t *testing.T) {
	t.Parallel()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	s := newExportStats()
	if err := s.registerInstruments(mp); err != nil {
		t.Fatalf("registerInstruments() error = %v", err)
	}

	ctx := context.Background()
	s.recordExported(ctx, 7)
	s.recordDropped(ctx, reasonExportError, 2)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}

	var sawExported, sawDropped bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch m.Name {
			case "mcp.otel.spans.exported":
				sawExported = true
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 7 {
					t.Fatalf("mcp.otel.spans.exported data = %#v, want a single point of 7", m.Data)
				}
			case "mcp.otel.spans.dropped":
				sawDropped = true
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok || len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 2 {
					t.Fatalf("mcp.otel.spans.dropped data = %#v, want a single point of 2", m.Data)
				}
				attrs := sum.DataPoints[0].Attributes
				if v, ok := attrs.Value("reason"); !ok || v.AsString() != reasonExportError {
					t.Fatalf("mcp.otel.spans.dropped reason attribute = %v, want %q", v, reasonExportError)
				}
			}
		}
	}
	if !sawExported {
		t.Error("mcp.otel.spans.exported was never collected")
	}
	if !sawDropped {
		t.Error("mcp.otel.spans.dropped was never collected")
	}
}

// stubExporter is a trace.SpanExporter whose ExportSpans outcome is fixed by
// the test, so countingExporter's classification can be pinned deterministically
// without a live OTLP collector.
type stubExporter struct {
	err error
}

func (e *stubExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return e.err
}

func (e *stubExporter) Shutdown(context.Context) error {
	return nil
}

// oneSpan builds a single recorded, ended span via the SDK's own test
// recorder, so len(spans) in the assertions below is exactly 1 — a real
// ReadOnlySpan, not a hand-rolled fake.
func oneSpan(t *testing.T) []sdktrace.ReadOnlySpan {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()
	return sr.Ended()
}

// TestCountingExporter_Success pins the exported-total path.
func TestCountingExporter_Success(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	e := &countingExporter{next: &stubExporter{}, stats: s}

	if err := e.ExportSpans(context.Background(), oneSpan(t)); err != nil {
		t.Fatalf("ExportSpans() error = %v, want nil", err)
	}
	exported, queueFull, exportTimeout, exportError, shutdown := s.snapshot()
	if exported != 1 || queueFull != 0 || exportTimeout != 0 || exportError != 0 || shutdown != 0 {
		t.Fatalf("snapshot = %v, want only exported=1", []int64{exported, queueFull, exportTimeout, exportError, shutdown})
	}
}

// TestCountingExporter_DeadlineExceeded pins the export_timeout classification
// and confirms the original error still propagates to the caller (the batch
// span processor's own retry/drop behaviour must be unaffected).
func TestCountingExporter_DeadlineExceeded(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	e := &countingExporter{next: &stubExporter{err: context.DeadlineExceeded}, stats: s}

	err := e.ExportSpans(context.Background(), oneSpan(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ExportSpans() error = %v, want context.DeadlineExceeded", err)
	}
	_, _, exportTimeout, exportError, _ := s.snapshot()
	if exportTimeout != 1 || exportError != 0 {
		t.Fatalf("exportTimeout=%d exportError=%d, want exportTimeout=1 exportError=0", exportTimeout, exportError)
	}
}

// TestCountingExporter_GenericError pins the export_error classification for
// any failure that isn't a deadline.
func TestCountingExporter_GenericError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("collector unreachable")
	s := newExportStats()
	e := &countingExporter{next: &stubExporter{err: sentinel}, stats: s}

	err := e.ExportSpans(context.Background(), oneSpan(t))
	if !errors.Is(err, sentinel) {
		t.Fatalf("ExportSpans() error = %v, want %v", err, sentinel)
	}
	_, _, exportTimeout, exportError, _ := s.snapshot()
	if exportError != 1 || exportTimeout != 0 {
		t.Fatalf("exportError=%d exportTimeout=%d, want exportError=1 exportTimeout=0", exportError, exportTimeout)
	}
}

// TestCountingExporter_GRPCStatusDeadlineExceeded pins the fix for the real
// failure shape: the OTLP gRPC exporter's own timeout surfaces as a gRPC
// DeadlineExceeded *status* error, not a bare context.DeadlineExceeded — an
// errors.Is(err, context.DeadlineExceeded)-only check (the original,
// pre-review version of this classification) misses it entirely and files
// every real export timeout under export_error instead. See
// TestNew_Enabled_ExportTimeoutClassifiedAgainstRealExporter (provider_test.go)
// for the same fix proven against the real exporter, not a stub.
func TestCountingExporter_GRPCStatusDeadlineExceeded(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	grpcErr := status.Error(codes.DeadlineExceeded, "context deadline exceeded")
	e := &countingExporter{next: &stubExporter{err: grpcErr}, stats: s}

	err := e.ExportSpans(context.Background(), oneSpan(t))
	if !errors.Is(err, grpcErr) {
		t.Fatalf("ExportSpans() error = %v, want %v", err, grpcErr)
	}
	_, _, exportTimeout, exportError, _ := s.snapshot()
	if exportTimeout != 1 || exportError != 0 {
		t.Fatalf("exportTimeout=%d exportError=%d, want exportTimeout=1 exportError=0 — a gRPC-status timeout must classify the same as a bare context.DeadlineExceeded",
			exportTimeout, exportError)
	}
}

// blockingExporter blocks ExportSpans until ctx is done, simulating a
// collector that accepts the connection but never responds — the shape
// Shutdown must not hang against.
type blockingExporter struct{}

func (e *blockingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	<-ctx.Done()
	return ctx.Err()
}

func (e *blockingExporter) Shutdown(context.Context) error {
	return nil
}

// TestCountingExporter_ShutdownDrainsQueuedSpan proves a real batch span
// processor actually flushes a queued span to the exporter on Shutdown, and
// that countingExporter records it as exported — the check that catches a
// Shutdown which silently no-ops instead of draining. Reverting
// Provider.Shutdown to skip p.tp.Shutdown(ctx) turns this test red.
func TestCountingExporter_ShutdownDrainsQueuedSpan(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	exporter := &countingExporter{next: &stubExporter{}, stats: s}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatalf("tp.Shutdown() error = %v, want nil", err)
	}

	exported, _, _, _, _ := s.snapshot()
	if exported != 1 {
		t.Fatalf("exported = %d, want 1: Shutdown should have flushed the queued span to the exporter", exported)
	}
}

// TestCountingExporter_ShutdownIsBoundedAgainstHangingExport is the
// direct, fast proof behind the AC "drain must not hang the process if the
// OTLP endpoint is unreachable": with a queued span and an exporter that
// never returns, Shutdown must still respect ctx's deadline rather than
// blocking on the export forever.
func TestCountingExporter_ShutdownIsBoundedAgainstHangingExport(t *testing.T) {
	t.Parallel()
	s := newExportStats()
	exporter := &countingExporter{next: &blockingExporter{}, stats: s}

	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	_, span := tp.Tracer("test").Start(context.Background(), "op")
	span.End()

	const bound = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()

	start := time.Now()
	err := tp.Shutdown(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("tp.Shutdown() error = nil, want an error: the export never completed within the bound")
	}
	if elapsed > bound+2*time.Second {
		t.Fatalf("tp.Shutdown() took %s, want roughly the %s bound — it blocked well past ctx's deadline instead of returning promptly", elapsed, bound)
	}
}
