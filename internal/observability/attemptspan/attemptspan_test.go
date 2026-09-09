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

package attemptspan

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// The tracer provider is installed exactly once per test binary: otel.Tracer
// handles obtained before the first SetTracerProvider delegate to that first
// provider and to no later one. Same constraint as the callers' own span
// tests (internal/semp/resilience, internal/idpclient).
var (
	sharedSpanRecorder *tracetest.SpanRecorder
	installSpanTracer  sync.Once
)

func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installSpanTracer.Do(func() {
		sharedSpanRecorder = tracetest.NewSpanRecorder()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sharedSpanRecorder),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		))
	})
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testTransport(t *testing.T, base http.RoundTripper, getState func(context.Context) *State) *Transport {
	t.Helper()
	return &Transport{
		Base:     base,
		Tracer:   otel.Tracer("attemptspan-test"),
		SpanName: "test.attempt",
		GetState: getState,
	}
}

func TestTransport_OpensOneSpanPerAttempt(t *testing.T) {
	recordSpans(t)

	state := &State{}
	tr := testTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}), func(context.Context) *State { return state })

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://broker.invalid/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if state.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", state.Attempt)
	}
	if state.Span == nil {
		t.Fatal("no span parked on state after RoundTrip")
	}
	state.Span.End()
}

func TestTransport_RedirectHopsAreNotNewAttempts(t *testing.T) {
	state := &State{}
	tr := testTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}), func(context.Context) *State { return state })

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://broker.invalid/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Response = &http.Response{StatusCode: http.StatusFound}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if state.Attempt != 0 {
		t.Errorf("Attempt = %d after a redirect hop, want 0", state.Attempt)
	}
	if state.Span != nil {
		t.Error("a redirect hop parked a span; a second one would overwrite and leak it")
	}
}

func TestTransport_NoStateFallsThroughUntraced(t *testing.T) {
	var reached bool
	tr := testTransport(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reached = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}), func(context.Context) *State { return nil })

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://broker.invalid/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()

	if !reached {
		t.Error("the base transport was not called")
	}
}

func TestTransport_ForwardsCloseIdleConnections(t *testing.T) {
	var closed int
	inner := &closeIdleRecorder{closed: &closed}
	tr := testTransport(t, inner, func(context.Context) *State { return nil })

	client := &http.Client{Transport: tr}
	client.CloseIdleConnections()

	if closed != 1 {
		t.Errorf("inner transport saw %d CloseIdleConnections calls, want 1", closed)
	}
}

func TestForwardCloseIdleConnections_NoopWhenAbsent(t *testing.T) {
	// A transport with no CloseIdleConnections method: must not panic.
	ForwardCloseIdleConnections(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, nil
	}))
}

type closeIdleRecorder struct {
	closed *int
}

func (c *closeIdleRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func (c *closeIdleRecorder) CloseIdleConnections() { *c.closed++ }

func TestFinish_SetsTheSharedAttributeSet(t *testing.T) {
	sr := recordSpans(t)

	ctx := correlation.With(context.Background(), "corr-123")
	ctx, span := otel.Tracer("test").Start(ctx, "test.attempt")
	state := &State{Attempt: 2, Span: span}

	resp := &http.Response{StatusCode: http.StatusServiceUnavailable}
	Finish(ctx, state, resp, true, true)

	if state.Span != nil {
		t.Error("Span is still parked on state after Finish")
	}
	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1", len(ended))
	}
	attrs := spanAttrs(ended[0])
	want := map[string]any{
		"attempt":                   int64(2),
		"retry.decision":            true,
		"correlation_id":            "corr-123",
		"http.response.status_code": int64(503),
		"retry.exhausted":           true,
	}
	for k, v := range want {
		if got, ok := attrs[k]; !ok || got != v {
			t.Errorf("attribute %q = %v (present=%v), want %v", k, got, ok, v)
		}
	}
}

