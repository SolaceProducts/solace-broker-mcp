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

package resilience

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/attemptspan"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// See internal/semp/sempv2/client_span_test.go for why the provider is
// installed exactly once per test binary: otel.Tracer handles obtained before
// the first SetTracerProvider (this package's own package-scoped `tracer`,
// resolved at init) delegate to that first provider and to no later one.
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
	// SetTracerProvider is honored, so it cannot be swapped per test), and
	// without this it accumulates across tests and across `go test -count`
	// iterations. Safe because nothing in this package calls t.Parallel.
	sharedSpanRecorder.Reset()
	return sharedSpanRecorder
}

// attemptSpans returns the ended `semp.attempt` spans belonging to traceID, in
// the order they ended.
//
// Scoped by trace rather than by name alone so a span left behind by another
// test — or by a goroutine that outlived one — cannot make these assertions
// pass or fail by accident.
func attemptSpans(sr *tracetest.SpanRecorder, traceID trace.TraceID) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == attemptSpanName && s.SpanContext().TraceID() == traceID {
			out = append(out, s)
		}
	}
	return out
}

// spanAttrs flattens a span's attributes for lookup.
func spanAttrs(s sdktrace.ReadOnlySpan) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

// runTracedRequest drives one Sender.Do under a parent span and returns that
// parent's span context (for scoping assertions to this trace) and Do's error.
//
// The parent stands in for the `semp.request` span the protocol clients start
// (Story 26); nothing here depends on those clients.
func runTracedRequest(t *testing.T, sender *Sender, req *http.Request) (trace.SpanContext, error) {
	t.Helper()
	ctx, parent := otel.Tracer("test").Start(req.Context(), "semp.request")
	defer parent.End()
	resp, err := sender.Do(ctx, req.WithContext(ctx))
	if resp != nil {
		_ = resp.Body.Close()
	}
	return parent.SpanContext(), err
}

// TestAttemptSpans_OnePerAttemptNestedUnderTheRequestSpan is the core
// acceptance criterion: a retried call produces one `semp.attempt` span per
// attempt, each a child of the request span, numbered 1..n, and carrying the
// status that attempt actually got.
//
// Before this story a call that retried twice was a single `semp.request` span:
// the attempt structure, and therefore which attempt succeeded, was invisible.
func TestAttemptSpans_OnePerAttemptNestedUnderTheRequestSpan(t *testing.T) {
	sr := recordSpans(t)

	var calls int
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonOK(w)
	}, "bearer", 10)
	defer server.Close()

	parentSC, err := runTracedRequest(t, sender, newGetRequest(t, server.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 3 {
		t.Fatalf("got %d %s spans, want 3 (one per attempt of a 503,503,200 chain)", len(spans), attemptSpanName)
	}

	wantStatus := []int64{
		http.StatusServiceUnavailable,
		http.StatusServiceUnavailable,
		http.StatusOK,
	}
	wantDecision := []bool{true, true, false}

	for i, s := range spans {
		if s.Parent().SpanID() != parentSC.SpanID() {
			t.Errorf("attempt %d: parent span ID = %v, want the semp.request span %v — an attempt span "+
				"that is not nested under the request span cannot be read as part of that call",
				i+1, s.Parent().SpanID(), parentSC.SpanID())
		}
		if got := s.SpanKind(); got != trace.SpanKindClient {
			t.Errorf("attempt %d: span kind = %v, want Client (an outbound broker call)", i+1, got)
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
		if _, present := attrs["retry.exhausted"]; present {
			t.Errorf("attempt %d: retry.exhausted is set, want absent — the chain succeeded well inside its "+
				"allowance, so nothing was exhausted", i+1)
		}
	}
}

// TestAttemptSpans_AllAttemptsShareTheIdenticalCorrelationID is the direct
// proof of the story's correlation criterion: the ID is resolved once per
// request and reused across retries, never regenerated per attempt.
//
// That identity is the whole point — it is what lets an operator take one ID
// out of a retry storm in the trace and grep every one of those attempts in the
// broker-side command log. Asserted as byte equality against the ID the caller
// put on the context, not merely as "an ID is present": a per-attempt
// regeneration would still leave every span carrying some ID.
func TestAttemptSpans_AllAttemptsShareTheIdenticalCorrelationID(t *testing.T) {
	sr := recordSpans(t)

	const wantID = "0198f1a2-3b4c-7d5e-8f90-a1b2c3d4e5f6"

	var calls int
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonOK(w)
	}, "bearer", 10)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	req = req.WithContext(correlation.With(req.Context(), wantID))

	parentSC, err := runTracedRequest(t, sender, req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 3 {
		t.Fatalf("got %d %s spans, want 3", len(spans), attemptSpanName)
	}
	for i, s := range spans {
		got, present := spanAttrs(s)["correlation_id"]
		if !present {
			t.Fatalf("attempt %d: no correlation_id attribute; the trace cannot be joined to the "+
				"request's logs or to the broker command log", i+1)
		}
		if got != wantID {
			t.Errorf("attempt %d: correlation_id = %q, want %q (identical on every attempt — the ID is "+
				"resolved once per request, never per attempt)", i+1, got, wantID)
		}
	}
}

// TestAttemptSpans_NoCorrelationIDAttributeWhenTheRequestCarriesNone pins the
// omit-rather-than-empty convention every other span and audit record follows,
// so a backend query for the attribute cannot match a request that never had
// an ID (correlation disabled).
func TestAttemptSpans_NoCorrelationIDAttributeWhenTheRequestCarriesNone(t *testing.T) {
	sr := recordSpans(t)

	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonOK(w)
	}, "bearer", 10)
	defer server.Close()

	parentSC, err := runTracedRequest(t, sender, newGetRequest(t, server.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1", len(spans), attemptSpanName)
	}
	if _, present := spanAttrs(spans[0])["correlation_id"]; present {
		t.Error("correlation_id is present on a request that carries no ID; it must be omitted, not written empty")
	}
}

