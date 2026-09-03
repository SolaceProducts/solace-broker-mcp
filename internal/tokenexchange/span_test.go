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

package tokenexchange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// forwardingProcessor exists solely so tests can swap "the current
// recorder" without ever calling otel.SetTracerProvider more than once.
//
// otel.Tracer(name) resolves GetTracerProvider() once, at the moment it's
// called — including for this package's own package-scoped `tracer` var
// (exchange.go), which resolves at package-init time, before any test runs.
// Only the FIRST SetTracerProvider call in a process is honored by handles
// already obtained before it; every later call is silently ignored for
// them. A naive per-test "install a fresh tracer provider, defer restore
// the old one" helper works for the FIRST test in the package and silently
// captures nothing for every test after it — proven while writing this
// file: the second test using such a helper found zero spans. One real
// provider, installed once, with a swappable recorder inside it, is the
// correct fix.
type forwardingProcessor struct {
	mu   sync.Mutex
	next sdktrace.SpanProcessor
}

func (p *forwardingProcessor) OnStart(ctx context.Context, s sdktrace.ReadWriteSpan) {
	p.mu.Lock()
	next := p.next
	p.mu.Unlock()
	if next != nil {
		next.OnStart(ctx, s)
	}
}

func (p *forwardingProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	p.mu.Lock()
	next := p.next
	p.mu.Unlock()
	if next != nil {
		next.OnEnd(s)
	}
}

func (p *forwardingProcessor) Shutdown(context.Context) error   { return nil }
func (p *forwardingProcessor) ForceFlush(context.Context) error { return nil }

var (
	sharedTestProcessor = &forwardingProcessor{}
	installSharedTracer sync.Once
)

// withRecordingTracer installs a real, always-sampling tracer provider
// globally exactly once for the whole test binary, and returns a fresh
// recorder wired as this test's target for that shared provider's spans —
// see forwardingProcessor's doc for why it has to work this way. Not
// parallel-safe with any other test that also calls withRecordingTracer:
// it swaps sharedTestProcessor.next with no synchronization against a
// concurrent test's own swap or its span assertions. Enforced today only by
// convention (none of these tests call t.Parallel()), not by a lock — a
// test that leaves a detached background call running past its own return
// (the way TestExchange_DetachedCallWarmsCacheAfterCallerBails in
// exchange_test.go deliberately does) is a real hazard the moment such a
// call creates its own span (Story 27, not yet built): that span would land
// in whichever recorder is installed by then, not necessarily this test's.
func withRecordingTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	installSharedTracer.Do(func() {
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithSpanProcessor(sharedTestProcessor),
			sdktrace.WithSampler(sdktrace.AlwaysSample()),
		)
		otel.SetTracerProvider(tp)
	})

	sr := tracetest.NewSpanRecorder()
	sharedTestProcessor.mu.Lock()
	sharedTestProcessor.next = sr
	sharedTestProcessor.mu.Unlock()
	t.Cleanup(func() {
		sharedTestProcessor.mu.Lock()
		sharedTestProcessor.next = nil
		sharedTestProcessor.mu.Unlock()
	})
	return sr
}

// findSpan returns the one ended span named name, failing the test if there
// isn't exactly one.
func findSpan(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d spans named %q, want exactly 1 (all ended spans: %v)", len(found), name, sr.Ended())
	}
	return found[0]
}

// attr returns the value of key on span's attributes, failing the test if
// it's absent.
func attr(t *testing.T, span sdktrace.ReadOnlySpan, key string) any {
	t.Helper()
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.AsInterface()
		}
	}
	t.Fatalf("span %q has no %q attribute (attributes: %v)", span.Name(), key, span.Attributes())
	return nil
}

// hasAttr reports whether key is present on span's attributes.
func hasAttr(span sdktrace.ReadOnlySpan, key string) bool {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return true
		}
	}
	return false
}

// hasExceptionEvent reports whether span.RecordError added an "exception"
// event to span.
func hasExceptionEvent(span sdktrace.ReadOnlySpan) bool {
	for _, ev := range span.Events() {
		if ev.Name == "exception" {
			return true
		}
	}
	return false
}

