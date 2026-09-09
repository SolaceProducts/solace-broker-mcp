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

// registerShutdownHooks actually registers what main() thinks it registers
// (SOL-153965).
//
// SOL-153884 wired the metrics meter-provider flush into the shared
// shutdown-hook registry (SOL-152449), and its own unit test proved the
// provider's Shutdown satisfies the hook contract — but not that main()
// actually calls Register. hooks.Registry held its hooks in an unexported
// slice with no accessor, so a registration could not be asserted; deleting
// the Register call in main.go left that unit test green. hooks.Registry now
// exposes Names() for exactly this, and the two call sites that used to sit
// next to each provider's own construction are consolidated into
// registerShutdownHooks (cmd/server/main.go), a seam this test can invoke
// directly instead of running the rest of main()'s startup.
package main

import (
	"context"
	"reflect"
	"testing"
	"time"

	sdkresource "go.opentelemetry.io/otel/sdk/resource"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/hooks"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/resource"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/tracing"
)

// TestRegisterShutdownHooks_BothProvidersRegistered builds both providers the
// way main() does — real construction, no fakes, matching
// resource_wiring_test.go's own pattern — and asserts both flushes land in
// the registry under the names main() has always used. tracing.New's
// TracingEnabled: true is safe without a real collector: the OTLP gRPC
// exporter it builds connects lazily, per every other test in
// internal/observability/tracing that already sets this flag.
func TestRegisterShutdownHooks_BothProvidersRegistered(t *testing.T) {
	res, err := resourceForTest(t)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}

	mp, err := metrics.New("v1.2.3", res, config.ObservabilityConfig{})
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mp.Shutdown(ctx)
	})

	tp, err := tracing.New(config.ObservabilityConfig{TracingEnabled: true, OTelSelfStatsIntervalS: 60}, mp.MeterProvider(), res)
	if err != nil {
		t.Fatalf("tracing.New() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})

	reg := hooks.NewRegistry()
	registerShutdownHooks(reg, mp, tp)

	want := []string{"metrics_provider", "tracer_provider"}
	if got := reg.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("registerShutdownHooks: Names() = %v, want %v", got, want)
	}
}

// TestRegisterShutdownHooks_NilProvidersRegisterNothing covers the two
// disabled-capability cases (OBS_METRICS_ENABLED / OBS_TRACING_ENABLED both
// off) main() hits in production far more often than the both-enabled case
// above: registerShutdownHooks must not panic on a nil provider, and must
// register nothing for one.
func TestRegisterShutdownHooks_NilProvidersRegisterNothing(t *testing.T) {
	reg := hooks.NewRegistry()
	registerShutdownHooks(reg, nil, nil)
	if got := reg.Names(); len(got) != 0 {
		t.Errorf("registerShutdownHooks(nil, nil): Names() = %v, want empty", got)
	}
}

// TestRegisterShutdownHooks_OnlyMetricsRegistered covers tracing disabled
// with metrics on — the shape a deployment that hasn't yet opted into an OTel
// collector has today.
func TestRegisterShutdownHooks_OnlyMetricsRegistered(t *testing.T) {
	res, err := resourceForTest(t)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	mp, err := metrics.New("v1.2.3", res, config.ObservabilityConfig{})
	if err != nil {
		t.Fatalf("metrics.New() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = mp.Shutdown(ctx)
	})

	reg := hooks.NewRegistry()
	registerShutdownHooks(reg, mp, nil)

	want := []string{"metrics_provider"}
	if got := reg.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("registerShutdownHooks(mp, nil): Names() = %v, want %v", got, want)
	}
}

// resourceForTest returns a minimal, real identity resource for a wiring
// test that doesn't care about its contents — sdkresource.Default() would
// also do, per metrics.New's and tracing.New's own doc comments, but
// resource.New exercises the same construction path main() uses.
func resourceForTest(t *testing.T) (*sdkresource.Resource, error) {
	t.Helper()
	return resource.New(config.ObservabilityConfig{
		ServiceName: "test-service",
	}, "v1.2.3")
}
