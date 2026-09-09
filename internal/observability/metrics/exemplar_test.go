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

// SOL-152419 (Story 47): trace exemplars on the two latency histograms.
//
// This file owns the *representation* half of decision D4 — what the two
// exposition formats do and do not carry, and what changes when tracing is off.
// The *call-site* half (does the request-scoped, span-bearing context actually
// reach the observation site) is asserted end-to-end where those call sites
// live: internal/tools/exemplar_test.go for the tool histogram and
// internal/semp/sempv2/client_exemplar_test.go for the SEMP histogram. Both
// halves are needed: a passing test here says the SDK and exporter cooperate,
// and says nothing about whether production hands them a live span.
package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// The two published histogram families this story attaches exemplars to.
const (
	toolDurationFamily = "mcp_tool_invocation_duration_seconds"
	sempDurationFamily = "mcp_semp_request_duration_seconds"
)

// Fixed sample values, so every case in a table produces byte-identical
// histogram output and the only difference left to observe is the exemplar.
const exemplarSampleDuration = 7 * time.Millisecond

// exemplarTraceIDRe pulls the trace_id out of the exemplar that follows the
// " # " marker on an OpenMetrics sample line:
//
//	mcp_..._bucket{...,le="0.01"} 1 # {trace_id="0a1b…",span_id="c3d4…"} 0.007 1.78e+09
//
// Matched by label name, never by position: the exporter builds the exemplar
// label set from a Go map, so trace_id and span_id come out in either order
// (observed both ways in one scrape).
var exemplarTraceIDRe = regexp.MustCompile(`# \{[^}]*\btrace_id="([0-9a-f]+)"`)

// scrapeOpenMetrics does one scrape that negotiates OpenMetrics, which is the
// only representation that carries exemplars (D4). The header is the whole
// point of the test: against a plain-text response the exemplar assertions
// below would pass or fail for the wrong reason.
func scrapeOpenMetrics(t *testing.T, p *Provider) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept", `application/openmetrics-text; version=1.0.0; charset=utf-8`)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "openmetrics-text") {
		t.Fatalf("handler did not serve OpenMetrics: Content-Type = %q. "+
			"Exemplars only appear under OpenMetrics negotiation, so without it every "+
			"assertion in this file is vacuous — check EnableOpenMetrics in Provider.Handler.", ct)
	}
	return rec.Body.String()
}

// familyLines returns the sample lines of one histogram family (_bucket, _sum,
// _count), exemplar suffix included.
func familyLines(body, family string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, family+"_bucket{") ||
			strings.HasPrefix(line, family+"_sum{") ||
			strings.HasPrefix(line, family+"_count{") {
			out = append(out, line)
		}
	}
	sort.Strings(out)
	return out
}

// withoutExemplars drops the " # {...}" exemplar suffix from each line, leaving
// the series identity and its value. Comparing two scrapes through this is how
// "the histogram is otherwise unchanged" is asserted: same series, same label
// keys, same bucket counts, exemplar or not.
func withoutExemplars(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		sample, _, _ := strings.Cut(line, " # ")
		out[i] = sample
	}
	return out
}

