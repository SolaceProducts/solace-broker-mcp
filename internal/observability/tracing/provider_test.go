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
	"net"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// withRestoredGlobalTracer saves the current global OTel tracer provider and
// restores it on cleanup. New installs a tracer provider globally (by
// design — see provider.go), so any test that calls New must not leak that
// change into tests that run after it. Not parallel-safe with any other test
// that also touches the global tracer provider; callers must not call
// t.Parallel().
func withRestoredGlobalTracer(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
}

// TestNew_Disabled_ReturnsNilAndTouchesNothing pins the flag-off behavior: a
// nil *Provider, no error, and — the actual regression this guards — the
// global tracer provider left completely untouched. New deliberately does
// NOT call otel.SetTracerProvider on this path (see New's doc comment):
// asserting only that spans come back non-recording would pass even if New
// clobbered a real, previously-installed provider with a fresh no-op, since
// the OTel API's own global default is already non-recording. Pre-installing
// a real recording provider first, and checking it's still there after,
// is what actually catches that regression.
func TestNew_Disabled_ReturnsNilAndTouchesNothing(t *testing.T) {
	withRestoredGlobalTracer(t)

	preexisting := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = preexisting.Shutdown(context.Background()) })
	otel.SetTracerProvider(preexisting)

	p, err := New(config.ObservabilityConfig{TracingEnabled: false}, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if p != nil {
		t.Fatalf("New() Provider = %v, want nil when tracing is disabled", p)
	}

	if got := otel.GetTracerProvider(); got != preexisting {
		t.Fatalf("global tracer provider = %v, want the pre-existing provider untouched — New(disabled) must not call otel.SetTracerProvider", got)
	}

	_, span := preexisting.Tracer("test").Start(context.Background(), "op")
	defer span.End()
	if !span.IsRecording() {
		t.Error("span.IsRecording() = false, want true: the pre-existing (always-on) provider should still be the one in effect")
	}
}

// TestNew_Enabled_SamplesByDefault pins that a non-nil *Provider is returned
// and that the globally-installed tracer actually samples (the v1 default is
// ParentBased(AlwaysSample()) — sample everything — see New's doc comment).
func TestNew_Enabled_SamplesByDefault(t *testing.T) {
	withRestoredGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true, OTelSelfStatsIntervalS: 60}
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if p == nil {
		t.Fatal("New() Provider = nil, want non-nil when tracing is enabled")
	}

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	if !span.IsRecording() {
		t.Error("span.IsRecording() = false, want true: the v1 default sampler samples everything")
	}
	span.End()

	// Best-effort cleanup only: this test's job is sampling behavior, not
	// shutdown timing (see TestNew_Enabled_Shutdown_RespectsTimeoutBound for
	// that) — the span just ended above may still be queued for export, and
	// with no live collector Shutdown can legitimately return a
	// deadline-exceeded error while it tries.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.Shutdown(ctx)
}

// TestNew_Enabled_Shutdown_RespectsTimeoutBound is the AC-level proof that
// draining "must not hang the process if the OTLP endpoint is unreachable":
// with nothing queued (no span was ever ended), Shutdown has no export to
// attempt and must return promptly and cleanly rather than blocking on a
// connection to a collector that was never there.
func TestNew_Enabled_Shutdown_RespectsTimeoutBound(t *testing.T) {
	withRestoredGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true, OTelSelfStatsIntervalS: 60}
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	const bound = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()

	start := time.Now()
	err = p.Shutdown(ctx)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("Shutdown() error = %v, want nil (nothing was queued to flush)", err)
	}
	if elapsed >= bound {
		t.Errorf("Shutdown() took %s, at or past the %s bound passed via ctx — it blocked instead of returning promptly", elapsed, bound)
	}
}

// TestProvider_Shutdown_ActuallyDrainsTheTracerProvider is the direct proof
// that Provider.Shutdown — the exported method, not just the underlying
// sdktrace mechanism — really drains the tracer provider it owns. A queued
// span is created via the global tracer New installs, then p.Shutdown is
// called against a collector that accepts the connection but never
// responds, so the export attempt this test cares about can only have
// happened inside Shutdown's own final flush. A Shutdown that skips
// p.tp.Shutdown(ctx) (returning nil unconditionally) passes every other test
// in this package — including TestNew_Enabled_Shutdown_RespectsTimeoutBound,
// which queues nothing — but leaves every counter here at zero, and must
// fail this one.
func TestProvider_Shutdown_ActuallyDrainsTheTracerProvider(t *testing.T) {
	withRestoredGlobalTracer(t)

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer ln.Close()
	// t.Cleanup must be called from the test's own goroutine, not this
	// accept loop (flagged by review) — collect accepted conns under a mutex
	// and register one cleanup from here instead.
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+ln.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "50") // ms; keeps the test fast

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true, OTelSelfStatsIntervalS: 60}
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	span.End()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = p.Shutdown(ctx) // an error is expected here; what it recorded is the point, not its return value.

	_, _, exportTimeout, exportError, shutdown := p.stats.snapshot()
	if exportTimeout == 0 && exportError == 0 && shutdown == 0 {
		t.Fatal("Provider.Shutdown() recorded no export attempt at all — it must call the real tracer provider's Shutdown, which flushes the one queued span")
	}
}

