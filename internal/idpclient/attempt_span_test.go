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

package idpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/attemptspan"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// The tracer provider is installed exactly once per test binary: otel.Tracer
// handles obtained before the first SetTracerProvider — this package's own
// package-scoped `tracer`, resolved at init — delegate to that first provider
// and to no later one. Same constraint, same shape, as the span tests in
// internal/tokenexchange and internal/semp/resilience.
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
	// Reset per test: the recorder is shared (only the first
	// SetTracerProvider is honored), and without this it accumulates across
	// tests and across `go test -count` runs. Safe because these tests do not
	// call t.Parallel.
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

// attemptSpans returns the ended `tokenexchange.attempt` spans in traceID, in
// the order they ended. Scoped by trace so a span from another test cannot
// affect these assertions.
func attemptSpans(sr *tracetest.SpanRecorder, traceID oteltrace.TraceID) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == attemptSpanName && s.SpanContext().TraceID() == traceID {
			out = append(out, s)
		}
	}
	return out
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

// fastRetryOptions keeps the backoff short so a retrying test does not sit for
// seconds. MaxRetries matches testRetryOptions so the "3 attempts total"
// invariant is the same one the rest of this package's tests assert.
func fastRetryOptions() RetryOptions {
	return RetryOptions{
		MaxRetries:   2,
		RetryWaitMin: 1 * time.Millisecond,
		RetryWaitMax: 5 * time.Millisecond,
	}
}

// TestAttemptSpans_OnePerAttemptNestedUnderTheCallersSpan is the direct
// acceptance proof for the token-exchange half of the story: each HTTP attempt
// of a retried IdP call gets its own span, nested under the span active at the
// call site, carrying the attempt number, the status it got, and the retry
// decision this client's own checkRetry returned.
func TestAttemptSpans_OnePerAttemptNestedUnderTheCallersSpan(t *testing.T) {
	sr := recordSpans(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewRetryingHTTPClient(fastRetryOptions())
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	// Stands in for the `tokenexchange.Exchange` span (Story 50) the real
	// caller re-attaches onto the detached exchange context.
	ctx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != 3 {
		t.Fatalf("got %d %s spans, want 3 (one per attempt of a 500,500,200 chain)", len(spans), attemptSpanName)
	}

	wantStatus := []int64{
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusOK,
	}
	wantDecision := []bool{true, true, false}

	for i, s := range spans {
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("attempt %d: parent span ID = %v, want the exchange span %v",
				i+1, s.Parent().SpanID(), parent.SpanContext().SpanID())
		}
		if got := s.SpanKind(); got != oteltrace.SpanKindClient {
			t.Errorf("attempt %d: span kind = %v, want Client (an outbound IdP call)", i+1, got)
		}
		attrs := spanAttrs(s)
		if got, want := attrs["attempt"], int64(i+1); got != want {
			t.Errorf("attempt %d: attempt attribute = %v, want %v", i+1, got, want)
		}
		if got := attrs["http.response.status_code"]; got != wantStatus[i] {
			t.Errorf("attempt %d: http.response.status_code = %v, want %v", i+1, got, wantStatus[i])
		}
		if got := attrs["retry.decision"]; got != wantDecision[i] {
			t.Errorf("attempt %d: retry.decision = %v, want %v", i+1, got, wantDecision[i])
		}
	}
	if got := spanAttrs(spans[2])["retry.exhausted"]; got != nil {
		t.Errorf("final attempt: retry.exhausted = %v, want absent — the chain succeeded on attempt 3 of 3, "+
			"nothing ran out", got)
	}
}

