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

// Package metrics provides the server's Prometheus metrics: an OTel meter
// provider, a client_golang registry, the self-observation instruments, and
// the /metrics handler. The v1 default is OFF (door-closing policy) — operators
// opt in. Emitted records carry schema.MetricsSchemaVersion.
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otlprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// Provider is the single metrics root. It owns one client_golang registry, the
// OTel Prometheus exporter that renders into it, and the meter provider every
// instrument registers against.
type Provider struct {
	registry      *promclient.Registry
	exporter      *otlprom.Exporter
	meterProvider *sdkmetric.MeterProvider
	scrapeCounter metric.Int64Counter
	resource      *sdkresource.Resource

	// otlpStats is nil unless the OTLP reader was attached; Shutdown uses it
	// to record a shutdown-time drop, so it needs a reference past New's own
	// scope.
	otlpStats *otlpStats

	toolMetricsOnce sync.Once
	toolMetrics     *ToolMetrics
	toolMetricsErr  error

	sempMetricsOnce sync.Once
	sempMetrics     *SEMPMetrics
	sempMetricsErr  error

	securityMetricsOnce sync.Once
	securityMetrics     *SecurityMetrics
	securityMetricsErr  error

	brokerMetricsOnce sync.Once
	brokerMetrics     *BrokerMetrics
	brokerMetricsErr  error

	tokenExchangeBreakerMetricsOnce sync.Once
	tokenExchangeBreakerMetrics     *TokenExchangeBreakerMetrics
	tokenExchangeBreakerMetricsErr  error
}

// instrumentScope names the meter that owns the server's own instruments.
const instrumentScope = "github.com/SolaceProducts/solace-broker-mcp"

// New builds the metrics provider: one registry holding both the OTel
// instruments (via the exporter) and the client_golang runtime collectors.
// buildVersion labels the mcp_build_info gauge.
//
// res is the shared identity resource (SOL-152425, Story 34) — the same
// resource.Resource the tracer provider (Story 25) uses, constructed once by
// internal/observability/resource so metrics and traces cannot disagree
// about which instance emitted them. It surfaces on the scrape endpoint via
// target_info (WithoutTargetInfo is deliberately not passed to otlprom.New),
// which is why res being nil is a caller error, not a silently-tolerated
// "no identity" case — every real caller has one to pass (see
// cmd/server/main.go); tests that don't care about identity can pass
// sdkresource.Default() or any other non-nil resource.
//
// cfg.MetricsOTLPEnabled (SOL-152418, Story 46) attaches a second reader — an
// OTLP push exporter — to the SAME meter provider the Prometheus exporter
// reads from, so both egresses observe one instrument set and share res. A
// zero-value config.ObservabilityConfig{} (every test that doesn't care about
// OTLP) leaves the OTLP reader out entirely, byte-for-byte the pre-Story-46
// construction.
func New(buildVersion string, res *sdkresource.Resource, cfg config.ObservabilityConfig) (*Provider, error) {
	if res == nil {
		// Enforces the doc comment above: sdkmetric.WithResource(nil)
		// silently overrides the SDK's own resource.Default(), collapsing
		// target_info to zero labels with no error anywhere — exactly the
		// identity loss this parameter exists to prevent. Every real caller
		// has a resource to pass (cmd/server/main.go always builds one, at
		// worst falling back to sdkresource.Default() itself); a test that
		// doesn't care about identity should pass sdkresource.Default()
		// explicitly, not nil.
		return nil, fmt.Errorf("metrics: res must not be nil (pass sdkresource.Default() if identity doesn't matter)")
	}
	registry := promclient.NewRegistry()

	// Free Go-runtime and process numbers (memory, goroutines, FDs). These
	// register directly on the client_golang registry, never through the OTel
	// SDK pipeline below — which is what keeps them off the OTLP egress (Story
	// 46's own scope note): the OTLP reader only ever sees what passes through
	// meterProvider's Meter(...), and these two collectors never do.
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// The exporter is a metric Reader that renders into the registry.
	// WithoutScopeInfo drops the otel_scope_* labels.
	exporter, err := otlprom.New(
		otlprom.WithRegisterer(registry),
		otlprom.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, fmt.Errorf("create prometheus exporter: %w", err)
	}

	// readers always carries the Prometheus exporter; the OTLP reader joins it
	// conditionally. Both must be passed to sdkmetric.NewMeterProvider in one
	// call — the SDK does not support adding a reader after construction —
	// which is also why otlpStats (below) is built in two steps: the reader
	// needs to exist before the MeterProvider does, but its own counters can
	// only register against the MeterProvider once built.
	readers := []sdkmetric.Option{sdkmetric.WithReader(exporter)}

	var otlpStatsInstance *otlpStats
	if cfg.MetricsOTLPEnabled {
		otlpStatsInstance = newOTLPStats()
		otlpReader, err := newOTLPReader(context.Background(), otlpStatsInstance)
		if err != nil {
			// Verified against the real exporter (v1.46.0): a malformed
			// OTEL_EXPORTER_OTLP_ENDPOINT does NOT reach this branch —
			// otlpmetricgrpc.New defers endpoint validation to connection
			// time (via the same gRPC lazy-dial otlptracegrpc.New uses) and
			// logs a parse failure through the global channel instead of
			// returning one, which is exactly the leak oteldiag.Install
			// closes. This branch is defensive for a future SDK version or
			// option that does return synchronously; kept rather than
			// removed on that basis, disclosed as effectively untested
			// rather than pretended otherwise.
			//
			// No err.Error(): newOTLPReader wraps otlpmetricgrpc.New, whose
			// returned error can echo back OTEL_EXPORTER_OTLP_HEADERS or a
			// malformed endpoint URL — operator-supplied, unaudited text that
			// conventionally carries a collector auth token
			// (docs/internal/secure-logging-rules.md Rule 5), same reasoning
			// as tracing's identical otlptracegrpc.New guard. That covers
			// this err value; oteldiag.Install (called inside newOTLPReader,
			// before this call can fail) is the other half, closing the
			// SDK's own global-channel leak for the same construction.
			//
			// Service continuity over strict correctness here too: a
			// malformed OTLP endpoint override must not take down the
			// Prometheus scrape, which is the capability with no further
			// opt-in gate once metrics are on at all.
			// validateMetricsOTLPCoherence (config package) already caught
			// the "OTLP on, metrics off" case at config load; this is a
			// narrower, rarer construction failure the coherence check
			// cannot see.
			slog.Error("OTLP metrics egress unavailable: exporter build failed")
			otlpStatsInstance = nil
		} else {
			readers = append(readers, sdkmetric.WithReader(otlpReader))
		}
	}

	// Single instrument root: all metrics register against this. WithResource
	// is what makes target_info carry the shared identity attributes.
	meterProvider := sdkmetric.NewMeterProvider(append(readers, sdkmetric.WithResource(res))...)

	// meterProvider already owns the OTLP reader's ticker goroutine and gRPC
	// connection (when attached) from here on, so every error return between
	// this point and the end of the function needs to shut it down — not
	// just the next one, since a later addition to this function is exactly
	// as exposed as the two below it. One deferred cleanup covers all of
	// them by construction, rather than relying on each new return
	// remembering to repeat the call by hand.
	built := false
	defer func() {
		if !built {
			_ = meterProvider.Shutdown(context.Background())
		}
	}()

	if otlpStatsInstance != nil {
		if err := otlpStatsInstance.registerInstruments(meterProvider); err != nil {
			return nil, err
		}
	}

	p := &Provider{
		registry:      registry,
		exporter:      exporter,
		meterProvider: meterProvider,
		resource:      res,
		otlpStats:     otlpStatsInstance,
	}
	if err := p.registerInstruments(buildVersion); err != nil {
		return nil, err
	}
	built = true
	return p, nil
}

