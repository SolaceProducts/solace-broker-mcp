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
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// Provider owns the tracer provider New installs globally, its
// self-observation counters, and (when running) the periodic INFO emitter.
type Provider struct {
	tp          *sdktrace.TracerProvider
	stats       *exportStats
	stopEmitter func()
	resource    *sdkresource.Resource
}

// Resource returns the identity resource this provider was built with, so a
// caller (or a test) can confirm it matches the metrics provider's own
// resource (SOL-152425, Story 34's anti-drift guarantee). A nil receiver
// (the flag-off return from New) returns nil.
func (p *Provider) Resource() *sdkresource.Resource {
	if p == nil {
		return nil
	}
	return p.resource
}

// New builds the tracer provider and installs it as the global OTel tracer
// provider, so later stories (26, 27, 40, 47, 50) create spans via
// otel.Tracer(name) with no wiring of their own. When cfg.TracingEnabled is
// false, New does nothing at all and returns a nil *Provider — there is
// nothing to flush on shutdown, so callers must skip hook registration on a
// nil return (see cmd/server/main.go). It deliberately does NOT install an
// explicit no-op tracer provider: the OTel API's own global default is
// already non-recording, and otel.SetTracerProvider binds the SDK's
// one-shot delegate the first time it's called — installing a throwaway
// no-op here would burn that binding for no observable benefit, and would
// break a future package-scoped `var tracer = otel.Tracer(name)` (the
// idiomatic OTel form stories 26/27/40/47/50 are expected to use) if such a
// handle happened to be created before this runs. Confirmed by review.
//
// OTLP endpoint, TLS, and sampling all come from the standard OTel SDK
// environment-variable contract (OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_EXPORTER_OTLP_INSECURE, OTEL_TRACES_SAMPLER /
// OTEL_TRACES_SAMPLER_ARG) with no code in this binary: otlptracegrpc.New
// reads the first two, and sdktrace.NewTracerProvider reads the sampler pair
// when WithSampler is not passed AND OTEL_TRACES_SAMPLER is set —
// OTEL_TRACES_SAMPLER_ARG alone, with no OTEL_TRACES_SAMPLER, is not read at
// all (confirmed by review; an earlier revision of this comment overstated
// the equivalence). With neither set, the SDK's actual default applies:
// ParentBased(AlwaysSample()) — sample everything, which is the v1 pilot
// intent, but an operator who sets only OTEL_TRACES_SAMPLER_ARG expecting a
// ratio sampler gets 100% sampling, not the ratio. Documented in
// docs/observability.md's runbook entry for this flag.
//
// meterProvider registers the mcp_otel_spans_exported_total and
// mcp_otel_spans_dropped_total{reason} counters when non-nil; pass nil when
// there is no metrics provider to register against (metrics off, or the
// metrics endpoint failed to start). Either way the counters keep counting
// in-process — nil only means they are not exposed on /metrics — and the
// periodic INFO fallback (Decision #12) starts automatically whenever
// meterProvider is nil, which is the actual "nothing else surfaces this"
// condition; gating it on cfg.MetricsEnabled instead would leave both
// surfaces dark if metrics is configured on but its provider failed to
// build (confirmed by review — see cmd/server/main.go's metricsProvider
// wiring).
//
// res is the shared identity resource (SOL-152425, Story 34) — the SAME
// resource.Resource the metrics meter provider (Story 14) uses, constructed
// once by internal/observability/resource. Passing two independently built
// resources here and in metrics.New would defeat the anti-drift guarantee
// this story exists to provide; see cmd/server/main.go for where it's built
// and threaded to both.
func New(cfg config.ObservabilityConfig, meterProvider *sdkmetric.MeterProvider, res *sdkresource.Resource) (*Provider, error) {
	if !cfg.TracingEnabled {
		return nil, nil
	}
	if res == nil {
		// Matches metrics.New's own guard (internal/observability/metrics):
		// sdktrace.WithResource(nil) silently overrides the SDK's own
		// resource.Default(), collapsing every exported span's identity to
		// zero attributes with no error anywhere — exactly what this
		// parameter exists to prevent (flagged by review). Placed after the
		// flag-off early return so the disabled-tracing (nil, nil) contract
		// is unaffected by callers that never pass a resource in that case.
		return nil, fmt.Errorf("tracing: res must not be nil (pass sdkresource.Default() if identity doesn't matter)")
	}

	// Before anything else: otlptracegrpc.New's own env-var parsing can log
	// through the OTel SDK's global error/log channel, which by default
	// prints straight to stderr, unrouted and unredacted (flagged by
	// review — see installOTelDiagnostics).
	installOTelDiagnostics()

	stats := newExportStats()
	if meterProvider != nil {
		if err := stats.registerInstruments(meterProvider); err != nil {
			return nil, fmt.Errorf("register otel self-observation instruments: %w", err)
		}
	}

	baseExporter, err := otlptracegrpc.New(context.Background())
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(&countingExporter{next: baseExporter, stats: stats}),
	)
	otel.SetTracerProvider(tp)

	p := &Provider{tp: tp, stats: stats, stopEmitter: func() {}, resource: res}
	if meterProvider == nil {
		interval := time.Duration(cfg.OTelSelfStatsIntervalS) * time.Second
		if interval <= 0 {
			// Guards time.NewTicker, which panics on a non-positive duration.
			// Should not happen through normal config loading (Story 1's
			// applyObservabilityDefaults re-defaults a non-positive
			// OTelSelfStatsIntervalS to 60s) — treated as "reporting off"
			// rather than a background-goroutine crash, same defensiveness as
			// health.StartOccupancyReporter.
			slog.Warn("otel self-stats interval is non-positive; periodic fallback disabled",
				slog.Int("otel_self_stats_interval_s", cfg.OTelSelfStatsIntervalS))
		} else {
			p.stopEmitter = startSelfStatsEmitter(interval, stats)
		}
	}
	return p, nil
}

// Shutdown stops the periodic self-stats emitter (if running) and drains the
// OTLP exporter, bounded by ctx's deadline. cmd/server registers this as a
// shutdown hook against Story 48's registry (SOL-152449) rather than editing
// drainAndShutdown directly, so this story needs no change there.
//
// A nil receiver (the flag-off return from New) is a no-op — callers that
// skip hook registration on a nil Provider never reach here, but Shutdown is
// safe to call anyway rather than making that skip load-bearing.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.stopEmitter()
	if err := p.tp.Shutdown(ctx); err != nil {
		// One event, not a per-span count: the SDK's Shutdown does not report
		// how many spans it failed to flush, only that the deadline was hit.
		// Documented as an intentional exception to the per-span accounting
		// every other reason uses (see stats.go).
		p.stats.recordDropped(ctx, reasonShutdown, 1)
		return fmt.Errorf("shutdown tracer provider: %w", err)
	}
	return nil
}