// exemplarTraceIDs returns the trace_id of every exemplar on the given family's
// sample lines.
func exemplarTraceIDs(body, family string) []string {
	var out []string
	for _, line := range familyLines(body, family) {
		if m := exemplarTraceIDRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// recordOneOfEach drives both real recorders once, with the ctx under test, so
// the assertions run against the shipped instruments rather than a histogram
// built for the test.
func recordOneOfEach(t *testing.T, p *Provider, ctx context.Context) {
	t.Helper()
	tm, err := p.ToolMetrics()
	if err != nil {
		t.Fatalf("ToolMetrics: %v", err)
	}
	sm, err := p.SEMPMetrics()
	if err != nil {
		t.Fatalf("SEMPMetrics: %v", err)
	}
	tm.Record(ctx, "test-tool", "test-broker", OutcomeSuccess, "", exemplarSampleDuration)
	sm.Record(ctx, SEMPRequest{
		API:       "v2",
		Broker:    "test-broker",
		Operation: "getMsgVpnQueue",
		Method:    "GET",
		Status:    "200",
		Address:   "broker.example.com",
		Attempt:   1,
	}, exemplarSampleDuration)
}

// TestExemplars_OnlyASampledSpanProducesOne is the story's central assertion,
// and its negative cases are load-bearing rather than decorative.
//
// Four contexts reach the same two recorders:
//
//   - a sampled span (tracing on, sampler admits) — must produce an exemplar
//     whose trace_id is that span's, on BOTH histograms;
//   - an unsampled span (tracing on, low OTEL_TRACES_SAMPLER_ARG) — must
//     produce none. This is the documented sampling interaction, not a defect;
//   - an explicit no-op tracer provider (OBS_TRACING_ENABLED off) — none;
//   - the process-global tracer provider, which in this test binary is the
//     no-op one nothing has replaced — none. Guarded below, so it cannot
//     quietly become a fifth "sampled" case and pass for the wrong reason.
//
// Every case's exemplar-stripped histogram output is then compared against the
// sampled case's. Identical output is the proof of two separate acceptance
// criteria at once: metrics gained no dependency on tracing being enabled, and
// exemplars added no label key and no series (which is also why Story 14's
// golden file needs no regeneration).
func TestExemplars_OnlyASampledSpanProducesOne(t *testing.T) {
	cases := []struct {
		name         string
		newTracer    func(t *testing.T) trace.Tracer
		wantExemplar bool
	}{
		{
			name: "tracing_on_and_sampled",
			newTracer: func(t *testing.T) trace.Tracer {
				return sdkTracer(t, sdktrace.AlwaysSample())
			},
			wantExemplar: true,
		},
		{
			name: "tracing_on_but_sampler_declined",
			newTracer: func(t *testing.T) trace.Tracer {
				return sdkTracer(t, sdktrace.NeverSample())
			},
			wantExemplar: false,
		},
		{
			name: "tracing_off_noop_provider",
			newTracer: func(*testing.T) trace.Tracer {
				return noop.NewTracerProvider().Tracer("exemplar-test")
			},
			wantExemplar: false,
		},
		{
			name: "tracing_off_process_default",
			newTracer: func(*testing.T) trace.Tracer {
				return otel.Tracer("exemplar-test")
			},
			wantExemplar: false,
		},
	}

	stripped := map[string][]string{}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(testVersion, sdkresource.Default())
			if err != nil {
				t.Fatal(err)
			}

			ctx, span := tc.newTracer(t).Start(context.Background(), "exemplar-test-span")
			sampled := span.SpanContext().IsSampled()
			traceID := span.SpanContext().TraceID().String()
			recordOneOfEach(t, p, ctx)
			span.End()

			// The negative cases assert an absence, which is exactly the shape
			// that passes when the setup silently stopped doing what it claims.
			// Pin the premise instead of trusting it: if a future edit installs
			// a sampling global provider in this package's test binary, or
			// changes what NeverSample does, this fails here with a reason
			// rather than passing quietly downstream.
			if sampled != tc.wantExemplar {
				t.Fatalf("span sampled = %v, want %v — this case no longer exercises "+
					"the path it names, so its exemplar assertion below is meaningless",
					sampled, tc.wantExemplar)
			}

			body := scrapeOpenMetrics(t, p)
			for _, family := range []string{toolDurationFamily, sempDurationFamily} {
				lines := familyLines(body, family)
				if len(lines) == 0 {
					t.Fatalf("%s: no sample lines in scrape — nothing was recorded, so "+
						"an absent exemplar proves nothing:\n%s", family, body)
				}

				got := exemplarTraceIDs(body, family)
				switch {
				case !tc.wantExemplar:
					if len(got) != 0 {
						t.Errorf("%s: got exemplar trace_ids %v, want none", family, got)
					}
				case len(got) == 0:
					t.Errorf("%s: no exemplar on any bucket, want one carrying trace_id %q\n%s",
						family, traceID, strings.Join(lines, "\n"))
				default:
					// One exemplar per bucket, and only the bucket the sample
					// landed in has one — so exactly one here, and it must be
					// this span's trace, not merely some trace.
					for _, id := range got {
						if id != traceID {
							t.Errorf("%s: exemplar trace_id = %q, want the recorded span's %q",
								family, id, traceID)
						}
					}
				}

				stripped[tc.name+"|"+family] = withoutExemplars(lines)
			}
		})
	}

	// "The histogram is otherwise unchanged": every case must have produced the
	// same series, labels and bucket counts as the sampled one.
	const reference = "tracing_on_and_sampled"
	for _, tc := range cases {
		if tc.name == reference {
			continue
		}
		for _, family := range []string{toolDurationFamily, sempDurationFamily} {
			want := strings.Join(stripped[reference+"|"+family], "\n")
			got := strings.Join(stripped[tc.name+"|"+family], "\n")
			if got != want {
				t.Errorf("%s: %s histogram differs from the sampled case beyond its exemplars.\n"+
					"--- %s ---\n%s\n--- %s ---\n%s", tc.name, family, tc.name, got, reference, want)
			}
		}
	}
}

// sdkTracer returns a tracer from a private SDK provider with the given
// sampler. Private, not the global one: two cases in the table above need
// different samplers in the same test binary, and otel.SetTracerProvider
// honours only the first call per process.
func sdkTracer(t *testing.T, sampler sdktrace.Sampler) trace.Tracer {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
	t.Cleanup(func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			t.Errorf("TracerProvider.Shutdown: %v", err)
		}
	})
	return tp.Tracer("exemplar-test")
}

// TestExemplars_AbsentFromPlainTextScrape pins the premise Story 14's golden
// file rests on: the plain-text exposition carries no exemplar even when a
// sampled span was active at the observation site. That is what makes
// TestGoldenSchema deterministic — an exemplar embeds a live trace ID, and a
// byte-comparison fixture over one would fail on every run. If this ever
// starts failing, the golden file has become a per-run regeneration chore and
// the schema-freeze gate has stopped being a gate; fix the representation
// split rather than regenerating the fixture.
func TestExemplars_AbsentFromPlainTextScrape(t *testing.T) {
	p, err := New(testVersion, sdkresource.Default())
	if err != nil {
		t.Fatal(err)
	}

	ctx, span := sdkTracer(t, sdktrace.AlwaysSample()).Start(context.Background(), "exemplar-test-span")
	if !span.SpanContext().IsSampled() {
		t.Fatal("span not sampled despite AlwaysSample(): this test would prove nothing")
	}
	recordOneOfEach(t, p, ctx)
	span.End()

	// Same provider, same recorded samples, both representations — so the
	// difference observed is the representation and nothing else.
	if got := exemplarTraceIDs(scrapeOpenMetrics(t, p), toolDurationFamily); len(got) == 0 {
		t.Fatal("no exemplar under OpenMetrics, so the plain-text assertion below is vacuous")
	}

	plain := scrapePlainText(t, p)
	for _, family := range []string{toolDurationFamily, sempDurationFamily} {
		lines := familyLines(plain, family)
		if len(lines) == 0 {
			t.Fatalf("%s: no sample lines in the plain-text scrape:\n%s", family, plain)
		}
		for _, line := range lines {
			if strings.Contains(line, " # ") {
				t.Errorf("%s: plain-text scrape carries an exemplar, which would make "+
					"Story 14's golden file non-deterministic:\n%s", family, line)
			}
		}
	}
}