// registerInstruments creates the server's self-observation instruments:
// mcp_build_info and mcp_schema_version (constant-1 info gauges) and the
// mcp_metrics_scrape_total counter incremented in Handler.
func (p *Provider) registerInstruments(buildVersion string) error {
	meter := p.meterProvider.Meter(instrumentScope)

	// mcp_build_info: always 1, labelled with the build version.
	if _, err := meter.Int64ObservableGauge(
		"mcp.build.info",
		metric.WithDescription("Server build info; always 1, labelled with the version."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, metric.WithAttributes(attribute.String("version", buildVersion)))
			return nil
		}),
	); err != nil {
		return fmt.Errorf("register mcp_build_info: %w", err)
	}

	// mcp_schema_version: always 1, labelled with the output schema versions.
	if _, err := meter.Int64ObservableGauge(
		"mcp.schema_version",
		metric.WithDescription("Observability schema versions; always 1, labelled with each schema."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(1, metric.WithAttributes(
				attribute.String("metrics_schema", schema.MetricsSchemaVersion),
				attribute.String("audit_schema", schema.AuditSchemaVersion),
			))
			return nil
		}),
	); err != nil {
		return fmt.Errorf("register mcp_schema_version: %w", err)
	}

	// mcp_metrics_scrape_total: +1 per served scrape (see Handler).
	scrapeCounter, err := meter.Int64Counter(
		"mcp.metrics.scrape",
		metric.WithDescription("Number of /metrics scrapes served."),
	)
	if err != nil {
		return fmt.Errorf("register mcp_metrics_scrape_total: %w", err)
	}
	p.scrapeCounter = scrapeCounter
	return nil
}

// MeterProvider returns the single meter provider all instruments register against.
func (p *Provider) MeterProvider() *sdkmetric.MeterProvider {
	return p.meterProvider
}

// Resource returns the identity resource this provider was built with, so a
// caller (or a test) can confirm it matches the tracer provider's own
// resource (SOL-152425, Story 34's anti-drift guarantee).
func (p *Provider) Resource() *sdkresource.Resource {
	return p.resource
}

// Meter returns a named meter for creating instruments.
func (p *Provider) Meter(name string) metric.Meter {
	return p.meterProvider.Meter(name)
}