// TestNew_MetricsEnabled_RegistersInstrumentsWithoutError is the wiring-level
// counterpart to TestExportStats_RegisterInstruments_MirrorsToMeter: New must
// not fail just because a real meter provider was supplied.
func TestNew_MetricsEnabled_RegistersInstrumentsWithoutError(t *testing.T) {
	withRestoredGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true}
	p, err := New(cfg, mp, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})
}

// TestNew_NonPositiveSelfStatsInterval_DoesNotPanic guards the defensive
// check in New: time.NewTicker panics on a non-positive duration, so a
// misconfigured (or test-constructed, bypassing config's own defaulting)
// OTelSelfStatsIntervalS must degrade to "reporting off", not crash the
// server at startup.
func TestNew_NonPositiveSelfStatsInterval_DoesNotPanic(t *testing.T) {
	withRestoredGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: false, OTelSelfStatsIntervalS: 0}
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown() error = %v, want nil", err)
	}
}

// TestProvider_Shutdown_NilReceiver confirms Shutdown is safe to call on the
// nil Provider New returns when tracing is disabled, even though every
// current caller already skips that call (cmd/server/main.go) — Shutdown
// itself must not make that skip load-bearing.
func TestProvider_Shutdown_NilReceiver(t *testing.T) {
	var p *Provider
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() on a nil Provider error = %v, want nil", err)
	}
}

// TestProvider_Shutdown_StopsSelfStatsEmitter confirms Shutdown halts the
// periodic INFO fallback before returning, so a stopped server does not keep
// logging on a timer after shutdown completes.
func TestProvider_Shutdown_StopsSelfStatsEmitter(t *testing.T) {
	withRestoredGlobalTracer(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	buf := captureLogs(t)

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: false, OTelSelfStatsIntervalS: 1}
	// A 1s configured interval would be slow to observe directly; instead,
	// confirm indirectly that no more otel_self_stats lines appear after
	// Shutdown returns, by racing a fast poll against a short window.
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}
	countAtShutdown := countOccurrences(buf, "otel_self_stats")

	time.Sleep(1200 * time.Millisecond)
	if got := countOccurrences(buf, "otel_self_stats"); got != countAtShutdown {
		t.Fatalf("self-stats emitter kept reporting after Shutdown: %d lines at shutdown, %d after waiting past its interval",
			countAtShutdown, got)
	}
}

// TestNew_Enabled_ExportTimeoutClassifiedAgainstRealExporter is the
// end-to-end proof for the countingExporter fix in stats.go: the real
// otlptracegrpc exporter's own timeout surfaces as a gRPC DeadlineExceeded
// status, not a bare context.DeadlineExceeded, so a naive errors.Is check
// alone misclassifies it as export_error and export_timeout never fires.
// Reverting that fix turns this test red. A local listener accepts the TCP
// connection but never completes anything at the gRPC layer, so the
// exporter's own OTEL_EXPORTER_OTLP_TIMEOUT budget is what expires — a
// stand-in for a collector that is up but wedged, the case the reason label
// exists to distinguish from "collector refused the connection".
func TestNew_Enabled_ExportTimeoutClassifiedAgainstRealExporter(t *testing.T) {
	withRestoredGlobalTracer(t)

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	defer ln.Close()
	// t.Cleanup must be called from the test's own goroutine, not this
	// accept loop (flagged by review) — collect accepted conns under a mutex
	// and register one cleanup from here instead.
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed at test end
			}
			// Accept and hold the connection open; never write a response,
			// so the exporter's own request eventually times out.
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+ln.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TIMEOUT", "300") // ms; keeps the test fast

	cfg := config.ObservabilityConfig{TracingEnabled: true, MetricsEnabled: true, OTelSelfStatsIntervalS: 60}
	p, err := New(cfg, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	_, span := otel.Tracer("test").Start(context.Background(), "op")
	span.End()

	// ForceFlush triggers an export attempt now instead of waiting out the
	// batch timer, and blocks until that attempt (and its 300ms timeout)
	// resolves.
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer flushCancel()
	_ = p.tp.ForceFlush(flushCtx) // error expected (the export itself failed); the assertion is on the reason it was classified under.

	exported, _, exportTimeout, exportError, _ := p.stats.snapshot()
	if exportTimeout != 1 || exportError != 0 {
		t.Fatalf("exported=%d exportTimeout=%d exportError=%d, want exportTimeout=1 exportError=0 — the gRPC DeadlineExceeded status wasn't classified as a timeout",
			exported, exportTimeout, exportError)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	_ = p.Shutdown(shutdownCtx)
}
