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

package metrics

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	collectormetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/grpc"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// fakeOTLPCollector is a minimal in-process OTLP metrics collector for tests.
// In blackHole mode it never responds, modelling an unreachable or hung
// collector (AC: "an unreachable collector must not degrade service").
// Otherwise it records every request so a test can inspect what arrived.
type fakeOTLPCollector struct {
	collectormetricspb.UnimplementedMetricsServiceServer

	blackHole bool

	mu       sync.Mutex
	requests []*collectormetricspb.ExportMetricsServiceRequest
}

func (f *fakeOTLPCollector) Export(ctx context.Context, req *collectormetricspb.ExportMetricsServiceRequest) (*collectormetricspb.ExportMetricsServiceResponse, error) {
	if f.blackHole {
		<-ctx.Done() // never respond; only returns when the caller gives up
		return nil, ctx.Err()
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	return &collectormetricspb.ExportMetricsServiceResponse{}, nil
}

func (f *fakeOTLPCollector) received() []*collectormetricspb.ExportMetricsServiceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*collectormetricspb.ExportMetricsServiceRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// startFakeOTLPCollector starts f on a local gRPC listener and points every
// standard OTLP env var at it, insecure (no TLS — a real collector's own
// concern, out of scope for this fixture). Cleanup stops the server and
// restores the environment (t.Setenv already does the latter).
func startFakeOTLPCollector(t *testing.T, f *fakeOTLPCollector) {
	t.Helper()
	lis, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := grpc.NewServer()
	collectormetricspb.RegisterMetricsServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://"+lis.Addr().String())
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
}

// pointAtBlackHole points every standard OTLP env var at an address nothing
// listens on, so a connection attempt hangs (rather than failing fast with
// connection-refused) — the closer analogue of a collector behind a
// misconfigured NetworkPolicy egress rule, the AC's own named first-deployment
// failure mode. 192.0.2.0/24 (TEST-NET-1, RFC 5737) is reserved for
// documentation and never routes.
func pointAtBlackHole(t *testing.T) {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://192.0.2.1:4317")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
}

// sumValue returns the summed int64 value of every data point across every
// occurrence of metric name in reqs — a test-only reader for the OTLP wire
// format, the counterpart to testutil.GatherAndCompare on the Prometheus
// side. name is the raw OTel instrument name (dotted, e.g.
// "mcp.tool.invocation") as it appears on the wire, NOT the
// Prometheus-rendered name (ADR-008's underscore/suffix rendering is a
// property of the Prometheus exporter only).
func sumValue(t *testing.T, reqs []*collectormetricspb.ExportMetricsServiceRequest, name string) (int64, bool) {
	t.Helper()
	var total int64
	found := false
	for _, req := range reqs {
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					if m.GetName() != name {
						continue
					}
					sum := m.GetSum()
					if sum == nil {
						continue
					}
					for _, dp := range sum.GetDataPoints() {
						total += dp.GetAsInt()
						found = true
					}
				}
			}
		}
	}
	return total, found
}

// sumTemporality returns the aggregation temporality of the first Sum-typed
// metric named name found in reqs, so a test can inspect what actually went
// on the wire rather than just its value.
func sumTemporality(reqs []*collectormetricspb.ExportMetricsServiceRequest, name string) (metricspb.AggregationTemporality, bool) {
	for _, req := range reqs {
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					if m.GetName() != name {
						continue
					}
					if sum := m.GetSum(); sum != nil {
						return sum.GetAggregationTemporality(), true
					}
				}
			}
		}
	}
	return 0, false
}

// findResourceAttr returns the string value of key on the Resource attached
// to the first ResourceMetrics carrying any data, or ("", false).
func findResourceAttr(reqs []*collectormetricspb.ExportMetricsServiceRequest, key string) (string, bool) {
	for _, req := range reqs {
		for _, rm := range req.GetResourceMetrics() {
			res := rm.GetResource()
			if res == nil {
				continue
			}
			for _, kv := range res.GetAttributes() {
				if kv.GetKey() == key {
					return kv.GetValue().GetStringValue(), true
				}
			}
		}
	}
	return "", false
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestOTLP_DualEgressAgreement is the ticket's first acceptance criterion:
// with both egresses on, the same instrument feeds both, and their reported
// counter values agree.
func TestOTLP_DualEgressAgreement(t *testing.T) {
	collector := &fakeOTLPCollector{}
	startFakeOTLPCollector(t, collector)

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}
	const calls = 3
	for range calls {
		tm.Record(context.Background(), "test-tool", "test-broker", OutcomeSuccess, "", time.Millisecond)
	}

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}

	// Prometheus side: scrape and parse mcp_tool_invocation_total's value.
	scrapeBody := scrapePlainText(t, p)
	var promValue int64 = -1
	for _, line := range strings.Split(scrapeBody, "\n") {
		if strings.HasPrefix(line, "mcp_tool_invocation_total{") {
			fields := strings.Fields(line)
			v, convErr := strconv.ParseInt(fields[len(fields)-1], 10, 64)
			if convErr != nil {
				t.Fatalf("parsing scrape line %q: %v", line, convErr)
			}
			promValue = v
		}
	}
	if promValue != calls {
		t.Fatalf("Prometheus mcp_tool_invocation_total = %d, want %d", promValue, calls)
	}

	// OTLP side: wait for the push (ForceFlush triggers it, but delivery is
	// still a network round trip on a goroutine) and parse the same counter's
	// raw instrument name.
	if !waitFor(t, 2*time.Second, func() bool { return len(collector.received()) > 0 }) {
		t.Fatal("fake OTLP collector received nothing within 2s of ForceFlush")
	}
	otlpValue, found := sumValue(t, collector.received(), "mcp.tool.invocation")
	if !found {
		t.Fatal("mcp.tool.invocation not found in any OTLP export")
	}
	if otlpValue != calls {
		t.Fatalf("OTLP mcp.tool.invocation = %d, want %d", otlpValue, calls)
	}

	if promValue != otlpValue {
		t.Errorf("Prometheus (%d) and OTLP (%d) disagree on the same counter", promValue, otlpValue)
	}
}

