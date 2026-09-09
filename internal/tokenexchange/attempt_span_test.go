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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/SolaceProducts/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// attemptSpanName duplicates idpclient's unexported constant. Deliberate: the
// on-the-wire span name is the contract an operator queries and this file
// asserts it as a literal, so a rename in idpclient shows up here as a failing
// test rather than being silently carried along by a shared symbol.
const attemptSpanName = "tokenexchange.attempt"

// newFastRetryingTestExchanger is newRetryingTestExchanger with millisecond
// backoff, so a test that needs two real HTTP attempts does not wait out the
// production 1-2s jittered wait.
func newFastRetryingTestExchanger(t *testing.T, serverURL string) *Exchanger {
	t.Helper()
	client, err := idpclient.NewRetryingHTTPClient(
		idpclient.RetryOptions{
			MaxRetries:   DefaultMaxRetries,
			RetryWaitMin: 1 * time.Millisecond,
			RetryWaitMax: 5 * time.Millisecond,
		},
		idpclient.WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}
	p := validParams(t)
	p.TokenURL = serverURL
	p.HTTPClient = client
	e, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

// endedAttemptSpans returns every ended `tokenexchange.attempt` span the
// recorder holds, in the order they ended. Not trace-scoped, on purpose: these
// tests assert WHICH trace each attempt span landed in, so scoping the query by
// trace up front would assume the answer.
func endedAttemptSpans(sr *tracetest.SpanRecorder) []sdktrace.ReadOnlySpan {
	var out []sdktrace.ReadOnlySpan
	for _, s := range sr.Ended() {
		if s.Name() == attemptSpanName {
			out = append(out, s)
		}
	}
	return out
}

// exchangeSpansByRole indexes the ended `tokenexchange.Exchange` spans by their
// singleflight_role attribute, failing the test if a role is not represented
// exactly once.
func exchangeSpansByRole(t *testing.T, sr *tracetest.SpanRecorder) map[string]sdktrace.ReadOnlySpan {
	t.Helper()
	byRole := map[string][]sdktrace.ReadOnlySpan{}
	for _, s := range sr.Ended() {
		if s.Name() != "tokenexchange.Exchange" {
			continue
		}
		for _, kv := range s.Attributes() {
			if kv.Key == "singleflight_role" {
				role := kv.Value.AsString()
				byRole[role] = append(byRole[role], s)
			}
		}
	}
	out := map[string]sdktrace.ReadOnlySpan{}
	for _, role := range []string{"winner", "follower"} {
		if len(byRole[role]) != 1 {
			t.Fatalf("found %d tokenexchange.Exchange spans with singleflight_role=%q, want exactly 1",
				len(byRole[role]), role)
		}
		out[role] = byRole[role][0]
	}
	return out
}

func attemptSpanAttrs(s sdktrace.ReadOnlySpan) map[string]any {
	out := map[string]any{}
	for _, kv := range s.Attributes() {
		out[string(kv.Key)] = kv.Value.AsInterface()
	}
	return out
}

// TestExchange_AttemptSpansNestUnderTheExchangeSpan proves the Story 50
// plumbing is actually used: the span context captured before the singleflight
// detach really is the parent of the attempt spans the retrying client creates,
// so a retried IdP call reads as attempts under one exchange rather than as
// orphan spans in a trace of their own.
func TestExchange_AttemptSpansNestUnderTheExchangeSpan(t *testing.T) {
	sr := withRecordingTracer(t)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-tok", 3600))
	}))
	defer srv.Close()

	e := newFastRetryingTestExchanger(t, srv.URL)

	ctx, caller := otel.Tracer("test").Start(context.Background(), "semp.request")
	if _, err := e.Exchange(ctx, validInput()); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	caller.End()

	exchange := findSpan(t, sr, "tokenexchange.Exchange")
	spans := endedAttemptSpans(sr)
	if len(spans) != 2 {
		t.Fatalf("got %d %s spans, want 2 (a 500 then a 200)", len(spans), attemptSpanName)
	}
	for i, s := range spans {
		if s.SpanContext().TraceID() != exchange.SpanContext().TraceID() {
			t.Errorf("attempt %d: trace ID = %v, want the exchange's %v — an attempt in its own trace is "+
				"invisible to anyone reading the request's trace",
				i+1, s.SpanContext().TraceID(), exchange.SpanContext().TraceID())
		}
		if s.Parent().SpanID() != exchange.SpanContext().SpanID() {
			t.Errorf("attempt %d: parent span ID = %v, want the tokenexchange.Exchange span %v",
				i+1, s.Parent().SpanID(), exchange.SpanContext().SpanID())
		}
		if got, want := attemptSpanAttrs(s)["attempt"], int64(i+1); got != want {
			t.Errorf("attempt %d: attempt attribute = %v, want %v", i+1, got, want)
		}
	}
	if got := attemptSpanAttrs(spans[0])["retry.decision"]; got != true {
		t.Errorf("attempt 1: retry.decision = %v, want true (the 500 was retried)", got)
	}
	if got := attemptSpanAttrs(spans[1])["retry.decision"]; got != false {
		t.Errorf("attempt 2: retry.decision = %v, want false (the 200 ended the chain)", got)
	}
}