func TestFinish_OmitsCorrelationIDWhenAbsent(t *testing.T) {
	sr := recordSpans(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "test.attempt")
	state := &State{Attempt: 1, Span: span}

	Finish(ctx, state, &http.Response{StatusCode: http.StatusOK}, false, false)

	attrs := spanAttrs(sr.Ended()[0])
	if _, present := attrs["correlation_id"]; present {
		t.Error("correlation_id is set when the context carried none")
	}
	if _, present := attrs["retry.exhausted"]; present {
		t.Error("retry.exhausted is set when exhausted=false — it should be omitted, not written false")
	}
}

func TestFinish_OmitsStatusCodeOnConnectionError(t *testing.T) {
	sr := recordSpans(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "test.attempt")
	state := &State{Attempt: 1, Span: span}

	// nil resp: a connection error, no response was ever received.
	Finish(ctx, state, nil, true, false)

	attrs := spanAttrs(sr.Ended()[0])
	if _, present := attrs["http.response.status_code"]; present {
		t.Error("http.response.status_code is set with a nil response")
	}
}

func TestFinish_NoOutcomeOrErrorStatusEverWritten(t *testing.T) {
	sr := recordSpans(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "test.attempt")
	state := &State{Attempt: 1, Span: span}

	Finish(ctx, state, &http.Response{StatusCode: http.StatusServiceUnavailable}, true, true)

	ended := sr.Ended()[0]
	if _, present := spanAttrs(ended)["outcome"]; present {
		t.Error("an attempt span must never carry outcome — that lives on the parent call span")
	}
	if ended.Status().Code == codes.Error {
		t.Error("an attempt span must never have its span status set to Error")
	}
}

func TestFinish_NilStateIsNoop(t *testing.T) {
	Finish(context.Background(), nil, nil, false, false)
}

func TestFinish_NilSpanIsNoop(t *testing.T) {
	state := &State{}
	Finish(context.Background(), state, nil, false, false)
}

func TestFinish_IdempotentSecondCallDoesNotDoubleEnd(t *testing.T) {
	sr := recordSpans(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "test.attempt")
	state := &State{Attempt: 1, Span: span}

	Finish(ctx, state, &http.Response{StatusCode: http.StatusOK}, false, false)
	if got := len(sr.Ended()); got != 1 {
		t.Fatalf("got %d ended spans after first Finish, want 1", got)
	}
	// Second call: state.Span is already nil, so this must be a pure no-op.
	Finish(ctx, state, &http.Response{StatusCode: http.StatusOK}, false, false)
	if got := len(sr.Ended()); got != 1 {
		t.Errorf("got %d ended spans after a second Finish call, want 1", got)
	}
}

func TestState_CloseDangling_ExportsAnUndecidedSpan(t *testing.T) {
	sr := recordSpans(t)

	_, span := otel.Tracer("test").Start(context.Background(), "test.attempt")
	state := &State{Attempt: 1, Span: span}

	state.CloseDangling()

	if state.Span != nil {
		t.Error("Span is still parked after CloseDangling ran")
	}
	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("got %d ended spans, want 1 — an unended span is never exported at all", len(ended))
	}
	if _, present := spanAttrs(ended[0])["retry.decision"]; present {
		t.Error("retry.decision is set on a span the backstop closed; there was no decision to report")
	}

	// Idempotent: a second call must not end the same span twice.
	state.CloseDangling()
	if got := len(sr.Ended()); got != 1 {
		t.Errorf("got %d ended spans after a second CloseDangling call, want 1", got)
	}
}

func TestState_CloseDangling_NilSpanIsNoop(t *testing.T) {
	state := &State{}
	state.CloseDangling()
}

// Compile-time check that Transport satisfies the interface http.Client uses
// to type-assert CloseIdleConnections support, since nothing else in this
// package fails to build if that method is dropped by accident.
var _ interface{ CloseIdleConnections() } = (*Transport)(nil)

// Sanity check that Transport really implements http.RoundTripper — the
// production call sites rely on this via assignment to an http.Client's
// Transport field, which the compiler checks structurally rather than by
// name.
var _ http.RoundTripper = (*Transport)(nil)