// TestExchange_Span_LiveCallIsChildOfCallersActiveSpan is the direct AC
// proof: "a trace including a live (non-cached) token-exchange call has a
// distinct child span nested under the SEMP-layer span active at the call
// site." A manually-started span stands in for the SEMP-attempt span Story
// 26 will create; nothing here depends on Story 26 having landed.
func TestExchange_Span_LiveCallIsChildOfCallersActiveSpan(t *testing.T) {
	sr := withRecordingTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-tok", 3600))
	}))
	defer srv.Close()
	e := newTestExchanger(t, srv.URL)

	parentCtx, parentSpan := otel.Tracer("test").Start(context.Background(), "semp.attempt")
	tok, err := e.Exchange(parentCtx, validInput())
	parentSpan.End()
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok == nil {
		t.Fatal("tok = nil, want non-nil")
	}

	exchangeSpan := findSpan(t, sr, "tokenexchange.Exchange")
	if got, want := exchangeSpan.Parent().SpanID(), parentSpan.SpanContext().SpanID(); got != want {
		t.Errorf("exchange span's parent SpanID = %s, want %s (the caller's active span)", got, want)
	}
	if exchangeSpan.Parent().TraceID() != parentSpan.SpanContext().TraceID() {
		t.Error("exchange span is not in the same trace as the caller's active span")
	}
}

// TestExchange_Span_LiveCall pins cache_hit=false and outcome=success on a
// real (non-cached) round trip, that error_type is deliberately absent (see
// Exchange's doc comment for why), and that correlation_id joins the span
// to the request's logs and audit records.
func TestExchange_Span_LiveCall(t *testing.T) {
	sr := withRecordingTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-tok", 3600))
	}))
	defer srv.Close()
	e := newTestExchanger(t, srv.URL)

	ctx := correlation.With(context.Background(), "corr-live-call")
	if _, err := e.Exchange(ctx, validInput()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	span := findSpan(t, sr, "tokenexchange.Exchange")
	if got := attr(t, span, "cache_hit"); got != false {
		t.Errorf("cache_hit = %v, want false", got)
	}
	if got := attr(t, span, "outcome"); got != "success" {
		t.Errorf("outcome = %v, want %q", got, "success")
	}
	if got := attr(t, span, "correlation_id"); got != "corr-live-call" {
		t.Errorf("correlation_id = %v, want %q", got, "corr-live-call")
	}
	if hasAttr(span, "error_type") {
		t.Error("error_type is present on a token-exchange span; it should never be set (see Exchange's doc comment)")
	}
}