// TestAttemptSpans_RetryDecisionIsCheckRetrysNotRederivedFromStatus is the
// mutation-proof for the story's "not re-derived" criterion.
//
// A POST is never replayed unless the caller marked it retry-safe, so a POST
// answered with 503 produces retry.decision=false against a status that says
// "retryable" as loudly as a status can. An implementation that read the status
// code at the span site instead of taking checkRetry's returned decision would
// report retry.decision=true here and claim a retry the Sender never performed.
func TestAttemptSpans_RetryDecisionIsCheckRetrysNotRederivedFromStatus(t *testing.T) {
	sr := recordSpans(t)

	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "bearer", 10)
	defer server.Close()

	parentSC, _ := runTracedRequest(t, sender, newMethodRequest(t, http.MethodPost, server.URL))

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1 (a POST is not replayed, so there is exactly one attempt)",
			len(spans), attemptSpanName)
	}
	attrs := spanAttrs(spans[0])
	if got := attrs["http.response.status_code"]; got != int64(http.StatusServiceUnavailable) {
		t.Fatalf("http.response.status_code = %v, want 503 (test precondition)", got)
	}
	if got := attrs["retry.decision"]; got != false {
		t.Errorf("retry.decision = %v, want false: checkRetry refused to replay a POST, so a span reporting "+
			"a retry decision of true is re-deriving the attribute from the 503 instead of reading the "+
			"decision the Sender acted on", got)
	}
	if _, present := attrs["retry.exhausted"]; present {
		t.Error("retry.exhausted is set on a request the policy refused to replay; nothing ran out — " +
			"the distinction between 'refused' and 'exhausted' is what an operator reads here")
	}
}

