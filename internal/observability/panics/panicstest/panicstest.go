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

// Package panicstest is shared test scaffolding for asserting against the
// mcp.panic.recovered counter (SOL-154037). internal/middleware/recovery and
// internal/tools both register against and exercise that counter, and each
// used to carry its own copy of this scaffolding; the copies had already
// diverged — the middleware and tools copies silently bucketed a data point
// missing its boundary attribute under "" instead of failing — which let the
// tests using them pass even if the instrument disappeared entirely.
// Centralizing it here means a future change to the metric name or the
// boundary label lands in one place, not two.
//
// internal/observability/panics itself — the package that owns the
// counter — keeps its own equivalent copy in panics_test.go rather than
// importing this one: that file is package panics (white-box, for direct
// access to the unexported counter), and this package imports panics, so
// importing it back would be a real cycle, not a style choice.
package panicstest

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/panics"
)

// InstallReader registers a fresh mcp.panic.recovered counter against a manual
// reader and returns the reader. The counter is process-level state (see the
// package doc on internal/observability/panics): every test that asserts
// counts must call this, because a test reusing another test's reader would
// see that test's totals. Callers must not run in parallel with another test
// that also registers, for the same reason.
//
// The provider is deliberately NOT shut down on cleanup: this package cannot
// unregister the process-level global, so shutting it down would leave the
// process pointing at a dead provider for the rest of the run. Leaving it
// live keeps a later stray increment harmless whatever order the tests
// execute in (-shuffle included). A ManualReader holds no goroutines or
// connections, so nothing leaks by not stopping it.
func InstallReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	if err := panics.Register(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))); err != nil {
		t.Fatalf("panics.Register() error = %v", err)
	}
	return reader
}

// Counts collects mcp.panic.recovered from reader and returns its value per
// boundary label. An absent metric yields an empty map — callers that need to
// tell "recorded zero" apart from "nothing exists" should also assert
// len(got) == 2, the documented cardinality bound, rather than checking only
// specific keys against zero.
//
// Fatals loudly if a data point is missing the boundary attribute, rather
// than silently bucketing it under "" — the shape of the divergence that let
// two of the three original copies of this helper pass vacuously.
func Counts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "mcp.panic.recovered" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("mcp.panic.recovered data = %#v, want a Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value("boundary")
				if !ok {
					t.Fatalf("data point %#v has no boundary attribute", dp)
				}
				got[v.AsString()] += dp.Value
			}
		}
	}
	return got
}