// TestExchange_Span_CacheHit pins cache_hit=true when the cache serves the
// call without any IdP round trip.
func TestExchange_Span_CacheHit(t *testing.T) {
	sr := withRecordingTracer(t)

	e := newTestExchanger(t, "http://unused.invalid")
	input := validInput()
	key := computeDeduplicationKey(input.DedupKeyInput())
	pr, err := e.cache.Put(context.Background(), key, cache.CachedCredential{
		Value: "cached-tok",
		// Real wall-clock time, not pinnedNow(): the otter cache backend's TTL
		// derivation (deriveTTL) compares ExpiresAt against time.Now() directly,
		// not e.nowFunc(), so a pinned-clock timestamp in the past would make
		// Put silently drop the entry as already-expired (PutDroppedTTL) — the
		// exact bug caught while writing this test.
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	if pr.Status != cache.PutStored {
		t.Fatalf("seed cache: Put status = %v, want PutStored — the entry never made it into the cache", pr.Status)
	}

	tok, err := e.Exchange(context.Background(), input)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok.Value != "cached-tok" {
		t.Fatalf("tok.Value = %q, want %q (expected the cache hit, not a live call)", tok.Value, "cached-tok")
	}

	span := findSpan(t, sr, "tokenexchange.Exchange")
	if got := attr(t, span, "cache_hit"); got != true {
		t.Errorf("cache_hit = %v, want true", got)
	}
	if got := attr(t, span, "outcome"); got != "success" {
		t.Errorf("outcome = %v, want %q", got, "success")
	}
}

// TestExchange_Span_ErrorRecordsOutcomeAndException pins outcome=error and
// that the span carries the failure via RecordError when the IdP rejects
// the exchange.
func TestExchange_Span_ErrorRecordsOutcomeAndException(t *testing.T) {
	sr := withRecordingTracer(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer srv.Close()
	e := newTestExchanger(t, srv.URL)

	if _, err := e.Exchange(context.Background(), validInput()); err == nil {
		t.Fatal("Exchange() error = nil, want an error from the rejected exchange")
	}

	span := findSpan(t, sr, "tokenexchange.Exchange")
	if got := attr(t, span, "outcome"); got != "error" {
		t.Errorf("outcome = %v, want %q", got, "error")
	}
	if !hasExceptionEvent(span) {
		t.Error("span has no recorded exception event; RecordError should have added one")
	}
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("span status code = %v, want codes.Error — trace backends key error views off span status, not RecordError alone", got)
	}
}

// TestExchange_Span_CancelledCallerClassifiesAsCancelled pins that a caller
// whose context is already done gets outcome=cancelled, not outcome=error —
// the exchange itself did nothing wrong, the caller left.
func TestExchange_Span_CancelledCallerClassifiesAsCancelled(t *testing.T) {
	sr := withRecordingTracer(t)

	e := newTestExchanger(t, "http://unused.invalid")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := e.Exchange(ctx, validInput()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Exchange() error = %v, want context.Canceled", err)
	}

	span := findSpan(t, sr, "tokenexchange.Exchange")
	if got := attr(t, span, "outcome"); got != "cancelled" {
		t.Errorf("outcome = %v, want %q", got, "cancelled")
	}
	// The stated rule (Exchange's doc comment): SetStatus(codes.Error, ...)
	// fires only when outcome is "error", never on "cancelled" — making
	// SetStatus unconditional would leave this test green without this
	// assertion (flagged by review).
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("span status code = %v, want codes.Unset — SetStatus must not be called on a cancelled span", got)
	}
	// A cancelled call is the caller leaving, not the exchange failing — an
	// exception event on this span would read as an error in trace-backend
	// UIs that key off event presence rather than span status (flagged by
	// review).
	if hasExceptionEvent(span) {
		t.Error("span has a recorded exception event; RecordError must not be called on a cancelled span")
	}
}

// TestExchange_Span_CallerDeadlineClassifiesAsCancelledNotError is the
// regression test for exchangeOutcome's fix: a caller-side
// context.DeadlineExceeded — the caller's OWN deadline expiring while
// Exchange waits on the singleflight channel — must classify as cancelled,
// the same as context.Canceled, not as error. This is exactly the shape a
// SEMP retry budget produces (internal/semp/resilience/sender.go wraps the
// context that flows through AddAuth into Exchange); classifying it as
// error would blame the exchange for the caller's own timeout. Also
// exercises the "ctx_done" abandonment branch specifically (the select's
// ctx.Done() case), which the entry-guard-only cancellation test above does
// not reach.
func TestExchange_Span_CallerDeadlineClassifiesAsCancelledNotError(t *testing.T) {
	sr := withRecordingTracer(t)

	// Blocks until the test unblocks it, so the caller's short deadline
	// fires first and the select's ctx.Done() branch — not the entry
	// guard — is what returns.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-unblock
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("late-tok", 3600))
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()
	e := newTestExchanger(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := e.Exchange(ctx, validInput())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exchange() error = %v, want context.DeadlineExceeded", err)
	}

	span := findSpan(t, sr, "tokenexchange.Exchange")
	if got := attr(t, span, "outcome"); got != "cancelled" {
		t.Errorf("outcome = %v, want %q — a caller-side deadline is the caller leaving, not the exchange failing", got, "cancelled")
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("span status code = %v, want codes.Unset — SetStatus must not be called on a cancelled span", got)
	}
	if hasExceptionEvent(span) {
		t.Error("span has a recorded exception event; RecordError must not be called on a cancelled span")
	}
}

