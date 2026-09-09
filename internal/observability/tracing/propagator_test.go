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

// The W3C Trace Context propagator New installs (SOL-152421).
//
// These exist because the failure is completely silent: OTel's global default
// propagator is a no-op, so with no SetTextMapPropagator call anywhere,
// otelhttp extracts nothing, every entry span starts a fresh root, and an
// agent's trace breaks at this server's edge — with no error, no log line, and
// spans that look healthy in the backend.
package tracing

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// A traceparent an AI agent ahead of this server would send: version 00, sampled.
const (
	upstreamTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	upstreamSpanID  = "00f067aa0ba902b7"
	upstreamHeader  = "00-" + upstreamTraceID + "-" + upstreamSpanID + "-01"
)

// restorePropagator returns the global propagator to its pre-test value. Unlike
// SetTracerProvider, SetTextMapPropagator has no one-shot delegate, so a plain
// save/restore is sound.
func restorePropagator(t *testing.T) {
	t.Helper()
	prev := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })
}

// The wiring half of AC 3: after New, an inbound traceparent resolves to the
// upstream span context, which is what lets the entry span be its child.
func TestNew_InstallsW3CTraceContextPropagator(t *testing.T) {
	restorePropagator(t)
	// Start from a known no-op, so a pass cannot come from a propagator an
	// earlier test left installed.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	p, err := New(config.ObservabilityConfig{TracingEnabled: true}, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	hdr := http.Header{}
	hdr.Set("traceparent", upstreamHeader)
	ctx := otel.GetTextMapPropagator().Extract(
		context.Background(), propagation.HeaderCarrier(hdr))

	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		t.Fatal("extracted span context is invalid: the inbound traceparent was not parsed, so entry spans would all be roots")
	}
	if got := sc.TraceID().String(); got != upstreamTraceID {
		t.Errorf("trace ID = %s, want %s", got, upstreamTraceID)
	}
	if got := sc.SpanID().String(); got != upstreamSpanID {
		t.Errorf("span ID = %s, want %s", got, upstreamSpanID)
	}
	if !sc.IsRemote() {
		t.Error("span context is not marked remote, but it came from an inbound header")
	}
	if !sc.IsSampled() {
		t.Error("sampled flag lost; a child of a sampled upstream span must stay sampled")
	}
}

// The outbound direction. A future story carries this server's span context on
// to the broker with the same propagator, so an extract-only propagator would
// be a half-wiring that reads as complete.
func TestNew_PropagatorInjectsTraceparent(t *testing.T) {
	restorePropagator(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	p, err := New(config.ObservabilityConfig{TracingEnabled: true}, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	// Synthetic rather than from p.tp.Tracer().Start(): a real recording span
	// gives the batch processor something to flush, and Shutdown then blocks
	// for the exporter's full 10s timeout. The propagator only reads the
	// span context anyway.
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(
		trace.SpanContextConfig{
			TraceID:    traceIDFromHex(t, upstreamTraceID),
			SpanID:     spanIDFromHex(t, upstreamSpanID),
			TraceFlags: trace.FlagsSampled,
		}))

	hdr := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(hdr))
	if got := hdr.Get("traceparent"); got != upstreamHeader {
		t.Errorf("injected traceparent = %q, want %q", got, upstreamHeader)
	}
}

func traceIDFromHex(t *testing.T, s string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(s)
	if err != nil {
		t.Fatalf("TraceIDFromHex(%q): %v", s, err)
	}
	return id
}

func spanIDFromHex(t *testing.T, s string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(s)
	if err != nil {
		t.Fatalf("SpanIDFromHex(%q): %v", s, err)
	}
	return id
}

// The deliberate omission. Baggage carries arbitrary upstream key-value pairs
// into this process and onward; this server has no reason to accept them and no
// redaction policy for them. This test failing is how a later story adds
// baggage knowingly rather than drifting into it.
func TestNew_PropagatorExcludesBaggage(t *testing.T) {
	restorePropagator(t)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	p, err := New(config.ObservabilityConfig{TracingEnabled: true}, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Shutdown(context.Background()) })

	hdr := http.Header{}
	hdr.Set("traceparent", upstreamHeader)
	hdr.Set("baggage", "tenant=acme,secret=hunter2")
	ctx := otel.GetTextMapPropagator().Extract(
		context.Background(), propagation.HeaderCarrier(hdr))

	// Nothing extracted it, so nothing can forward it.
	out := http.Header{}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(out))
	if got := out.Get("baggage"); got != "" {
		t.Errorf("baggage = %q, want it not propagated; New installs TraceContext only", got)
	}
	// traceparent and tracestate are both W3C Trace Context and expected;
	// "baggage" is the field whose absence is the point.
	for _, f := range otel.GetTextMapPropagator().Fields() {
		if f == "baggage" {
			t.Error("propagator claims the baggage field; New installs TraceContext only")
		}
	}
}

// The door-closing half: with the flag off, New returns before installing
// anything. Together with TestNew_Disabled_DoesNotTouchGlobalTracerProvider
// (provider_test.go), this is AC 6 at the level the flag actually controls.
func TestNew_Disabled_LeavesPropagatorUntouched(t *testing.T) {
	restorePropagator(t)
	noop := propagation.NewCompositeTextMapPropagator()
	otel.SetTextMapPropagator(noop)

	p, err := New(config.ObservabilityConfig{TracingEnabled: false}, nil, sdkresource.Default())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if p != nil {
		t.Fatalf("New() = %v, want nil when disabled", p)
	}

	hdr := http.Header{}
	hdr.Set("traceparent", upstreamHeader)
	ctx := otel.GetTextMapPropagator().Extract(
		context.Background(), propagation.HeaderCarrier(hdr))
	if trace.SpanContextFromContext(ctx).IsValid() {
		t.Error("a traceparent was extracted with tracing disabled: New installed a propagator it should not have")
	}
}
