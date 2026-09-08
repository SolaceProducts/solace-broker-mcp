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
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/oteldiag"
)

// Reason values for mcp_otel_metrics_dropped_total{reason} — a closed set,
// deliberately spelled identically to tracing's mcp_otel_spans_dropped_total
// reasons (internal/observability/tracing/stats.go) so the two pairs read as
// one vocabulary rather than two similar-but-different ones.
const (
	reasonQueueFull     = "queue_full"
	reasonExportTimeout = "export_timeout"
	reasonExportError   = "export_error"
	reasonShutdown      = "shutdown"
)

// otlpStats holds the OTLP metrics egress self-observation counters
// (SOL-152418, Story 46): mcp_otel_metrics_exported_total and
// mcp_otel_metrics_dropped_total{reason}.
//
// Unlike tracing's exportStats (internal/observability/tracing/stats.go),
// this has no atomics-plus-nil-checked-counter split and no periodic INFO
// fallback for a "metrics disabled" case: the OTLP reader this type
// instruments only ever exists when the metrics capability itself is on
// (validateMetricsOTLPCoherence rejects the opposite at config load), so the
// counters below are always registered by the time anything calls them.
type otlpStats struct {
	exportedCounter metric.Int64Counter
	droppedCounter  metric.Int64Counter
}

// newOTLPStats returns a bare, unregistered otlpStats. Construction is split
// from registerInstruments (below) to break a circular dependency: the OTLP
// reader must be built and passed to sdkmetric.NewMeterProvider as a
// WithReader option BEFORE that call returns a *sdkmetric.MeterProvider —
// but this type's own counters can only be registered AGAINST that same,
// not-yet-constructed MeterProvider. New (provider.go) resolves the cycle by
// building the reader against this bare, nil-countered stats first, then
// calling registerInstruments once the MeterProvider it was attached to
// exists.
func newOTLPStats() *otlpStats {
	return &otlpStats{}
}

// registerInstruments creates mcp_otel_metrics_exported_total and
// mcp_otel_metrics_dropped_total{reason} against meterProvider, alongside the
// existing span pair (docs/observability.md). meterProvider is the metrics
// package's own provider — the one this stats instance's reader was already
// attached to via WithReader — never a caller-supplied one, so the two
// cannot register against different roots.
func (s *otlpStats) registerInstruments(meterProvider *sdkmetric.MeterProvider) error {
	meter := meterProvider.Meter(instrumentScope)

	exportedCounter, err := meter.Int64Counter(
		"mcp.otel.metrics.exported",
		metric.WithDescription("Metric data points successfully exported over OTLP."),
	)
	if err != nil {
		return fmt.Errorf("register mcp_otel_metrics_exported_total: %w", err)
	}

	droppedCounter, err := meter.Int64Counter(
		"mcp.otel.metrics.dropped",
		metric.WithDescription("Metric data points dropped before or during OTLP export, by reason."),
	)
	if err != nil {
		return fmt.Errorf("register mcp_otel_metrics_dropped_total: %w", err)
	}

	s.exportedCounter = exportedCounter
	s.droppedCounter = droppedCounter
	return nil
}

// recordExported adds n to the exported counter. No-op for n<=0 so a
// zero-data-point export (a collection cycle with nothing new to report)
// does not mint a zero-value series churn, and no-op while the counter is
// still nil — the brief window between the reader being attached and
// registerInstruments completing (see newOTLPStats), which the SDK's default
// 60-second collection interval makes unreachable in practice but costs
// nothing to guard anyway.
func (s *otlpStats) recordExported(ctx context.Context, n int64) {
	if n <= 0 || s.exportedCounter == nil {
		return
	}
	s.exportedCounter.Add(ctx, n)
}

// recordDropped adds n to the dropped counter labelled by reason. reason must
// be one of the four constants above; every call site in this file passes a
// package constant, so an unrecognized value dropping silently rather than
// widening the label's cardinality should never happen in practice. Also a
// no-op while the counter is still nil — see recordExported.
func (s *otlpStats) recordDropped(ctx context.Context, reason string, n int64) {
	if n <= 0 || s.droppedCounter == nil {
		return
	}
	switch reason {
	case reasonQueueFull, reasonExportTimeout, reasonExportError, reasonShutdown:
	default:
		return
	}
	s.droppedCounter.Add(ctx, n, metric.WithAttributes(attribute.String("reason", reason)))
}

// countDataPoints sums the data points across every metric in rm — the
// OTLP-push analogue of counting spans in tracing's countingExporter. A
// batch's "how much telemetry" is proportional to data points, not to the
// number of distinct instruments (mcp_tool_invocation_total alone contributes
// one point per distinct tool/broker/outcome/error_type combination
// observed this cycle), so this is what makes the counter move with load
// rather than sitting at a constant equal to the instrument count.
//
// The switch covers every aggregation type metricdata ships as of SDK
// v1.46.0. An aggregation type this switch does not recognize (a future SDK
// addition) still counts as 1, so a batch is never silently invisible to the
// counter — undercounting its exact point total is preferable to that.
func countDataPoints(rm *metricdata.ResourceMetrics) int64 {
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				n += int64(len(d.DataPoints))
			case metricdata.Sum[float64]:
				n += int64(len(d.DataPoints))
			case metricdata.Gauge[int64]:
				n += int64(len(d.DataPoints))
			case metricdata.Gauge[float64]:
				n += int64(len(d.DataPoints))
			case metricdata.Histogram[int64]:
				n += int64(len(d.DataPoints))
			case metricdata.Histogram[float64]:
				n += int64(len(d.DataPoints))
			case metricdata.ExponentialHistogram[int64]:
				n += int64(len(d.DataPoints))
			case metricdata.ExponentialHistogram[float64]:
				n += int64(len(d.DataPoints))
			case metricdata.Summary:
				n += int64(len(d.DataPoints))
			default:
				n++
			}
		}
	}
	return n
}