// TestAttemptSpans_ExhaustedOnTheFinalAttempt pins retry.exhausted: checkRetry
// still says "retry" on the last 500, and retryablehttp has no attempts left.
//
// retry.decision=true beside retry.exhausted=true is the correct pairing. The
// decision recorded is the one the policy made; the exhaustion flag is what
// explains why no attempt followed it.
func TestAttemptSpans_ExhaustedOnTheFinalAttempt(t *testing.T) {
	sr := recordSpans(t)

	handler, hits := countingHandler(t, http.StatusInternalServerError, "", nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	opts := fastRetryOptions()
	client, err := NewRetryingHTTPClient(opts)
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	ctx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	if got, want := int(hits.Load()), opts.MaxRetries+1; got != want {
		t.Fatalf("server saw %d attempts, want %d (test precondition)", got, want)
	}

	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != opts.MaxRetries+1 {
		t.Fatalf("got %d %s spans, want %d", len(spans), attemptSpanName, opts.MaxRetries+1)
	}
	for i, s := range spans[:len(spans)-1] {
		if _, present := spanAttrs(s)["retry.exhausted"]; present {
			t.Errorf("attempt %d: retry.exhausted is set while attempts remained", i+1)
		}
	}
	final := spanAttrs(spans[len(spans)-1])
	if got := final["retry.decision"]; got != true {
		t.Errorf("final attempt: retry.decision = %v, want true (checkRetry did decide to retry the 500)", got)
	}
	if got := final["retry.exhausted"]; got != true {
		t.Errorf("final attempt: retry.exhausted = %v, want true (MaxRetries ran out)", got)
	}
}

// TestAttemptSpans_RetryDecisionIsCheckRetrysNotRederivedFromStatus is the
// mutation-proof for the "not re-derived" criterion on this path.
//
// The context is already done when the response arrives, so checkRetry returns
// no-retry on a 500 — the one status this policy otherwise always retries. An
// implementation that read the status code at the span site would report
// retry.decision=true and claim a retry the client never performed.
func TestAttemptSpans_RetryDecisionIsCheckRetrysNotRederivedFromStatus(t *testing.T) {
	sr := recordSpans(t)

	cancelled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Cancel the caller's context, then answer with the most retryable
		// status there is. checkRetry's ctx guard runs first and wins.
		close(cancelled)
		<-time.After(20 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := NewRetryingHTTPClient(fastRetryOptions())
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	baseCtx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()
	go func() {
		<-cancelled
		cancel()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	parent.End()

	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1 (a cancelled context is never retried)", len(spans), attemptSpanName)
	}
	attrs := spanAttrs(spans[0])
	if got := attrs["retry.decision"]; got != false {
		t.Errorf("retry.decision = %v, want false: the context was already done, so checkRetry refused to "+
			"retry. A span reporting true is re-deriving the attribute from the response instead of "+
			"reading the decision the client acted on", got)
	}
	if _, present := attrs["retry.exhausted"]; present {
		t.Error("retry.exhausted is set on a call the caller cancelled; nothing ran out")
	}
}

// TestAttemptSpans_CarryTheContextCorrelationID pins the join key. On this path
// the ID on the context is the singleflight WINNER's (see attemptSpanName's
// documented limitation), so what is asserted is that every attempt of one
// exchange carries the identical ID the context held — not that it belongs to
// any particular caller.
func TestAttemptSpans_CarryTheContextCorrelationID(t *testing.T) {
	sr := recordSpans(t)

	const wantID = "0198f1a2-3b4c-7d5e-8f90-a1b2c3d4e5f6"

	handler, _ := countingHandler(t, http.StatusInternalServerError, "", nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client, err := NewRetryingHTTPClient(fastRetryOptions())
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	ctx, parent := otel.Tracer("test").Start(
		correlation.With(context.Background(), wantID), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) == 0 {
		t.Fatalf("no %s spans", attemptSpanName)
	}
	for i, s := range spans {
		got, present := spanAttrs(s)["correlation_id"]
		if !present {
			t.Fatalf("attempt %d: no correlation_id attribute", i+1)
		}
		if got != wantID {
			t.Errorf("attempt %d: correlation_id = %q, want %q (identical on every attempt)", i+1, got, wantID)
		}
	}
}

// TestAttemptSpans_NoCorrelationIDAttributeWhenAbsent pins the
// omit-rather-than-empty convention, so a backend query for the attribute
// cannot match an exchange that never had an ID.
func TestAttemptSpans_NoCorrelationIDAttributeWhenAbsent(t *testing.T) {
	sr := recordSpans(t)

	handler, _ := countingHandler(t, http.StatusOK, "", nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client, err := NewRetryingHTTPClient(fastRetryOptions())
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	ctx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1", len(spans), attemptSpanName)
	}
	if _, present := spanAttrs(spans[0])["correlation_id"]; present {
		t.Error("correlation_id is present with no ID on the context; it must be omitted, not written empty")
	}
}

// TestAttemptSpans_ConnectionErrorHasNoStatusAttribute pins that an attempt
// which got no response at all reports no status, rather than a synthetic zero
// an operator would read as a real code.
func TestAttemptSpans_ConnectionErrorHasNoStatusAttribute(t *testing.T) {
	sr := recordSpans(t)

	// Up then down, so every dial is refused.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	opts := fastRetryOptions()
	client, err := NewRetryingHTTPClient(opts)
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	ctx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do returned no error against a closed port")
	}
	parent.End()

	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != opts.MaxRetries+1 {
		t.Fatalf("got %d %s spans, want %d (a connection error retries on the full allowance)",
			len(spans), attemptSpanName, opts.MaxRetries+1)
	}
	for i, s := range spans {
		if _, present := spanAttrs(s)["http.response.status_code"]; present {
			t.Errorf("attempt %d: http.response.status_code is set on an attempt that got no response", i+1)
		}
	}
	if got := spanAttrs(spans[len(spans)-1])["retry.exhausted"]; got != true {
		t.Errorf("final attempt: retry.exhausted = %v, want true", got)
	}
}

// TestAttemptSpans_RedirectHopsAreNotNewAttempts pins the redirect guard on the
// per-attempt wrapper. A hop is not a retry attempt, and a second Start for one
// attempt would overwrite the parked handle and leak the first span — so the
// guard is a correctness requirement, not label hygiene.
func TestAttemptSpans_RedirectHopsAreNotNewAttempts(t *testing.T) {
	sr := recordSpans(t)

	state := &attemptSpanState{retryMax: 2}
	rec := newAttemptSpanTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}))

	ctx, parent := otel.Tracer("test").Start(
		context.WithValue(context.Background(), attemptSpanKey{}, state), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://idp.invalid/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Response = &http.Response{StatusCode: http.StatusFound}

	resp, err := rec.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	if state.spanState.Attempt != 0 {
		t.Errorf("attempt counter = %d after a redirect hop, want 0", state.spanState.Attempt)
	}
	if state.spanState.Span != nil {
		t.Error("a redirect hop parked an attempt span; a second one would overwrite and leak it")
	}
	if got := attemptSpans(sr, parent.SpanContext().TraceID()); len(got) != 0 {
		t.Errorf("got %d %s spans for a redirect hop, want 0", len(got), attemptSpanName)
	}
}

