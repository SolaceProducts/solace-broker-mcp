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

// SOL-152417 (Story 45): a hard-time-boxed spike proving whether
// go.opentelemetry.io/otel/exporters/prometheus can render our intended frozen
// metric names — the go/no-go on ADR-007's OTel-first metrics path. This is
// the durable form of that evidence, not a throwaway: on PASS its captured
// scrape output seeds Story 14's golden-file fixture. See ADR-007, ADR-008,
// RISK-006, and D3/D4 in ALIGNMENT-DECISIONS.md (discovery repo,
// Broker-MCP/broker-mcp-obs-tel-sol-150251) for the decisions this test
// harness is built to verify rather than assume.
//
// Pinned module versions (2026-08-28, current stable at time of writing):
//
//	go.opentelemetry.io/otel                       v1.46.0
//	go.opentelemetry.io/otel/sdk                   v1.46.0
//	go.opentelemetry.io/otel/exporters/prometheus   v0.68.0
//	github.com/prometheus/client_golang            v1.24.1
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otlprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
)

// The two named instruments the AC calls for, plus their intended frozen
// Prometheus names (ADR-008's table). The third representative instrument —
// the constant-1 info gauge — has no fixed name in the AC, so it is built
// fresh in whichever test needs a specific unit/name combination.
const (
	toolInvocationName    = "mcp.tool.invocation"
	toolInvocationUnit    = "1"
	toolInvocationDurName = "mcp.tool.invocation.duration"
	toolInvocationDurUnit = "s"
	wantCounterName       = "mcp_tool_invocation_total"
	wantHistogramName     = "mcp_tool_invocation_duration_seconds"
	infoGaugeName         = "mcp.build.info"
	wantInfoGaugeName     = "mcp_build_info"
)

// newHarness builds a fresh prometheus.Registry + otlprom.Exporter +
// sdkmetric.MeterProvider per call, so t.Parallel() subtests never collide on
// a shared registry. expOpts are passed through to otlprom.New ahead of the
// mandatory WithRegisterer; mpOpts are passed through to
// sdkmetric.NewMeterProvider ahead of the mandatory WithReader.
func newHarness(t *testing.T, mpOpts []sdkmetric.Option, expOpts ...otlprom.Option) (*prometheus.Registry, metric.Meter) {
	t.Helper()

	reg := prometheus.NewRegistry()
	opts := append(append([]otlprom.Option{}, expOpts...), otlprom.WithRegisterer(reg))
	exp, err := otlprom.New(opts...)
	if err != nil {
		t.Fatalf("otlprom.New: %v", err)
	}

	providerOpts := append(append([]sdkmetric.Option{}, mpOpts...), sdkmetric.WithReader(exp))
	mp := sdkmetric.NewMeterProvider(providerOpts...)
	t.Cleanup(func() {
		if err := mp.Shutdown(context.Background()); err != nil {
			t.Errorf("MeterProvider.Shutdown: %v", err)
		}
	})

	return reg, mp.Meter("solace-broker-mcp/exporter-fidelity-test")
}

// scrape serves reg through promhttp.HandlerFor exactly as Story 14's real
// /metrics endpoint will, with OpenMetrics always enabled on the handler side
// (per D4 / ADR-008 — the handler must offer it even though only an
// OpenMetrics-negotiating request receives it). openMetrics selects which
// representation the *request* negotiates.
func scrape(t *testing.T, reg *prometheus.Registry, openMetrics bool) string {
	t.Helper()

	handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{EnableOpenMetrics: true})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	if openMetrics {
		req.Header.Set("Accept", `application/openmetrics-text; version=1.0.0; charset=utf-8`)
	} else {
		req.Header.Set("Accept", "text/plain")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// registerThreeInstruments registers the counter, histogram, and constant-1
// info gauge named in the AC, then records one sample on each so a scrape has
// something to show. Unit "1" on the counter is deliberate (the AC's exact
// spec) and, per ADR-008's derivation, contributes no suffix on a monotonic
// counter — only a Gauge with unit "1" earns the "_ratio" trap (see
// TestExporterFidelity_AdjustedNamesAndUnits). The info gauge here
// deliberately carries no unit, which is that adjustment already applied.
func registerThreeInstruments(t *testing.T, meter metric.Meter) {
	t.Helper()
	ctx := context.Background()

	counter, err := meter.Int64Counter(toolInvocationName, metric.WithUnit(toolInvocationUnit))
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", "get-broker-status")))

	hist, err := meter.Float64Histogram(toolInvocationDurName, metric.WithUnit(toolInvocationDurUnit))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	hist.Record(ctx, 0.042, metric.WithAttributes(attribute.String("tool", "get-broker-status")))

	gauge, err := meter.Int64Gauge(infoGaugeName)
	if err != nil {
		t.Fatalf("Int64Gauge: %v", err)
	}
	gauge.Record(ctx, 1, metric.WithAttributes(attribute.String("version", "0.0.0-spike")))
}

// TestExporterFidelity_DefaultTransformation is the AC's first rung, and the
// premise the other rungs are judged against: with no options beyond
// WithRegisterer, does the exporter's default transformation already produce
// the intended frozen names?
func TestExporterFidelity_DefaultTransformation(t *testing.T) {
	t.Parallel()

	reg, meter := newHarness(t, nil)
	registerThreeInstruments(t, meter)
	got := scrape(t, reg, false)

	for _, want := range []string{
		wantCounterName + `{`,
		wantHistogramName + `_bucket{`,
		wantHistogramName + `_sum{`,
		wantHistogramName + `_count{`,
		wantInfoGaugeName + `{`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("default transformation: scrape missing %q\n--- scrape ---\n%s", want, got)
		}
	}
}