// countingExporter wraps an sdkmetric.Exporter, classifying every Export
// outcome into stats so support can diagnose OTLP push health — from
// /metrics, which stays reachable regardless of whether the push side is
// working — without a live collector in place. Mirrors
// internal/observability/tracing's countingExporter for spans.
type countingExporter struct {
	next  sdkmetric.Exporter
	stats *otlpStats
}

// Temporality and Aggregation delegate unchanged: this wrapper only
// classifies Export outcomes, it does not alter what is asked for or how it
// is aggregated.
func (e *countingExporter) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return e.next.Temporality(k)
}

func (e *countingExporter) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return e.next.Aggregation(k)
}

// Export classifies the wrapped exporter's outcome: success increments the
// exported total by the batch's data-point count; a deadline-exceeded error
// increments export_timeout; any other error increments export_error. The
// original error always propagates unchanged so the PeriodicReader's own
// handling (logging via the SDK's global error handler) is unaffected.
//
// Both a bare context.DeadlineExceeded AND a gRPC DeadlineExceeded status
// count as a timeout — same reasoning, and the same real-exporter-verified
// necessity, as tracing's countingExporter: otlpmetricgrpc's own export
// timeout surfaces as a gRPC status error, not a wrapped context error, so
// checking errors.Is alone would leave export_timeout permanently at zero.
func (e *countingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	n := countDataPoints(rm)
	if err := e.next.Export(ctx, rm); err != nil {
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

// ForceFlush and Shutdown delegate to the wrapped exporter. A batch dropped
// during shutdown is counted by Provider.Shutdown (provider.go), the only
// caller that knows a shutdown — rather than an ordinary periodic export — is
// in progress; this method has no visibility into WHY a later call might
// fail, so it must not guess a reason here (mirrors tracing's
// countingExporter.Shutdown).
func (e *countingExporter) ForceFlush(ctx context.Context) error {
	return e.next.ForceFlush(ctx)
}

func (e *countingExporter) Shutdown(ctx context.Context) error {
	return e.next.Shutdown(ctx)
}

// cumulativeTemporality is passed to otlpmetricgrpc.WithTemporalitySelector
// so every instrument kind reports Cumulative regardless of the SDK's own
// default (which happens to already be cumulative, per
// sdkmetric.DefaultTemporalitySelector) or of a customer-set
// OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE environment variable
// (whose "delta" and "lowmemory" values are real, live options a customer
// might set for an unrelated OTLP-native app in the same environment).
// Explicit rather than relied-upon, per the ticket: Prometheus's own OTLP
// receiver needs the experimental otlp-deltatocumulative feature flag to
// accept delta, so shipping (or silently inheriting) delta would break the
// interop this story exists to provide.
func cumulativeTemporality(sdkmetric.InstrumentKind) metricdata.Temporality {
	return metricdata.CumulativeTemporality
}

// newOTLPReader builds the OTLP metric push reader (SOL-152418, Story 46):
// an otlpmetricgrpc.Exporter, wrapped for self-observation, inside a
// PeriodicReader. Endpoint and every other transport concern (TLS,
// compression, headers) come from the standard OTEL_EXPORTER_OTLP_ENDPOINT /
// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT environment variables and their
// siblings — no code here reads them explicitly, matching how
// internal/observability/tracing's otlptracegrpc.New call needs none either.
//
// Returns (nil, nil, err) on a construction failure — a malformed endpoint
// override, for example — rather than failing the whole metrics provider:
// the Prometheus scrape is the capability with no opt-in gate at the
// customer-visibility level once metrics are on at all, and a broken OTLP
// push configuration must not take it down too. The caller (New) logs and
// continues Prometheus-only; this mirrors internal/observability/tracing's
// own build-failure handling in cmd/server/main.go, non-fatal to the rest of
// the server.
func newOTLPReader(ctx context.Context, stats *otlpStats) (*sdkmetric.PeriodicReader, error) {
	// Before anything else: otlpmetricgrpc.New's own env-var parsing can log
	// through the OTel SDK's global error/log channel, which by default
	// prints straight to stderr, unrouted and unredacted — the identical risk
	// tracing.New guards against for otlptracegrpc.New (see
	// internal/observability/oteldiag's doc comment for why this must be
	// installed independently here rather than relying on tracing to have
	// done it: a deployment can run OTLP metrics with tracing off).
	oteldiag.Install()

	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithTemporalitySelector(cumulativeTemporality))
	if err != nil {
		return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
	}
	reader := sdkmetric.NewPeriodicReader(&countingExporter{next: exporter, stats: stats})
	return reader, nil
}
