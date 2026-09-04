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

package panics

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// newTestReader installs a fresh counter against a manual reader and returns
// the reader. Each test that asserts counts must call it, because the
// registered instrument is package state: a test that reused a previous test's
// reader would read that test's totals.
func newTestReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	t.Cleanup(func() { counter.Store(nil) })
	if err := Register(mp); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return reader
}

// countsByBoundary collects mcp.panic.recovered and returns its value per
// boundary label. An absent metric yields an empty map, which is how a
// never-incremented counter reads.
func countsByBoundary(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
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

// TestRecovered_CountsPerBoundary is the AC-level proof that the two recovery
// nets land on one counter, separated by the boundary label rather than by two
// different metric names.
func TestRecovered_CountsPerBoundary(t *testing.T) {
	reader := newTestReader(t)
	ctx := context.Background()

	Recovered(ctx, BoundaryHTTP)
	Recovered(ctx, BoundaryTool)
	Recovered(ctx, BoundaryTool)

	got := countsByBoundary(t, reader)
	if len(got) != 2 {
		t.Fatalf("boundary series = %v, want exactly 2 (the documented cardinality bound)", got)
	}
	if got["http"] != 1 {
		t.Errorf(`boundary="http" = %d, want 1`, got["http"])
	}
	if got["tool"] != 2 {
		t.Errorf(`boundary="tool" = %d, want 2`, got["tool"])
	}
}

// TestRecovered_RecordsOnCancelledContext pins that a panic still counts when
// the request's own context is already done. Both call sites pass the request
// context — recovery.HTTPMiddleware passes r.Context(), withRecovery passes the
// handler ctx — and a panic under load or on a disconnecting client is exactly
// the case where that context is cancelled. If the SDK dropped those records the
// counter would silently miss the panics that matter most.
func TestRecovered_RecordsOnCancelledContext(t *testing.T) {
	reader := newTestReader(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	Recovered(ctx, BoundaryHTTP)

	if got := countsByBoundary(t, reader); got["http"] != 1 {
		t.Errorf(`boundary="http" = %d, want 1 on a cancelled context (counts = %v)`, got["http"], got)
	}
}

// TestRecovered_ZeroBoundaryIsDropped covers the one invalid value the type
// system cannot rule out. Boundary's field is unexported, so no other package
// can name a third boundary, but `var b Boundary` still compiles; that must not
// record an empty label and create a third series.
func TestRecovered_ZeroBoundaryIsDropped(t *testing.T) {
	reader := newTestReader(t)

	var unset Boundary
	Recovered(context.Background(), unset)

	if got := countsByBoundary(t, reader); len(got) != 0 {
		t.Errorf("counts = %v, want none (the zero Boundary must not widen cardinality)", got)
	}
}

// TestRecovered_NoOpBeforeRegister covers the OBS_METRICS_ENABLED=false case:
// with no meter provider ever supplied, a recovery site still calls Recovered
// on every panic and must neither panic nor record. The recovery nets
// themselves are unconditional, so this is the normal state with metrics off.
func TestRecovered_NoOpBeforeRegister(t *testing.T) {
	counter.Store(nil)
	t.Cleanup(func() { counter.Store(nil) })

	Recovered(context.Background(), BoundaryHTTP)
	Recovered(context.Background(), BoundaryTool)
}

// TestRegister_NilMeterProviderIsRejected pins that Register refuses a nil
// provider rather than quietly leaving the counter unregistered. The only way
// to reach it is calling Register with metrics disabled, which is the caller's
// gate to get right.
func TestRegister_NilMeterProviderIsRejected(t *testing.T) {
	if err := Register(nil); err == nil {
		t.Fatal("Register(nil) error = nil, want an error")
	}
	if counter.Load() != nil {
		t.Error("Register(nil) installed a counter, want none")
	}
}