// TestExporterFidelity_ScopeInfoSuppressed confirms WithoutScopeInfo() removes
// otel_scope_name / otel_scope_version from every series — present by default,
// absent with the option.
func TestExporterFidelity_ScopeInfoSuppressed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		withOption  bool
		wantPresent bool
	}{
		{name: "default_carries_scope_info", withOption: false, wantPresent: true},
		{name: "WithoutScopeInfo_removes_it", withOption: true, wantPresent: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var expOpts []otlprom.Option
			if tc.withOption {
				expOpts = append(expOpts, otlprom.WithoutScopeInfo())
			}
			reg, meter := newHarness(t, nil, expOpts...)
			registerThreeInstruments(t, meter)
			got := scrape(t, reg, false)

			hasScope := strings.Contains(got, "otel_scope_name") || strings.Contains(got, "otel_scope_version")
			if hasScope != tc.wantPresent {
				t.Errorf("otel_scope_* present = %v, want %v\n--- scrape ---\n%s", hasScope, tc.wantPresent, got)
			}
		})
	}
}

// TestExporterFidelity_TargetInfoCarriesResourceAttributes confirms
// target_info is present and carries resource attributes — the mechanism
// Story 34's dashboard-variable approach depends on.
func TestExporterFidelity_TargetInfoCarriesResourceAttributes(t *testing.T) {
	t.Parallel()

	res := resource.NewSchemaless(
		attribute.String("service.name", "solace-broker-mcp"),
		attribute.String("service.instance.id", "spike-test-instance"),
	)
	reg, meter := newHarness(t, []sdkmetric.Option{sdkmetric.WithResource(res)})
	registerThreeInstruments(t, meter)
	got := scrape(t, reg, false)

	if !strings.Contains(got, "target_info{") {
		t.Fatalf("target_info series missing entirely\n--- scrape ---\n%s", got)
	}
	for _, want := range []string{`service_name="solace-broker-mcp"`, `service_instance_id="spike-test-instance"`} {
		if !strings.Contains(got, want) {
			t.Errorf("target_info missing resource attribute %q\n--- scrape ---\n%s", want, got)
		}
	}
}

// TestExporterFidelity_TranslationStrategyExplicit is ADR-007's rung 1,
// evaluated explicitly per D3: WithTranslationStrategy is NOT deprecated —
// it's the documented replacement WithoutUnits/WithoutCounterSuffixes point
// at. Records what both supported strategies actually produce.
func TestExporterFidelity_TranslationStrategyExplicit(t *testing.T) {
	t.Parallel()

	t.Run("UnderscoreEscapingWithSuffixes_matches_default", func(t *testing.T) {
		t.Parallel()
		reg, meter := newHarness(t, nil, otlprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes))
		registerThreeInstruments(t, meter)
		got := scrape(t, reg, false)
		if !strings.Contains(got, wantCounterName+`{`) {
			t.Errorf("UnderscoreEscapingWithSuffixes: expected %q, got:\n%s", wantCounterName, got)
		}
	})

	t.Run("NoTranslation_recorded_for_the_record", func(t *testing.T) {
		t.Parallel()
		reg, meter := newHarness(t, nil, otlprom.WithTranslationStrategy(otlptranslator.NoTranslation))
		registerThreeInstruments(t, meter)
		got := scrape(t, reg, false)
		// Recorded, not asserted-to-pass-or-fail: NoTranslation disables suffixing
		// and unit rewriting, so the counter does NOT get "_total" and the
		// histogram does NOT get "_seconds" — whatever dotted or quoted form the
		// OTel name survives as is exactly the evidence the AC asks this rung to
		// record, not a defect in the harness.
		t.Logf("NoTranslation scrape output:\n%s", got)
	})
}

