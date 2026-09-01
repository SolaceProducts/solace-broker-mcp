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

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
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