// roundTripperFunc adapts a function to http.RoundTripper, so a test can
// intercept the exact *http.Request doExchange builds without a real server.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// TestExchange_Span_DetachedCallCarriesParentForFutureRetrySpans is the
// direct proof behind the SpanContext-capture-before-detach mechanism
// (winnerSpanCtx in Exchange, re-attached via oteltrace.ContextWithSpanContext
// in runExchangeOnce): a span created from the request context doExchange
// builds — standing in for a Story 27 retry-attempt span, not yet built —
// must come back parented to the Exchange span, in the same trace, not a
// disconnected root. Breaking the capture (an invalid SpanContext) or the
// re-attachment (skipping it in runExchangeOnce) each pass every other test
// in this package silently, since nothing else observes the detached
// context's span parentage.
func TestExchange_Span_DetachedCallCarriesParentForFutureRetrySpans(t *testing.T) {
	sr := withRecordingTracer(t)

	var standInSpan sdktrace.ReadOnlySpan
	e := newTestExchanger(t, "http://placeholder.invalid")
	e.httpClient = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		// Stands in for a retry-attempt span Story 27 will create from this
		// same request context.
		_, standIn := otel.Tracer("story27-stand-in").Start(req.Context(), "semp.attempt")
		standIn.End()
		for _, s := range sr.Ended() {
			if s.Name() == "semp.attempt" {
				standInSpan = s
			}
		}
		return httptest.NewRecorder().Result(), nil
	})}

	// The response above is empty, so parseIdPResponse fails and Exchange
	// returns an error — irrelevant to this test, which only cares whether
	// the transport was reached and what it saw as its span's parent.
	_, _ = e.Exchange(context.Background(), validInput())

	if standInSpan == nil {
		t.Fatal("the stand-in span was never created — doExchange never reached the transport")
	}
	exchangeSpan := findSpan(t, sr, "tokenexchange.Exchange")
	if got, want := standInSpan.Parent().SpanID(), exchangeSpan.SpanContext().SpanID(); got != want {
		t.Errorf("stand-in span's parent SpanID = %s, want %s (the Exchange span) — the captured parent was not threaded into the detached call", got, want)
	}
	if standInSpan.Parent().TraceID() != exchangeSpan.SpanContext().TraceID() {
		t.Error("stand-in span is not in the same trace as the Exchange span")
	}
}

// TestExchange_ReturnValueUnaffectedByGlobalTracerState confirms Exchange's
// return value is unaffected by whatever the process's current global tracer
// provider happens to be — this test deliberately does not call
// withRecordingTracer, so it runs against either the real no-op default (the
// production state before cmd/server wires internal/observability/tracing,
// or whenever OBS_TRACING_ENABLED is off) or a recording provider some
// earlier test in this package already installed (both are equally safe;
// see forwardingProcessor's doc for why this package's tests cannot force
// "no provider was ever installed" once any test has installed one). Either
// way, span creation must never affect the token or error Exchange returns.
//
// Named for what it actually checks, not for AC6 (review flagged the
// original name as claiming more than the assertions cover): this cannot
// reliably assert IsRecording()==false, because by the time this test runs
// in the suite an earlier test may have already installed the shared
// recording provider (forwardingProcessor's doc explains why that, once
// installed, stays installed for the rest of the binary). AC6 itself — a
// disabled tracer provider is a true no-op — is covered at the provider
// level by TestNew_Disabled_ReturnsNilAndTouchesNothing in
// internal/observability/tracing.
func TestExchange_ReturnValueUnaffectedByGlobalTracerState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-tok", 3600))
	}))
	defer srv.Close()
	e := newTestExchanger(t, srv.URL)

	tok, err := e.Exchange(context.Background(), validInput())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if tok == nil || tok.Value != "exchanged-tok" {
		t.Fatalf("tok = %v, want a token with Value %q", tok, "exchanged-tok")
	}
}

// panicCache is a cache.TokenCache whose Get always panics — a stand-in for
// an unexpected failure inside Exchange's own body, to prove the deferred
// span-closing block records it correctly before letting it propagate.
type panicCache struct{}

func (panicCache) Get(context.Context, string) (cache.GetResult, error) {
	panic("boom")
}
func (panicCache) Put(context.Context, string, cache.CachedCredential) (cache.PutResult, error) {
	return cache.PutResult{}, nil
}
func (panicCache) Delete(context.Context, string) (cache.DeleteResult, error) {
	return cache.DeleteResult{}, nil
}
func (panicCache) Close() error { return nil }