// TestOTLP_CumulativeTemporality pins the AC's explicit requirement: every
// instrument kind reports Cumulative, asserted directly rather than inferred
// from a default that a future SDK version, or a customer's own
// OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE, could change.
func TestOTLP_CumulativeTemporality(t *testing.T) {
	for _, kind := range []sdkmetric.InstrumentKind{
		sdkmetric.InstrumentKindCounter,
		sdkmetric.InstrumentKindUpDownCounter,
		sdkmetric.InstrumentKindHistogram,
		sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter,
		sdkmetric.InstrumentKindObservableGauge,
	} {
		if got := cumulativeTemporality(kind); got != metricdata.CumulativeTemporality {
			t.Errorf("cumulativeTemporality(%s) = %v, want CumulativeTemporality", kind, got)
		}
	}
}

// TestOTLP_CumulativeTemporality_WinsOverEnvOverride pins what
// TestOTLP_CumulativeTemporality above cannot: that cumulativeTemporality is
// actually wired into the exporter via WithTemporalitySelector, not just
// defined and left unused. Removing that option from newOTLPReader leaves
// the test above green, since it calls the free function directly — proven
// by mutation before this test was added. Asserting on the wire, with the
// one environment variable that would otherwise flip it, is what closes
// that gap: OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta is a
// real, live setting a customer might set cluster-wide for an unrelated
// OTLP-native app sharing this process's environment.
func TestOTLP_CumulativeTemporality_WinsOverEnvOverride(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta")
	collector := &fakeOTLPCollector{}
	startFakeOTLPCollector(t, collector)

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}
	tm.Record(context.Background(), "test-tool", "test-broker", OutcomeSuccess, "", time.Millisecond)

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(collector.received()) > 0 }) {
		t.Fatal("fake OTLP collector received nothing within 2s of ForceFlush")
	}

	got, found := sumTemporality(collector.received(), "mcp.tool.invocation")
	if !found {
		t.Fatal("mcp.tool.invocation not found in any OTLP export")
	}
	if got != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Errorf("aggregation temporality = %v, want CUMULATIVE even with the delta preference env var set"+
			" (delta breaks Prometheus's OTLP-receiver interop, the exact case this override exists for)", got)
	}
}

// TestOTLP_FlagOff_NoOTLPInstrumentsRegistered proves the default-off
// contract: with MetricsOTLPEnabled false, the OTLP self-observation
// counters never register at all, so they never appear on a scrape — the
// clearest external evidence available that no OTLP reader (and so no
// exporter, no connection attempt) was constructed.
func TestOTLP_FlagOff_NoOTLPInstrumentsRegistered(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	body := scrapePlainText(t, p)
	for _, name := range []string{"mcp_otel_metrics_exported_total", "mcp_otel_metrics_dropped_total"} {
		if strings.Contains(body, name) {
			t.Errorf("scrape contains %s with OBS_METRICS_OTLP_ENABLED off; want absent", name)
		}
	}
}

// TestOTLP_UnreachableCollector_ScrapeStaysUpAndDropsAreVisible covers two
// AC bullets at once: self-observation counters are readable on /metrics
// while the OTLP endpoint is unreachable, and doing so does not itself wedge
// the process.
//
// OTEL_METRIC_EXPORT_TIMEOUT is shortened so the reader's OWN internal
// collect-and-export timeout (30s default) resolves quickly: PeriodicReader's
// ForceFlush only waits for a background goroutine's signal — it does not run
// collectAndExport with the caller's ctx directly — so bounding this test's
// own ForceFlush call does not bound how soon that background attempt
// actually fails and records the drop. Shrinking the reader's real timeout is
// the correct lever, not a longer test wait: it is the same env var an
// operator would use to get a faster failure signal from a real unreachable
// collector.
func TestOTLP_UnreachableCollector_ScrapeStaysUpAndDropsAreVisible(t *testing.T) {
	pointAtBlackHole(t)
	t.Setenv("OTEL_METRIC_EXPORT_TIMEOUT", "300")

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}
	tm.Record(context.Background(), "test-tool", "test-broker", OutcomeSuccess, "", time.Millisecond)

	// New (not ForceFlush) is what must stay bounded here — see doc comment:
	// ForceFlush's own return does not coincide with the background export
	// actually failing, so its elapsed time is not a meaningful assertion.
	_ = p.ForceFlush(context.Background())

	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(scrapePlainText(t, p), "mcp_otel_metrics_dropped_total")
	}) {
		t.Fatal("mcp_otel_metrics_dropped_total did not appear on /metrics within 5s of an unreachable collector")
	}

	body := scrapePlainText(t, p)
	if !strings.Contains(body, "mcp_tool_invocation_total") {
		t.Error("mcp_tool_invocation_total (unrelated to OTLP health) missing from a scrape taken while the OTLP endpoint is unreachable — the scrape surface must stay up regardless of push health")
	}
}