// TestAttemptSpans_ExhaustedOnTheTransientRetryCap pins retry.exhausted on the
// way a real broker-overload storm actually ends.
//
// maxTransientRetries caps a 429/503 episode far below RetryMax, so at default
// settings the cap — not RetryMax — is what stops the chain. If retry.exhausted
// only covered RetryMax, the retry storm this story exists to make legible
// would never carry the attribute.
func TestAttemptSpans_ExhaustedOnTheTransientRetryCap(t *testing.T) {
	sr := recordSpans(t)

	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "bearer", 10)
	defer server.Close()

	parentSC, err := runTracedRequest(t, sender, newGetRequest(t, server.URL))
	if err == nil {
		t.Fatal("Do returned no error; a 503 chain hitting the transient cap must fail")
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	// 1 initial + maxTransientRetries replays, then the cap refuses the next.
	if len(spans) != maxTransientRetries+1 {
		t.Fatalf("got %d %s spans, want %d (1 + maxTransientRetries)",
			len(spans), attemptSpanName, maxTransientRetries+1)
	}
	for i, s := range spans[:len(spans)-1] {
		if _, present := spanAttrs(s)["retry.exhausted"]; present {
			t.Errorf("attempt %d: retry.exhausted is set on a non-final attempt", i+1)
		}
	}
	final := spanAttrs(spans[len(spans)-1])
	if got := final["retry.exhausted"]; got != true {
		t.Errorf("final attempt: retry.exhausted = %v, want true (the transient-error cap stopped the chain)", got)
	}
	if got := final["retry.decision"]; got != false {
		t.Errorf("final attempt: retry.decision = %v, want false (the cap refused the replay)", got)
	}
}

// TestAttemptSpans_ExhaustedWhenRetryMaxRunsOut pins the other exhaustion case:
// checkRetry still says retry, but retryablehttp has no attempts left.
//
// retry.decision=true with retry.exhausted=true is the correct, and the only
// honest, pairing here — the decision recorded is the one the code made, and
// the exhaustion flag is what explains why no attempt followed it. A connection
// error is used because it retries on the full RetryMax rather than the tighter
// transient or once-only sub-caps.
func TestAttemptSpans_ExhaustedWhenRetryMaxRunsOut(t *testing.T) {
	sr := recordSpans(t)

	const retries = 2
	// A server that is closed before the request runs: every attempt is a
	// connection error, which the policy retries up to RetryMax.
	server := newClosedServer(t)
	sender := newTestSender(t, &http.Client{}, bearerAuth(t), retries)

	parentSC, err := runTracedRequest(t, sender, newGetRequest(t, server))
	if err == nil {
		t.Fatal("Do returned no error; every attempt hit a closed port")
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != retries+1 {
		t.Fatalf("got %d %s spans, want %d (RetryMax+1 attempts)", len(spans), attemptSpanName, retries+1)
	}
	final := spanAttrs(spans[len(spans)-1])
	if got := final["retry.decision"]; got != true {
		t.Errorf("final attempt: retry.decision = %v, want true — checkRetry did decide to retry a "+
			"connection error; the span must report that decision, not the fact that no retry followed", got)
	}
	if got := final["retry.exhausted"]; got != true {
		t.Errorf("final attempt: retry.exhausted = %v, want true (RetryMax ran out)", got)
	}
	if _, present := final["http.response.status_code"]; present {
		t.Error("http.response.status_code is set on an attempt that got no response at all")
	}
	for i, s := range spans[:len(spans)-1] {
		if _, present := spanAttrs(s)["retry.exhausted"]; present {
			t.Errorf("attempt %d: retry.exhausted is set while attempts remained", i+1)
		}
	}
}

// TestAttemptSpans_NotExhaustedWhenTheReplayWasRefused is the negative half of
// retry.exhausted, and it is the half a naive predicate gets wrong.
//
// A caller-declared non-idempotent request answered with a 503 ends the chain
// with a sentinel error, exactly like the transient cap does — so a predicate
// written as "checkRetry returned an error" reports exhaustion here. Nothing
// ran out: the policy refused to replay a request the broker may already have
// carried out. An operator seeing `retry.exhausted` would go hunting for a
// budget to raise when the request is simply not replayable.
func TestAttemptSpans_NotExhaustedWhenTheReplayWasRefused(t *testing.T) {
	sr := recordSpans(t)

	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "bearer", 10)
	defer server.Close()

	// PUT, so the method guard does not fire first — this exercises the
	// caller-declared non-idempotency gate specifically.
	req := newMethodRequest(t, http.MethodPut, server.URL)
	req = req.WithContext(WithRetryUnsafe(req.Context()))

	parentSC, err := runTracedRequest(t, sender, req)
	if err == nil {
		t.Fatal("Do returned no error; a gated non-idempotent 503 must fail")
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1 (the request is never replayed)", len(spans), attemptSpanName)
	}
	attrs := spanAttrs(spans[0])
	if got := attrs["retry.decision"]; got != false {
		t.Errorf("retry.decision = %v, want false", got)
	}
	if _, present := attrs["retry.exhausted"]; present {
		t.Error("retry.exhausted is set on a refused replay. Nothing ran out — the policy declined to " +
			"repeat a request the broker may already have carried out, and the remedy for that is not " +
			"a bigger retry budget")
	}
}