// TestAttemptSpans_UntracedWithoutSeededState pins that a request reaching the
// per-attempt wrapper without the seeded per-request state — retryablehttp
// wired by hand, bypassing NewRetryingHTTPClient — is passed through rather
// than given a span nothing will ever close.
func TestAttemptSpans_UntracedWithoutSeededState(t *testing.T) {
	sr := recordSpans(t)

	var reached bool
	rec := newAttemptSpanTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reached = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}))

	ctx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://idp.invalid/token", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rec.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	if !reached {
		t.Fatal("the inner transport was not called")
	}
	if got := attemptSpans(sr, parent.SpanContext().TraceID()); len(got) != 0 {
		t.Errorf("got %d %s spans without seeded state, want 0", len(got), attemptSpanName)
	}
}

// TestCloseDanglingSpan_ExportsAnUndecidedSpan pins the backstop the seeder
// defers. Unreachable on today's retryablehttp (CheckRetry runs after every
// dispatch), so this drives it directly: an unended span is dropped by the SDK
// entirely, so the backstop's job is to keep the attempt in the trace even with
// no decision to report.
func TestCloseDanglingSpan_ExportsAnUndecidedSpan(t *testing.T) {
	sr := recordSpans(t)

	ctx, parent := otel.Tracer("test").Start(context.Background(), "tokenexchange.Exchange")
	_, span := tracer.Start(ctx, attemptSpanName)
	state := &attemptSpanState{spanState: attemptspan.State{Attempt: 1, Span: span}}

	state.closeDanglingSpan()
	parent.End()

	if state.spanState.Span != nil {
		t.Error("span is still parked after the backstop ran")
	}
	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1 — an unended span is never exported", len(spans), attemptSpanName)
	}
	if _, present := spanAttrs(spans[0])["retry.decision"]; present {
		t.Error("retry.decision is set on a span the backstop closed; there was no decision to report")
	}
	// Idempotent: a second call must not end the same span twice.
	state.closeDanglingSpan()
	if got := attemptSpans(sr, parent.SpanContext().TraceID()); len(got) != 1 {
		t.Errorf("got %d spans after a second backstop call, want 1", len(got))
	}
}

// TestRetriesExhausted_ReadsTheDecisionNotTheOutcome pins the predicate
// directly, including the case a re-derivation would get wrong: a decision NOT
// to retry is never exhaustion on this path, whatever the attempt number.
func TestRetriesExhausted_ReadsTheDecisionNotTheOutcome(t *testing.T) {
	cases := []struct {
		name     string
		retryMax int
		attempt  int
		retry    bool
		want     bool
	}{
		{"first of three, retrying", 2, 1, true, false},
		{"last of three, retrying", 2, 3, true, true},
		{"last of three, not retrying", 2, 3, false, false},
		{"no retries configured, retrying", 0, 1, true, true},
		{"no retries configured, not retrying", 0, 1, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retriesExhausted(tc.retryMax, tc.attempt, tc.retry); got != tc.want {
				t.Errorf("retriesExhausted(%d, %d, %v) = %v, want %v",
					tc.retryMax, tc.attempt, tc.retry, got, tc.want)
			}
		})
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