// TestExporterFidelity_AdjustedNamesAndUnits is ADR-007's rung 2: instead of a
// translation-strategy override, choose instrument names/units so the
// *default* transformation lands on the intended output. Demonstrates the
// real trap ADR-008's derivation calls out — a Gauge with unit "1" earns an
// unwanted "_ratio" suffix (unlike a Counter, where unit "1" is silent) — and
// the fix: give the info gauge no unit at all.
func TestExporterFidelity_AdjustedNamesAndUnits(t *testing.T) {
	t.Parallel()

	t.Run("gauge_with_unit_1_gets_unwanted_ratio_suffix", func(t *testing.T) {
		t.Parallel()
		reg, meter := newHarness(t, nil)
		gauge, err := meter.Int64Gauge(infoGaugeName, metric.WithUnit("1"))
		if err != nil {
			t.Fatalf("Int64Gauge: %v", err)
		}
		gauge.Record(context.Background(), 1)
		got := scrape(t, reg, false)
		if !strings.Contains(got, wantInfoGaugeName+"_ratio{") {
			t.Errorf("expected the unit=1 gauge trap to append _ratio; got:\n%s", got)
		}
	})

	t.Run("gauge_with_no_unit_lands_on_intended_name", func(t *testing.T) {
		t.Parallel()
		reg, meter := newHarness(t, nil)
		gauge, err := meter.Int64Gauge(infoGaugeName) // no WithUnit — the adjustment
		if err != nil {
			t.Fatalf("Int64Gauge: %v", err)
		}
		gauge.Record(context.Background(), 1)
		got := scrape(t, reg, false)
		if !strings.Contains(got, wantInfoGaugeName+"{") {
			t.Errorf("expected adjusted (unitless) gauge to land on %q; got:\n%s", wantInfoGaugeName, got)
		}
		if strings.Contains(got, wantInfoGaugeName+"_ratio") {
			t.Errorf("adjusted gauge should NOT carry _ratio; got:\n%s", got)
		}
	})
}

// TestExporterFidelity_NoDeprecatedOptions is structural: SA1019
// (staticcheck, unrelaxed on _test.go files per .golangci.yml) is the actual
// enforcement that WithoutUnits()/WithoutCounterSuffixes() never appear in
// this harness. This test doc-comments the constraint; it does not (and
// cannot) assert an absence of code elsewhere in the file. If a future edit
// adds either deprecated call anywhere in this package, `make check` fails on
// SA1019 before it fails here.
func TestExporterFidelity_NoDeprecatedOptions(t *testing.T) {
	t.Parallel()
	t.Log("otlprom.WithoutUnits() and otlprom.WithoutCounterSuffixes() are deprecated " +
		"(exporters/prometheus@v0.68.0/config.go) and must never appear in this package. " +
		"staticcheck's SA1019 enforces this at build time, unrelaxed on _test.go files.")
}

// TestExporterFidelity_ExemplarsUnderOpenMetrics is D4: a histogram Record
// inside a sampled span's context should carry that span's trace ID as an
// exemplar — but only under OpenMetrics negotiation. The plain-text scrape
// must show no exemplar (which is why Story 14's golden file pins plain-text
// as the deterministic representation); the OpenMetrics scrape must show one
// whose trace_id matches the recorded span.
func TestExporterFidelity_ExemplarsUnderOpenMetrics(t *testing.T) {
	t.Parallel()

	reg, meter := newHarness(t, nil)
	hist, err := meter.Float64Histogram(toolInvocationDurName, metric.WithUnit(toolInvocationDurUnit))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("TracerProvider.Shutdown: %v", err)
		}
	})
	tracer := tp.Tracer("exporter-fidelity-test")
	ctx, span := tracer.Start(context.Background(), "test-span")
	hist.Record(ctx, 0.042, metric.WithAttributes(attribute.String("tool", "get-broker-status")))
	span.End()

	if !span.SpanContext().IsSampled() {
		t.Fatalf("span not sampled despite AlwaysSample()")
	}
	traceID := span.SpanContext().TraceID().String()

	plain := scrape(t, reg, false)
	if strings.Contains(plain, "# {") {
		t.Errorf("plain-text scrape unexpectedly carries an exemplar:\n%s", plain)
	}

	openMetrics := scrape(t, reg, true)
	if !strings.Contains(openMetrics, traceID) {
		t.Errorf("OpenMetrics scrape missing exemplar trace_id %q:\n%s", traceID, openMetrics)
	}
}