// TestAttemptSpans_NotExhaustedWhenTheContextEnded is the other negative case a
// "checkRetry returned an error" predicate would get wrong: a cancelled context
// makes checkRetry return ctx.Err(), which is an error but not a budget.
func TestAttemptSpans_NotExhaustedWhenTheContextEnded(t *testing.T) {
	sr := recordSpans(t)

	// Cancel while the handler is still deciding, so checkRetry's own ctx guard
	// is what ends the chain, on a status it would otherwise retry.
	cancelled := make(chan struct{})
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		close(cancelled)
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "bearer", 10)
	defer server.Close()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-cancelled
		cancel()
	}()

	req := newGetRequest(t, server.URL)
	parentSC, err := runTracedRequest(t, sender, req.WithContext(base))
	if err == nil {
		t.Fatal("Do returned no error; the caller's context was cancelled")
	}

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) == 0 {
		t.Fatalf("no %s spans", attemptSpanName)
	}
	for i, s := range spans {
		if _, present := spanAttrs(s)["retry.exhausted"]; present {
			t.Errorf("attempt %d: retry.exhausted is set on a call the caller abandoned; a context ending "+
				"is not a retry budget running out", i+1)
		}
	}
}

// TestAttemptSpans_ExhaustedOnTheOnceOnlyServerErrorCap covers the third retry
// allowance. A non-429/503 5xx is replayed once; the second one ends the chain
// with (false, nil) — no sentinel, no RetryMax involvement.
//
// Included because `retry.exhausted` means one thing: an allowance the policy
// enforces was already spent. A predicate keyed only on the transient sentinel
// and RetryMax would leave a 500,500 chain looking like a plain terminal
// failure, so an operator filtering for chains that ran out would miss it.
func TestAttemptSpans_ExhaustedOnTheOnceOnlyServerErrorCap(t *testing.T) {
	sr := recordSpans(t)

	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, "bearer", 10)
	defer server.Close()

	parentSC, _ := runTracedRequest(t, sender, newGetRequest(t, server.URL))

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 2 {
		t.Fatalf("got %d %s spans, want 2 (a non-429/503 5xx is replayed once)", len(spans), attemptSpanName)
	}
	if _, present := spanAttrs(spans[0])["retry.exhausted"]; present {
		t.Error("attempt 1: retry.exhausted is set while the once-only replay was still available")
	}
	final := spanAttrs(spans[1])
	if got := final["retry.decision"]; got != false {
		t.Errorf("attempt 2: retry.decision = %v, want false", got)
	}
	if got := final["retry.exhausted"]; got != true {
		t.Errorf("attempt 2: retry.exhausted = %v, want true (the once-only 5xx replay was spent)", got)
	}
}

