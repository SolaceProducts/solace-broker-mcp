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

package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/hooks"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/version"
)

func metricsConfig(bindAddr string) *config.ServerConfig {
	return &config.ServerConfig{
		Observability: config.ObservabilityConfig{MetricsBindAddress: bindAddr},
	}
}

// A bind failure must surface on /readyz as "metrics_endpoint: <err>". The
// provider is built separately now (before the tool manager); serveMetricsEndpoint
// only starts the listener.
func TestServeMetricsEndpoint_BindFailureIsUnready(t *testing.T) {
	// Occupy an address so the metrics listener cannot bind to it.
	var lc net.ListenConfig
	occupied, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not occupy a port: %v", err)
	}
	defer occupied.Close()

	provider, err := metrics.New(version.Version(), sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	defer provider.Shutdown(context.Background())

	readiness := health.NewReadinessState()
	serveMetricsEndpoint(metricsConfig(occupied.Addr().String()), readiness, provider)

	readiness.SetInitialized()
	_, ready, reason := readiness.Evaluate()
	if ready {
		t.Error("expected /readyz to be unready after a bind failure")
	}
	if !strings.Contains(reason, "metrics_endpoint") {
		t.Errorf("expected the reason to name metrics_endpoint, got %q", reason)
	}
}

// A successful bind must leave /readyz ready.
func TestServeMetricsEndpoint_SuccessIsReady(t *testing.T) {
	provider, err := metrics.New(version.Version(), sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	defer provider.Shutdown(context.Background())

	readiness := health.NewReadinessState()
	serveMetricsEndpoint(metricsConfig("127.0.0.1:0"), readiness, provider)

	readiness.SetInitialized()
	status, ready, reason := readiness.Evaluate()
	if !ready {
		t.Errorf("expected /readyz to be ready, got status=%q reason=%q", status, reason)
	}
}

// The provider's Shutdown must satisfy the shutdown-hook contract (SOL-153884):
// registrable on a hooks.Registry and run cleanly, well within RunAll's budget.
func TestMetricsProvider_ShutdownHook(t *testing.T) {
	provider, err := metrics.New(version.Version(), sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}

	reg := hooks.NewRegistry()
	reg.Register("metrics_provider", provider.Shutdown)

	// Bound the context like cmd/server does, so a hook that ever blocks fails
	// at the deadline instead of hanging CI. RunAll honours the deadline only if
	// the caller sets one.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg.RunAll(ctx)
	if ctx.Err() != nil {
		t.Fatalf("RunAll exceeded its budget: %v", ctx.Err())
	}

	// The hook ran provider.Shutdown, so the meter provider is already stopped:
	// a second Shutdown now errors. This proves Shutdown satisfies the hook
	// contract, not that main() wires it in (that is exercised end-to-end).
	if err := provider.Shutdown(context.Background()); err == nil {
		t.Error("expected the provider to be already shut down by the hook, got nil")
	}
}

// wireMetricsEndpoint (SOL-154607) is main()'s one decision about the scrape
// listener; the three tests below are the three things it can leave on
// /readyz. The provider passed in is a scrape-built one throughout — the
// decision reads cfg, not the provider, and a real OTLP-only provider would
// need a collector to shut down against — so what these pin is the gate,
// which is exactly the part of main() this story changed.

// OTLP-only must bind nothing: metrics_bind_address points at a port that is
// already taken, and /readyz stays ready because nothing ever tried to open
// it. With the scrape flag on, the same setup goes unready
// (TestWireMetricsEndpoint_ScrapeOn_Binds), so the pair pins that the flag,
// not luck, is what keeps the port closed.
func TestWireMetricsEndpoint_OTLPOnly_BindsNothing(t *testing.T) {
	var lc net.ListenConfig
	occupied, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not occupy a port: %v", err)
	}
	defer occupied.Close()

	provider, err := metrics.New(version.Version(), sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	defer provider.Shutdown(context.Background())

	cfg := &config.ServerConfig{Observability: config.ObservabilityConfig{
		MetricsOTLPEnabled: true,
		MetricsBindAddress: occupied.Addr().String(),
	}}
	readiness := health.NewReadinessState()
	wireMetricsEndpoint(cfg, readiness, provider, nil)

	readiness.SetInitialized()
	status, ready, reason := readiness.Evaluate()
	if !ready {
		t.Fatalf("OTLP-only wiring left /readyz unready: status=%q reason=%q (did it try to bind?)", status, reason)
	}
}

// The scrape flag on routes through serveMetricsEndpoint, so an occupied bind
// address surfaces on /readyz exactly as it did before this story.
func TestWireMetricsEndpoint_ScrapeOn_Binds(t *testing.T) {
	var lc net.ListenConfig
	occupied, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not occupy a port: %v", err)
	}
	defer occupied.Close()

	provider, err := metrics.New(version.Version(), sdkresource.Default(), config.ObservabilityConfig{MetricsScrapeEnabled: true})
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	defer provider.Shutdown(context.Background())

	cfg := &config.ServerConfig{Observability: config.ObservabilityConfig{
		MetricsScrapeEnabled: true,
		MetricsBindAddress:   occupied.Addr().String(),
	}}
	readiness := health.NewReadinessState()
	wireMetricsEndpoint(cfg, readiness, provider, nil)

	readiness.SetInitialized()
	_, ready, reason := readiness.Evaluate()
	if ready || !strings.Contains(reason, "metrics_endpoint") {
		t.Fatalf("scrape wiring with an occupied port: ready=%v reason=%q, want unready naming metrics_endpoint", ready, reason)
	}
}

// A provider build failure is reported whichever egress asked for the
// provider: an OTLP-only deployment whose exporter failed to build has no
// listener, but it still has nothing exporting, and /readyz is where that has
// always surfaced.
func TestWireMetricsEndpoint_BuildErr_Unready(t *testing.T) {
	cfg := &config.ServerConfig{Observability: config.ObservabilityConfig{MetricsOTLPEnabled: true}}
	readiness := health.NewReadinessState()
	wireMetricsEndpoint(cfg, readiness, nil, errors.New("boom"))

	readiness.SetInitialized()
	_, ready, reason := readiness.Evaluate()
	if ready || !strings.Contains(reason, "metrics_endpoint") {
		t.Fatalf("build error: ready=%v reason=%q, want unready naming metrics_endpoint", ready, reason)
	}
}