// ForceFlush flushes every reader attached at construction — the Prometheus
// exporter (a no-op; it is pulled, not pushed) and, when MetricsOTLPEnabled
// was set, the OTLP push reader (SOL-152418, Story 46). Exists so a caller
// (a test, or an operator-triggered pre-shutdown flush) can force an
// immediate OTLP push rather than waiting for the reader's own periodic
// interval.
func (p *Provider) ForceFlush(ctx context.Context) error {
	return p.meterProvider.ForceFlush(ctx)
}

// Handler serves the registry in Prometheus/OpenMetrics format for /metrics,
// counting each scrape via mcp_metrics_scrape_total. EnableOpenMetrics is
// required so a later change can emit exemplars.
func (p *Provider) Handler() http.Handler {
	base := promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Count before serving so this scrape's own render includes the increment.
		p.scrapeCounter.Add(r.Context(), 1)
		base.ServeHTTP(w, r)
	})
}

// ToolMetrics returns the per-tool RED instruments, registering them once on
// first call and returning the same set thereafter, so repeated calls don't
// re-register instruments against the meter.
func (p *Provider) ToolMetrics() (*ToolMetrics, error) {
	p.toolMetricsOnce.Do(func() {
		p.toolMetrics, p.toolMetricsErr = NewToolMetrics(p.Meter(instrumentScope))
	})
	return p.toolMetrics, p.toolMetricsErr
}

// SEMPMetrics returns the per-attempt SEMP instruments, registering them once
// on first call and returning the same set thereafter.
func (p *Provider) SEMPMetrics() (*SEMPMetrics, error) {
	p.sempMetricsOnce.Do(func() {
		p.sempMetrics, p.sempMetricsErr = NewSEMPMetrics(p.Meter(instrumentScope))
	})
	return p.sempMetrics, p.sempMetricsErr
}

// SecurityMetrics returns the security counters (SOL-152099), registering
// them once on first call — the same contract as ToolMetrics.
func (p *Provider) SecurityMetrics() (*SecurityMetrics, error) {
	p.securityMetricsOnce.Do(func() {
		p.securityMetrics, p.securityMetricsErr = NewSecurityMetrics(p.Meter(instrumentScope))
	})
	return p.securityMetrics, p.securityMetricsErr
}

// BrokerMetrics returns the broker reachability gauges, registering them once on first call.
func (p *Provider) BrokerMetrics(brokerStates func() map[string]health.BrokerSnapshot) (*BrokerMetrics, error) {
	p.brokerMetricsOnce.Do(func() {
		p.brokerMetrics, p.brokerMetricsErr = NewBrokerMetrics(p.Meter(instrumentScope), brokerStates)
	})
	return p.brokerMetrics, p.brokerMetricsErr
}

// TokenExchangeBreakerMetrics returns the process-wide token-exchange breaker
// state gauge, registering it once on first call.
func (p *Provider) TokenExchangeBreakerMetrics(
	snapshot func() (tokenexchange.BreakerSnapshot, bool),
) (*TokenExchangeBreakerMetrics, error) {
	p.tokenExchangeBreakerMetricsOnce.Do(func() {
		p.tokenExchangeBreakerMetrics, p.tokenExchangeBreakerMetricsErr =
			NewTokenExchangeBreakerMetrics(p.Meter(instrumentScope), snapshot)
	})
	return p.tokenExchangeBreakerMetrics, p.tokenExchangeBreakerMetricsErr
}

// Shutdown flushes and stops the meter provider. cmd/server registers it as a
// shutdown hook (SOL-153884).
//
// This flushes every reader attached at construction, the OTLP reader
// (SOL-152418, Story 46) included when MetricsOTLPEnabled was set — the SDK
// shuts down all of a MeterProvider's readers from one Shutdown call
// (go.opentelemetry.io/otel/sdk/metric@v1.46.0 config.go's readerSignals),
// sequentially, sharing ctx's one deadline. That is deliberately NOT a second,
// separately-registered shutdown hook: this method is already registered
// against the Story 48 registry as "metrics_provider"
// (cmd/server/main.go's registerShutdownHooks), and a second hook flushing
// the OTLP reader again would either race this call over the same reader
// (both are goroutines under hooks.Registry.RunAll) or, since the SDK's
// per-reader shutdown is idempotent, just return ErrReaderShutdown
// harmlessly on whichever hook loses the race — correct, but pure noise. One
// hook, one shared budget, no race, is the simpler and equally-bounded
// alternative.
//
// An incomplete OTLP flush logs a WARN rather than incrementing the dropped
// counter with reasonShutdown: this call has already torn down the reader
// that backs the scrape by the time it could record anything, so a counter
// touched here is unobservable, not merely delayed.
func (p *Provider) Shutdown(ctx context.Context) error {
	err := p.meterProvider.Shutdown(ctx)
	if err != nil && p.otlpStats != nil {
		// Logged, not counted: by the time this call returns, this same
		// Shutdown has already torn down the Prometheus reader along with
		// everything else, so a counter incremented here could never be
		// scraped — verified directly, not assumed. A log line is the one
		// channel still live at this point in the process's life.
		slog.Warn("OTLP metrics flush incomplete at shutdown")
	}
	return err
}