// TestAttemptSpans_ExhaustedOnTheOnceOnly401Reauth covers the fourth allowance:
// a 401 that persists after the one re-auth attempt.
//
// Distinct from an authenticator that declines the FIRST 401 — that is "cannot
// recover", not a budget running out, and TestAttemptSpans_NotExhausted... does
// not cover it because no allowance was ever spent there.
func TestAttemptSpans_ExhaustedOnTheOnceOnly401Reauth(t *testing.T) {
	sr := recordSpans(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	// rotatingTokenAuth signals Retry+ReAuth, so the one re-auth allowance is
	// used and the persisted 401 then ends the chain.
	sender := newTestSender(t, server.Client(), &rotatingTokenAuth{}, 10)
	sender.brokerURL = server.URL

	parentSC, _ := runTracedRequest(t, sender, newGetRequest(t, server.URL))

	spans := attemptSpans(sr, parentSC.TraceID())
	if len(spans) != 2 {
		t.Fatalf("got %d %s spans, want 2 (one 401, one re-auth replay)", len(spans), attemptSpanName)
	}
	if _, present := spanAttrs(spans[0])["retry.exhausted"]; present {
		t.Error("attempt 1: retry.exhausted is set while the re-auth allowance was still available")
	}
	if got := spanAttrs(spans[1])["retry.exhausted"]; got != true {
		t.Errorf("attempt 2: retry.exhausted = %v, want true (the once-only 401 re-auth was spent)", got)
	}
}

// TestRetriesExhausted_Predicate pins the predicate directly, both directions,
// so the two meanings cannot be collapsed by a later edit.
func TestRetriesExhausted_Predicate(t *testing.T) {
	cases := []struct {
		name           string
		retryMax       int
		attempt        int
		retry          bool
		allowanceSpent bool
		want           bool
	}{
		{"retrying with attempts left", 10, 1, true, false, false},
		{"retrying with none left", 2, 3, true, false, true},
		{"retrying past the end", 2, 4, true, false, true},
		{"not retrying, allowance spent", 10, 4, false, true, true},
		{"not retrying, nothing spent", 10, 1, false, false, false},
		// A spent allowance is what matters, not the attempt number: the
		// 429/503 cap fires at attempt 4 of a RetryMax of 10.
		{"spent allowance well inside RetryMax", 10, 4, false, true, true},
		// retry=true takes the attempt-count branch, so a spent allowance
		// cannot mask an available attempt.
		{"retrying with attempts left despite a spent allowance", 10, 1, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := retriesExhausted(tc.retryMax, tc.attempt, tc.retry, tc.allowanceSpent)
			if got != tc.want {
				t.Errorf("retriesExhausted(%d, %d, %v, %v) = %v, want %v",
					tc.retryMax, tc.attempt, tc.retry, tc.allowanceSpent, got, tc.want)
			}
		})
	}
}

// TestNew_DoesNotMutateTheCallersHTTPClient pins that New composes its transport
// wrappers onto a copy.
//
// Two properties depend on it. A caller that installed its own RoundTripper
// keeps the client it configured; and two Senders over one *http.Client each get
// their own complete chain, rather than the second silently inheriting the
// first's — which, if only the second carried WithMetrics, would drop that
// Sender's SEMP metrics entirely.
func TestNew_DoesNotMutateTheCallersHTTPClient(t *testing.T) {
	callerTransport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	})
	httpClient := &http.Client{Transport: callerTransport, Timeout: 7 * time.Second}

	first := newTestSender(t, httpClient, bearerAuth(t), 1)
	second := newTestSender(t, httpClient, bearerAuth(t), 1)

	if _, wrapped := httpClient.Transport.(*attemptspan.Transport); wrapped {
		t.Error("New replaced the caller's own Transport; it must wrap a copy of the client instead")
	}

	// Each Sender got its own attempt-span transport over the caller's
	// transport — one layer each, not one nested inside the other.
	for name, d := range map[string]*Sender{"first": first, "second": second} {
		at, ok := d.retryClient.HTTPClient.Transport.(*attemptspan.Transport)
		if !ok {
			t.Fatalf("%s Sender: retry client transport is %T, want *attemptspan.Transport", name, d.retryClient.HTTPClient.Transport)
		}
		if _, nested := at.Base.(*attemptspan.Transport); nested {
			t.Errorf("%s Sender: the attempt-span transport is nested inside another; every attempt would be "+
				"counted twice and carry two spans", name)
		}
		if d.retryClient.HTTPClient.Timeout != httpClient.Timeout {
			t.Errorf("%s Sender: per-attempt Timeout = %s, want the caller's %s — the copy must carry it",
				name, d.retryClient.HTTPClient.Timeout, httpClient.Timeout)
		}
	}
}

