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
	"net"
	"strings"
	"testing"
	"time"

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

// A bind failure must surface on /readyz as "metrics_endpoint: <err>" and the
// provider must not be returned, since the endpoint is not serving.
func TestStartMetricsEndpoint_BindFailureIsUnready(t *testing.T) {
	// Occupy an address so the metrics listener cannot bind to it.
	var lc net.ListenConfig
	occupied, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not occupy a port: %v", err)
	}
	defer occupied.Close()

	readiness := health.NewReadinessState()
	provider := startMetricsEndpoint(metricsConfig(occupied.Addr().String()), readiness)
	if provider != nil {
		t.Error("expected a nil provider when the listener fails to bind")
	}

	readiness.SetInitialized()
	_, ready, reason := readiness.Evaluate()
	if ready {
		t.Error("expected /readyz to be unready after a bind failure")
	}
	if !strings.Contains(reason, "metrics_endpoint") {
		t.Errorf("expected the reason to name metrics_endpoint, got %q", reason)
	}
}

// A successful bind must return the provider and leave /readyz ready.
func TestStartMetricsEndpoint_SuccessIsReady(t *testing.T) {
	readiness := health.NewReadinessState()
	provider := startMetricsEndpoint(metricsConfig("127.0.0.1:0"), readiness)
	if provider == nil {
		t.Fatal("expected a non-nil provider on a successful bind")
	}
	defer provider.Shutdown(context.Background())

	readiness.SetInitialized()
	status, ready, reason := readiness.Evaluate()
	if !ready {
		t.Errorf("expected /readyz to be ready, got status=%q reason=%q", status, reason)
	}
}

// The provider's Shutdown must satisfy the shutdown-hook contract (SOL-153884):
// registrable on a hooks.Registry and run cleanly, well within RunAll's budget.
func TestMetricsProvider_ShutdownHook(t *testing.T) {
	provider, err := metrics.New(version.Version())
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