// TestOTLP_Shutdown_RespectsTimeoutBound mirrors tracing's own
// TestNew_Enabled_Shutdown_RespectsTimeoutBound: a hung flush against an
// unreachable collector must not hold Shutdown past ctx's deadline — the
// property the shutdown-hook registry (Story 48) depends on regardless of
// how many providers are registered against it.
func TestOTLP_Shutdown_RespectsTimeoutBound(t *testing.T) {
	pointAtBlackHole(t)

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const budget = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	_ = p.Shutdown(ctx) // error expected (unreachable / deadline); only the bound is asserted
	if elapsed := time.Since(start); elapsed > budget+500*time.Millisecond {
		t.Errorf("Shutdown against a black hole took %s, want bounded near the %s budget", elapsed, budget)
	}
}

// TestOTLP_Shutdown_WarnsOnIncompleteFlush pins Provider.Shutdown's WARN on
// a flush that doesn't finish before ctx's deadline.
//
// Not a counter, deliberately: a scrape can never observe an increment made
// from inside Shutdown, since the same call has already torn down the
// Prometheus reader by the time it would run (verified directly — pointing
// this at recordDropped instead and scraping afterward gets
// metric.ErrReaderShutdown from the reader, not the new series). A log line
// is the one channel that outlives the pipeline it describes.
func TestOTLP_Shutdown_WarnsOnIncompleteFlush(t *testing.T) {
	pointAtBlackHole(t)

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if shutdownErr := p.Shutdown(ctx); shutdownErr == nil {
		t.Fatal("Shutdown against a black hole returned nil; want an incomplete-flush error to warn about")
	}

	if got := buf.String(); !strings.Contains(got, "OTLP metrics flush incomplete at shutdown") {
		t.Errorf("no WARN logged for an incomplete shutdown flush:\n%s", got)
	}
}

// TestOTLP_ResourceAttributesTravelOnTheStream pins the AC bullet naming
// cloud.region specifically: the identity resource shared with the tracer
// provider (Story 34) reaches the OTLP wire, not just target_info on the
// scrape.
func TestOTLP_ResourceAttributesTravelOnTheStream(t *testing.T) {
	collector := &fakeOTLPCollector{}
	startFakeOTLPCollector(t, collector)

	res, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		attribute.String("cloud.region", "us-east-1"),
	))
	if err != nil {
		t.Fatalf("sdkresource.Merge: %v", err)
	}

	p, err := New(testVersion, res, config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(collector.received()) > 0 }) {
		t.Fatal("fake OTLP collector received nothing within 2s of ForceFlush")
	}

	got, found := findResourceAttr(collector.received(), "cloud.region")
	if !found {
		t.Fatal("cloud.region not present on the OTLP Resource")
	}
	if got != "us-east-1" {
		t.Errorf("cloud.region = %q, want us-east-1", got)
	}
}

// syncBuffer is a concurrency-safe io.Writer for capturing slog output —
// otlpmetricgrpc.New's own env-var parsing fires from the calling goroutine,
// but this proves it regardless of which one.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestOTLP_MalformedHeaders_DoesNotLeakToStderr reproduces
// internal/observability/tracing's own
// TestNew_MalformedOTLPHeaders_DoesNotLeakToStderr for the metrics side of
// the identical leak surface: OTEL_EXPORTER_OTLP_HEADERS with a value that
// isn't valid percent-encoding makes otlpmetricgrpc.New's own env-var
// parsing log the raw value through the OTel SDK's global error/log channel.
// Proves newOTLPReader's oteldiag.Install() call actually closes it for a
// real otlpmetricgrpc.New call, not just for the direct unit tests in
// internal/observability/oteldiag.
func TestOTLP_MalformedHeaders_DoesNotLeakToStderr(t *testing.T) {
	buf := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "Basic%hunter2secret" // %hu is not a valid hex escape
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization="+secret)
	pointAtBlackHole(t)

	p, err := New(testVersion, sdkresource.Default(), config.ObservabilityConfig{MetricsEnabled: true, MetricsOTLPEnabled: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = p.Shutdown(ctx)
	})

	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("captured logs contain the raw malformed header value %q, want it suppressed:\n%s", secret, got)
	}
}
