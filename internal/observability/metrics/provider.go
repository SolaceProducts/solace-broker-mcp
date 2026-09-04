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
	"net/http"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otlprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
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
func New(buildVersion string, res *sdkresource.Resource) (*Provider, error) {
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

	// Free Go-runtime and process numbers (memory, goroutines, FDs)
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

	// Single instrument root: all metrics register against this. WithResource
	// is what makes target_info carry the shared identity attributes.
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter), sdkmetric.WithResource(res))

	p := &Provider{
		registry:      registry,
		exporter:      exporter,
		meterProvider: meterProvider,
		resource:      res,
	}
	if err := p.registerInstruments(buildVersion); err != nil {
		return nil, err
	}
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

func (p *Provider) ToolMetrics() (*ToolMetrics, error) {
	return NewToolMetrics(p.Meter(instrumentScope))
}

// Shutdown flushes and stops the meter provider. cmd/server registers it as a
// shutdown hook (SOL-153884).
func (p *Provider) Shutdown(ctx context.Context) error {
	return p.meterProvider.Shutdown(ctx)
}
