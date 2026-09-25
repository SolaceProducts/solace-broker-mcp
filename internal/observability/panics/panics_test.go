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

// newTestReader installs a fresh counter against a manual reader and returns the
// reader. Each test that asserts counts must call it, because the registered
// instrument is package state: a test reusing a previous test's reader would see
// that test's totals.
//
// The cleanup goes through Unregister rather than touching counter directly, so
// this package's tests and panicstest's t.Cleanup hook exercise the same reset
// path (SOL-154365).
//
// This package cannot import internal/observability/panics/panicstest — that
// package imports panics, and this file is package panics (white-box, for
// direct access to the unexported counter below), so importing it back would be
// a real import cycle, not a style choice. panicstest.InstallReader /
// panicstest.Counts carry the equivalent logic for every other package's tests;
// keep this copy and that one in sync by hand.
func newTestReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(Unregister)
	if err := Register(mp); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	return reader
}

// countsByBoundary collects mcp.panic.recovered and returns its value per
// boundary label. An absent metric yields an empty map. See newTestReader on
// why this is not panicstest.Counts.
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

// TestRegister_SeedsBothSeriesAtZero is the alertability guarantee. An OTel
// counter renders nothing until it has a data point, and PromQL's increase()
// needs two samples in the window, so a series created BY the first panic makes
// that panic invisible to `increase(...) > 0` — the alert the schema prescribes,
// on exactly the event it exists for. Seeding at registration is what makes the
// first panic in a process's life fire the alert, and what makes a flat zero
// mean "nothing panicked" rather than "No data".
func TestRegister_SeedsBothSeriesAtZero(t *testing.T) {
	reader := newTestReader(t)

	got := countsByBoundary(t, reader)
	if len(got) != 2 {
		t.Fatalf("series after Register = %v, want both boundaries present at zero", got)
	}
	if got["http"] != 0 || got["tool"] != 0 {
		t.Errorf("seeded values = %v, want both 0", got)
	}
}

// TestRecovered_CountsPerBoundary is the AC-level proof that the two recovery
// nets land on one counter, separated by the boundary label rather than by two
// different metric names.
func TestRecovered_CountsPerBoundary(t *testing.T) {
	reader := newTestReader(t)
	ctx := context.Background()

	RecoveredHTTP(ctx)
	RecoveredTool(ctx)
	RecoveredTool(ctx)

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

	RecoveredHTTP(ctx)

	if got := countsByBoundary(t, reader); got["http"] != 1 {
		t.Errorf(`boundary="http" = %d, want 1 on a cancelled context (counts = %v)`, got["http"], got)
	}
}

// TestRecovered_NoOpBeforeRegister covers the no-metrics-egress case (neither
// OBS_METRICS_SCRAPE_ENABLED nor OBS_METRICS_OTLP_ENABLED):
// with no meter provider ever supplied, a recovery site still calls these on
// every panic and must neither panic nor record. The recovery nets themselves
// are unconditional, so this is the normal state with metrics off.
func TestRecovered_NoOpBeforeRegister(t *testing.T) {
	Unregister()
	t.Cleanup(Unregister)

	RecoveredHTTP(context.Background())
	RecoveredTool(context.Background())
}

// TestRegister_NilMeterProviderIsRejected pins that Register refuses a nil
// provider rather than quietly leaving the counter unregistered. The only way to
// reach it is calling Register with metrics disabled, which is the caller's gate
// to get right.
func TestRegister_NilMeterProviderIsRejected(t *testing.T) {
	Unregister()
	t.Cleanup(Unregister)

	if err := Register(nil); err == nil {
		t.Fatal("Register(nil) error = nil, want an error")
	}
	if counter.Load() != nil {
		t.Error("Register(nil) installed a counter, want none")
	}
}

// TestUnregister_ReturnsCounterToNoOp pins the two contracts panicstest depends
// on (SOL-154365): after Unregister the record functions are no-ops again, so a
// registration cannot outlive the test that made it, and IsRegistered tracks
// that transition in both directions. The reader stays live throughout, so a
// write that still reached the instrument would be visible here rather than
// silently discarded.
func TestUnregister_ReturnsCounterToNoOp(t *testing.T) {
	reader := newTestReader(t)

	RecoveredHTTP(context.Background())
	if got := countsByBoundary(t, reader)["http"]; got != 1 {
		t.Fatalf("http count before Unregister = %d, want 1", got)
	}

	if !IsRegistered() {
		t.Error("IsRegistered() = false while an instrument is installed, want true")
	}

	Unregister()
	if counter.Load() != nil {
		t.Fatal("Unregister() left an instrument installed, want none")
	}
	// panicstest.Register's nested-registration guard is built on this
	// returning false once the cleanup has run; if it ever reported stale
	// state the guard would reject every legitimate sequential registration.
	if IsRegistered() {
		t.Error("IsRegistered() = true after Unregister(), want false")
	}

	RecoveredHTTP(context.Background())
	if got := countsByBoundary(t, reader)["http"]; got != 1 {
		t.Errorf("http count after Unregister = %d, want 1 (the write must not land)", got)
	}
}