// TestExchange_Span_PanicRecordsErrorThenRepanics is the direct proof for
// the panic-recovery fix in Exchange's own deferred span-closing block: a
// panic between tracer.Start and that defer must not close the span
// reporting outcome=success right before taking the process down (flagged
// by review — the named return err is never populated by an unwound panic,
// so without this fix the span would misreport the worst kind of failure as
// a clean success). The panic must still propagate afterward — recovering
// here is for span fidelity only, not to swallow it.
func TestExchange_Span_PanicRecordsErrorThenRepanics(t *testing.T) {
	sr := withRecordingTracer(t)

	e := newTestExchanger(t, "http://unused.invalid")
	e.cache = panicCache{}

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("Exchange() did not panic; want the panic to propagate past the span-closing defer, not be swallowed")
			}
		}()
		_, _ = e.Exchange(context.Background(), validInput())
	}()

	span := findSpan(t, sr, "tokenexchange.Exchange")
	if got := attr(t, span, "outcome"); got != "error" {
		t.Errorf("outcome = %v, want %q", got, "error")
	}
	if got := span.Status().Code; got != codes.Error {
		t.Errorf("span status code = %v, want codes.Error", got)
	}
	if !hasExceptionEvent(span) {
		t.Error("span has no recorded exception event for the panic")
	}
}

// TestExchange_Span_FollowerIsSelfDescribingViaWinnerAttributes exercises the
// path no test previously reached (flagged by review): two concurrent
// callers sharing one singleflight key, so this recorder holds two
// "tokenexchange.Exchange" spans at once — findSpan's "exactly one" scan
// cannot be used here, hence the singleflight_role attribute itself is the
// discriminator. Confirms the follower's span carries the winner's own
// trace/span IDs, so an operator can pivot from a follower's span to the
// trace that actually did the IdP work — this SDK's Span has no AddLink
// method to attach a real span Link after the span has already started
// (see Exchange's doc comment on singleflight_role for why).
func TestExchange_Span_FollowerIsSelfDescribingViaWinnerAttributes(t *testing.T) {
	sr := withRecordingTracer(t)

	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("shared-tok", 3600))
	}))
	defer srv.Close()
	e := newTestExchanger(t, srv.URL)

	input := validInput()
	const n = 2
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = e.Exchange(context.Background(), input)
		}(i)
	}

	// Brief pause to let both goroutines queue up in singleflight under the
	// same key before either's IdP call is allowed to complete — same
	// pattern as TestExchange_MultipleCallersCollapseIntoOneIdPCall in
	// exchange_test.go.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Exchange: %v", i, err)
		}
	}

	var winnerSpan, followerSpan sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() != "tokenexchange.Exchange" || !hasAttr(s, "singleflight_role") {
			continue
		}
		switch attr(t, s, "singleflight_role") {
		case "winner":
			winnerSpan = s
		case "follower":
			followerSpan = s
		}
	}
	if winnerSpan == nil {
		t.Fatal("no span with singleflight_role=\"winner\" found among the ended spans")
	}
	if followerSpan == nil {
		t.Fatal("no span with singleflight_role=\"follower\" found — the two callers never shared one singleflight call")
	}

	if got, want := attr(t, followerSpan, "winner_trace_id"), winnerSpan.SpanContext().TraceID().String(); got != want {
		t.Errorf("follower's winner_trace_id = %v, want %q (the winner's own trace)", got, want)
	}
	if got, want := attr(t, followerSpan, "winner_span_id"), winnerSpan.SpanContext().SpanID().String(); got != want {
		t.Errorf("follower's winner_span_id = %v, want %q (the winner's own span)", got, want)
	}
	if hasAttr(winnerSpan, "winner_trace_id") || hasAttr(winnerSpan, "winner_span_id") {
		t.Error("the winner's own span should not carry winner_trace_id/winner_span_id — those point elsewhere only on a follower")
	}

	// The real span Link, not just the two ID attributes above — AddLink
	// does work post-start on this SDK version (an earlier comment claimed
	// otherwise and was wrong, per review).
	links := followerSpan.Links()
	if len(links) != 1 {
		t.Fatalf("follower span has %d links, want exactly 1", len(links))
	}
	if got, want := links[0].SpanContext.TraceID(), winnerSpan.SpanContext().TraceID(); got != want {
		t.Errorf("follower span's link TraceID = %s, want %s (the winner's own trace)", got, want)
	}
	if got, want := links[0].SpanContext.SpanID(), winnerSpan.SpanContext().SpanID(); got != want {
		t.Errorf("follower span's link SpanID = %s, want %s (the winner's own span)", got, want)
	}
	if len(winnerSpan.Links()) != 0 {
		t.Error("the winner's own span should carry no links — only a follower links back to the winner")
	}
}