// TestAttemptTransport_ForwardsCloseIdleConnections pins the passthrough.
//
// http.Client.CloseIdleConnections reaches the transport by type assertion, so
// a wrapper without the method makes the call a silent no-op — and retryablehttp
// calls it on every failure path to stop a chain that just went wrong from
// leaving connections in the pool. Because the attempt-span transport is
// installed unconditionally, losing this would disable that hygiene on the
// default configuration for every broker, with nothing failing.
func TestAttemptTransport_ForwardsCloseIdleConnections(t *testing.T) {
	var closed int
	inner := &closeIdleRecorder{closed: &closed}

	// The full production chain: the attempt-span transport over
	// metricsTransport over the real transport. Both wrappers have to relay
	// or the chain breaks.
	tr := newAttemptTransport(&metricsTransport{base: inner})
	client := &http.Client{Transport: tr}
	client.CloseIdleConnections()

	if closed != 1 {
		t.Errorf("inner transport saw %d CloseIdleConnections calls, want 1; the wrapper chain swallowed it, "+
			"so retryablehttp's post-failure connection hygiene is a no-op", closed)
	}
}

// closeIdleRecorder is a RoundTripper that counts CloseIdleConnections calls,
// standing in for the real *http.Transport at the bottom of the chain.
type closeIdleRecorder struct {
	closed *int
}

func (c *closeIdleRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
}

func (c *closeIdleRecorder) CloseIdleConnections() { *c.closed++ }

// TestAttemptNumber_AgreesWithTheMetricLabelSource pins the single-owner
// invariant the story's technical notes require: the attempt number on the span
// and the `attempt` metric label come from one counter, so they cannot drift.
//
// The counter used to be bumped by metricsTransport, which is installed only
// when metrics are on — so tracing alone would have reported every attempt as
// attempt 1. The attempt-span transport owns it now and is always installed.
func TestAttemptNumber_AgreesWithTheMetricLabelSource(t *testing.T) {
	state := &retryState{}
	ctx := context.WithValue(context.Background(), retryStateKey{}, state)

	if got := attemptNumber(ctx); got != 1 {
		t.Errorf("attemptNumber before any attempt = %d, want 1", got)
	}
	state.spanState.Attempt = 3
	if got := attemptNumber(ctx); got != 3 {
		t.Errorf("attemptNumber = %d, want 3 (the counter's value, not a fresh bump)", got)
	}
	// Reading must not advance the counter: two readers (the span and the
	// metric) share it, and a bump-on-read would make them disagree.
	if got := attemptNumber(ctx); got != 3 {
		t.Errorf("attemptNumber on a second read = %d, want 3 — reading must not increment", got)
	}
	if got := attemptNumber(context.Background()); got != 1 {
		t.Errorf("attemptNumber with no retry state = %d, want 1", got)
	}
}

// TestAttemptSpans_UntracedWhenSenderDoIsBypassed pins that a request reaching
// the transport without Sender.Do's per-request state is passed through rather
// than given a span whose attempt number and retry decision nothing will fill
// in.
func TestAttemptSpans_UntracedWhenSenderDoIsBypassed(t *testing.T) {
	sr := recordSpans(t)

	var reached bool
	tr := newAttemptTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		reached = true
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}))

	ctx, parent := otel.Tracer("test").Start(context.Background(), "semp.request")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://broker.invalid/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	_ = resp.Body.Close()
	parent.End()

	if !reached {
		t.Fatal("the base transport was not called")
	}
	if got := attemptSpans(sr, parent.SpanContext().TraceID()); len(got) != 0 {
		t.Errorf("got %d %s spans, want 0 for a request that bypassed Sender.Do", len(got), attemptSpanName)
	}
}