// TestExporterFidelity_SharedRegistry confirms the OTel bridge coexists with
// client_golang's own Go/process collectors in one registry — the arrangement
// Stories 14 and 19 depend on to serve both from a single /metrics endpoint.
func TestExporterFidelity_SharedRegistry(t *testing.T) {
	t.Parallel()

	reg, meter := newHarness(t, nil)
	if err := reg.Register(collectors.NewGoCollector()); err != nil {
		t.Fatalf("register Go collector: %v", err)
	}
	if err := reg.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		t.Fatalf("register process collector: %v", err)
	}
	registerThreeInstruments(t, meter)

	got := scrape(t, reg, false)
	for _, want := range []string{wantCounterName + `{`, "go_goroutines", "process_"} {
		if !strings.Contains(got, want) {
			t.Errorf("shared registry: missing %q\n--- scrape ---\n%s", want, got)
		}
	}
}

// TestExporterFidelity_CaptureSeedOutput exercises the actual scrape output
// under the ADR-008-recommended production shape — WithoutScopeInfo() plus
// resource attributes — so the PASS verdict pastes real captured bytes into
// ISSUE-029 rather than a hand-assembled example, and Story 14's golden file
// has a real seed to start from. Plain-text is the deterministic
// representation (per D4, this is what Story 14 pins) and is reproduced
// byte-for-byte on every run given these fixed inputs. The OpenMetrics
// capture is NOT reproducible by design — an exemplar embeds the live
// trace_id/span_id/timestamp of a freshly sampled span — so writing it to
// testdata/ on every `go test` run would leave the working tree dirty after
// every local run and every CI job. The write to testdata/ is therefore
// gated behind WRITE_FIDELITY_SEED=1, a deliberate one-time capture, not a
// side effect of the ordinary test suite; run
// `WRITE_FIDELITY_SEED=1 go test ./internal/observability/metrics/... -run TestExporterFidelity_CaptureSeedOutput`
// to (re)generate testdata/fidelity_plaintext.txt and
// testdata/fidelity_openmetrics.txt. Without the flag this test still
// exercises the same code path and asserts the scrape is well-formed; it
// just doesn't touch disk.
func TestExporterFidelity_CaptureSeedOutput(t *testing.T) {
	t.Parallel()

	res := resource.NewSchemaless(
		attribute.String("service.name", "solace-broker-mcp"),
		attribute.String("service.version", "0.0.0-spike"),
		attribute.String("service.instance.id", "spike-test-instance"),
	)
	reg, meter := newHarness(t, []sdkmetric.Option{sdkmetric.WithResource(res)}, otlprom.WithoutScopeInfo())

	counter, err := meter.Int64Counter(toolInvocationName, metric.WithUnit(toolInvocationUnit))
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	hist, err := meter.Float64Histogram(toolInvocationDurName, metric.WithUnit(toolInvocationDurUnit))
	if err != nil {
		t.Fatalf("Float64Histogram: %v", err)
	}
	gauge, err := meter.Int64Gauge(infoGaugeName)
	if err != nil {
		t.Fatalf("Int64Gauge: %v", err)
	}
	gauge.Record(context.Background(), 1, metric.WithAttributes(attribute.String("version", "0.0.0-spike")))

	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("TracerProvider.Shutdown: %v", err)
		}
	})
	ctx, span := tp.Tracer("exporter-fidelity-test").Start(context.Background(), "test-span")
	counter.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", "get-broker-status")))
	hist.Record(ctx, 0.042, metric.WithAttributes(attribute.String("tool", "get-broker-status")))
	span.End()

	plain := scrape(t, reg, false)
	if !strings.Contains(plain, wantCounterName+`{`) {
		t.Fatalf("plain-text capture missing %q; refusing to treat this as seed-worthy:\n%s", wantCounterName, plain)
	}

	openMetrics := scrape(t, reg, true)
	if !strings.Contains(openMetrics, wantCounterName+`{`) {
		t.Fatalf("OpenMetrics capture missing %q; refusing to treat this as seed-worthy:\n%s", wantCounterName, openMetrics)
	}

	if os.Getenv("WRITE_FIDELITY_SEED") == "" {
		t.Skip("WRITE_FIDELITY_SEED not set; scrape validated but testdata/ left untouched (see doc comment)")
	}
	if err := os.WriteFile("testdata/fidelity_plaintext.txt", []byte(plain), 0o644); err != nil {
		t.Fatalf("write fidelity_plaintext.txt: %v", err)
	}
	if err := os.WriteFile("testdata/fidelity_openmetrics.txt", []byte(openMetrics), 0o644); err != nil {
		t.Fatalf("write fidelity_openmetrics.txt: %v", err)
	}
}