// TestExchange_DedupedCallerSeesNoAttemptSpans is the documented-limitation
// test the story calls for.
//
// The exchange runs on a context-detached, singleflight-owned goroutine, and
// the attempt spans parent to the span context the WINNING caller captured
// before that detach. Only the winner's closure runs, so a deduped caller's own
// `tokenexchange.Exchange` span has no attempt children at all — the attempts
// are in the winner's trace. This is accepted behaviour, not a bug: a follower
// is pointed at the work that served it by `singleflight_role="follower"`, its
// `winner_trace_id`/`winner_span_id` attributes, and a span Link.
//
// Asserted rather than merely commented because the failure mode is silent. An
// implementation that started the attempt spans from the follower's own context
// — or from no parent at all — would still produce attempt spans and still look
// healthy in a single-caller test.
func TestExchange_DedupedCallerSeesNoAttemptSpans(t *testing.T) {
	sr := withRecordingTracer(t)

	const winnerCorrID = "0198f1a2-3b4c-7d5e-8f90-a1b2c3d4e5f6"
	const followerCorrID = "0198f1a2-3b4c-7d5e-8f90-ffffffffffff"

	var hits atomic.Int32
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			// Hold the winner's first attempt open so the second caller is
			// guaranteed to arrive while the shared call is in flight, and
			// therefore to be deduped rather than to run its own exchange.
			once.Do(func() { close(entered) })
			<-gate
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("exchanged-tok", 3600))
	}))
	defer srv.Close()

	e := newFastRetryingTestExchanger(t, srv.URL)
	input := validInput()

	// Two callers, each under its own root span in its own trace, so "which
	// trace did the attempts land in" is an answerable question.
	winnerCtx, winnerSpan := otel.Tracer("test").Start(
		correlation.With(context.Background(), winnerCorrID), "semp.request")
	followerCtx, followerSpan := otel.Tracer("test").Start(
		correlation.With(context.Background(), followerCorrID), "semp.request")

	errs := make(chan error, 2)
	go func() {
		_, err := e.Exchange(winnerCtx, input)
		errs <- err
	}()
	<-entered // the winner's IdP call is in flight, so it has already won
	go func() {
		_, err := e.Exchange(followerCtx, input)
		errs <- err
	}()
	// Give the follower time to reach DoChan and join the in-flight call. The
	// gate below is what actually lets the exchange proceed, so this only has
	// to be long enough for a goroutine to get to its select.
	time.Sleep(50 * time.Millisecond)
	close(gate)

	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Exchange (caller %d): %v", i+1, err)
		}
	}
	winnerSpan.End()
	followerSpan.End()

	roles := exchangeSpansByRole(t, sr)
	winner, follower := roles["winner"], roles["follower"]

	spans := endedAttemptSpans(sr)
	if len(spans) == 0 {
		t.Fatal("no attempt spans at all; the test proved nothing")
	}
	for i, s := range spans {
		if s.Parent().SpanID() == follower.SpanContext().SpanID() {
			t.Errorf("attempt %d is parented to the FOLLOWER's exchange span; a deduped caller must see no "+
				"attempt spans — it never ran the IdP call", i+1)
		}
		if s.SpanContext().TraceID() == follower.SpanContext().TraceID() {
			t.Errorf("attempt %d landed in the follower's trace %v; the attempts belong to the winner's trace %v",
				i+1, s.SpanContext().TraceID(), winner.SpanContext().TraceID())
		}
		if s.Parent().SpanID() != winner.SpanContext().SpanID() {
			t.Errorf("attempt %d: parent span ID = %v, want the WINNER's exchange span %v",
				i+1, s.Parent().SpanID(), winner.SpanContext().SpanID())
		}
		// The other documented limitation: correlation_id on this path is the
		// winner's, so it carries no single-caller guarantee. A follower's own
		// request ID never appears on an attempt span.
		got := attemptSpanAttrs(s)["correlation_id"]
		if got != winnerCorrID {
			t.Errorf("attempt %d: correlation_id = %v, want the winner's %q", i+1, got, winnerCorrID)
		}
		if got == followerCorrID {
			t.Errorf("attempt %d: correlation_id is the FOLLOWER's; the detached exchange is seeded with the "+
				"winner's ID only", i+1)
		}
	}
}