// TestAttemptSpans_RedirectHopsAreNotNewAttempts pins the redirect guard. A hop
// is not a retry attempt, and a second Start for one attempt would overwrite
// the parked span handle and leak the first span — which is why the guard is a
// correctness requirement here, not just label hygiene.
func TestAttemptSpans_RedirectHopsAreNotNewAttempts(t *testing.T) {
	sr := recordSpans(t)

	state := &retryState{}
	tr := newAttemptTransport(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: req}, nil
	}))

	ctx, parent := otel.Tracer("test").Start(
		context.WithValue(context.Background(), retryStateKey{}, state), "semp.request")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://broker.invalid/test", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A redirect hop is signalled by a non-nil req.Response, the same marker
	// metricsTransport reads.
	req.Response = &http.Response{StatusCode: http.StatusFound}

	resp, err := tr.RoundTrip(req)
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

// TestCloseDanglingAttemptSpan_ExportsAnUndecidedSpan pins the backstop
// Sender.Do defers. The path is unreachable on today's retryablehttp (CheckRetry
// runs after every dispatch), so this drives it directly: an unended span is
// dropped by the SDK entirely, so the backstop's job is to make the attempt
// visible even with no decision to report.
func TestCloseDanglingAttemptSpan_ExportsAnUndecidedSpan(t *testing.T) {
	sr := recordSpans(t)

	ctx, parent := otel.Tracer("test").Start(context.Background(), "semp.request")
	_, span := tracer.Start(ctx, attemptSpanName)
	state := &retryState{spanState: attemptspan.State{Attempt: 1, Span: span}}

	state.closeDanglingAttemptSpan()
	parent.End()

	if state.spanState.Span != nil {
		t.Error("attemptSpan is still parked after the backstop ran")
	}
	spans := attemptSpans(sr, parent.SpanContext().TraceID())
	if len(spans) != 1 {
		t.Fatalf("got %d %s spans, want 1 — an unended span is never exported at all", len(spans), attemptSpanName)
	}
	if _, present := spanAttrs(spans[0])["retry.decision"]; present {
		t.Error("retry.decision is set on a span the backstop closed; there was no decision to report, " +
			"and inventing one is the re-derivation this design forbids")
	}
	// Idempotent: a second call must not end the same span twice.
	state.closeDanglingAttemptSpan()
	if got := attemptSpans(sr, parent.SpanContext().TraceID()); len(got) != 1 {
		t.Errorf("got %d spans after a second backstop call, want 1", len(got))
	}
}

// TestSenderDo_LeavesNoAttemptSpanOpen pins the invariant end to end: after a
// retried call returns, nothing is left parked on the request state.
func TestSenderDo_LeavesNoAttemptSpanOpen(t *testing.T) {
	sr := recordSpans(t)

	var calls int
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonOK(w)
	}, "bearer", 10)
	defer server.Close()

	parentSC, err := runTracedRequest(t, sender, newGetRequest(t, server.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}

	// Every span that was started must also have ended, which for the SDK's
	// recorder means Started() and Ended() agree for this trace.
	var started, ended int
	for _, s := range sr.Started() {
		if s.Name() == attemptSpanName && s.SpanContext().TraceID() == parentSC.TraceID() {
			started++
		}
	}
	ended = len(attemptSpans(sr, parentSC.TraceID()))
	if started != ended || started != 2 {
		t.Errorf("started=%d ended=%d %s spans, want 2 and 2 — an unended span never reaches a collector",
			started, ended, attemptSpanName)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newClosedServer brings a server up to obtain a URL, then closes it, so every
// connection attempt to that URL is refused at the transport rather than
// returning a status. Same pattern as the connection-error tests in
// sender_test.go.
func newClosedServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}
