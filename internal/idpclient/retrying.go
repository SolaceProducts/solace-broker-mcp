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
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/hashicorp/go-retryablehttp"
)

// tracer names the attempt spans this package creates (SOL-152422, Story 27).
// Resolved lazily by the OTel global delegation, so it picks up whatever
// provider main() installs and is a cheap no-op when tracing is off.
var tracer = otel.Tracer("solace-broker-mcp/idpclient")

// attemptSpanName is the span for ONE HTTP attempt of a token exchange
// (SOL-152422, Story 27). Named for the operation rather than for this package
// because that is what an operator is looking at: this package exists only to
// talk to an identity provider, and NewRetryingHTTPClient's single production
// caller is the RFC 8693 token exchange (cmd/server/main.go). The span nests
// under the `tokenexchange.Exchange` span of the singleflight-winning caller
// (Story 50, SOL-153333).
//
// Two documented limitations, both consequences of the exchange running on a
// context-detached, singleflight-owned goroutine and neither a bug to fix here:
//
//   - A DEDUPED CALLER SEES NO ATTEMPT SPANS. Only the winner's closure runs,
//     and these spans parent to the span context that closure carried, so a
//     follower's own `tokenexchange.Exchange` span has no attempt children at
//     all. A follower's span instead carries `singleflight_role="follower"` and
//     a Link to the winner, which is the pointer to the trace that does hold
//     the attempts.
//   - `correlation_id` CARRIES NO SINGLE-CALLER GUARANTEE. The detached
//     exchange is re-seeded with the WINNER's correlation ID
//     (tokenexchange.runExchangeOnce), so the attempts of one IdP call all
//     agree with each other but report the winner's request, not a follower's.
//     This is weaker than the SEMP path, where every attempt of a request
//     carries that request's own ID.
const attemptSpanName = "tokenexchange.attempt"

// RetryOptions carries the tuning knobs for NewRetryingHTTPClient's
// retry loop. Deliberately a plain struct with no smart defaults or
// validation: the token-exchange layer owns the numbers (via its own
// Default* constants and its ComputeChainDeadline formula) and passes
// them in explicitly. This package stays a mechanical composition of
// go-retryablehttp — no policy of its own.
//
// Zero values are legal but produce a client that never retries
// (MaxRetries=0). RetryWaitMin/Max are only meaningful when MaxRetries>0.
type RetryOptions struct {
	// MaxRetries is the number of retries AFTER the first attempt.
	// Total attempts = MaxRetries + 1.
	MaxRetries int

	// RetryWaitMin and RetryWaitMax bound the jittered backoff between
	// attempts. `RateLimitLinearJitterBackoff` samples uniformly in
	// [min, max]; `Retry-After` overrides the sample when present and
	// is uncapped from RetryWaitMax by design so the IdP's guidance wins.
	RetryWaitMin time.Duration
	RetryWaitMax time.Duration
}

// NewRetryingHTTPClient returns an *http.Client that retries transient IdP
// failures automatically. Composition-only: the inner attempt client is a
// stock NewHTTPClient (same TLS roots, same SSL_CERT_FILE escape hatch, same
// per-attempt Timeout). retryablehttp sits on top and drives the retry loop.
//
// Retry policy (see checkRetry):
//   - HTTP 5xx: retry.
//   - HTTP 429: retry. RateLimitLinearJitterBackoff parses the IdP's
//     Retry-After header uncapped; we defer to that signal rather than
//     second-guessing when to come back. Chain deadline still fences a
//     hostile or misconfigured Retry-After. This matches
//     retryablehttp.DefaultRetryPolicy and the general industry convention
//     of treating 429 and 5xx as siblings for backoff purposes.
//   - Connection errors (DNS, TLS handshake, body-read partials): retry.
//   - Everything else (2xx, 3xx, 4xx except 429): no retry.
//
// The returned client has no outer Timeout — callers bound the whole
// retry chain via context.WithTimeout, and the inner client's Timeout
// bounds each attempt. This split is deliberate: an outer Timeout on
// the returned client would apply to the whole chain rather than per
// attempt, defeating the point of a chain-level context deadline.
//
// Callers that want to observe how many attempts a call took can wrap the
// per-request context with WithAttemptsCounter before calling Do; see that
// function's doc for details.
//
// Every attempt also gets its own `tokenexchange.attempt` span, carrying the
// retry decision this client actually made (SOL-152422). Unconditional and
// nothing to opt into: when tracing is off the tracer is a no-op. It nests
// under whatever span is active on the request's context — see attemptSpanName
// for what that means, and does not mean, on the singleflight-detached
// exchange path.
func NewRetryingHTTPClient(retry RetryOptions, opts ...Option) (*http.Client, error) {
	inner, err := NewHTTPClient(opts...)
	if err != nil {
		return nil, err
	}

	// Wrap the inner transport so we can count attempts per request and open
	// one span per attempt. Preserves the inner client's Timeout —
	// retryablehttp calls c.HTTPClient.Do on every attempt, which applies
	// inner.Timeout.
	inner.Transport = &attemptSpanRecorder{inner: &attemptsRecorder{inner: inner.Transport}}

	rc := retryablehttp.NewClient()
	rc.HTTPClient = inner
	rc.RetryMax = retry.MaxRetries
	rc.RetryWaitMin = retry.RetryWaitMin
	rc.RetryWaitMax = retry.RetryWaitMax
	rc.Backoff = retryablehttp.RateLimitLinearJitterBackoff
	rc.CheckRetry = checkRetry
	// PassthroughErrorHandler returns the final response as-is instead of
	// dropping it into an error. Parse-layer classification runs on the
	// same shape whether or not retries were exhausted.
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
	rc.Logger = nil // silence the library's own logging; ours is at the exchange layer

	// One more wrapper, OUTSIDE retryablehttp's own RoundTripper, so it runs
	// once per logical request rather than once per attempt. That is the only
	// layer that can seed per-request state onto the context the retry loop
	// reads: retryablehttp passes the request through unchanged
	// (retryablehttp.FromRequest keeps *http.Request as-is), so a value planted
	// here is visible both to CheckRetry, which is handed req.Context(), and to
	// the inner per-attempt transport. It is also the one place that can see the
	// whole chain finish, which is where the dangling-span backstop belongs.
	std := rc.StandardClient()
	std.Transport = &attemptSpanSeeder{inner: std.Transport, retryMax: retry.MaxRetries}

	return std, nil
}

// checkRetry is the token-exchange retry policy. It reads raw HTTP, not
// parsed sentinels — the token-exchange package owns sentinel classification
// separately (see internal/tokenexchange/response.go).
//
// The named returns exist for the deferred endAttemptSpan call: the span for
// the attempt that just finished is tagged with the decision THIS function
// returns, never with one re-derived from the status code at the span site
// (SOL-152422). Re-deriving would let the span disagree with the retry the
// client actually performed — a 500 answered while ctx is already done reports
// no retry here, and a status-derived attribute would claim one.
func checkRetry(ctx context.Context, resp *http.Response, err error) (retry bool, checkErr error) {
	defer func() {
		endAttemptSpan(ctx, resp, retry, checkErr)
	}()

	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	// Connection error (DNS, TLS handshake, refused, body-read partial).
	// Retryable — the retry loop is exactly the case this class of error
	// motivates.
	if err != nil {
		return true, nil
	}
	if resp != nil && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests) {
		return true, nil
	}
	// 2xx, 3xx, 4xx (except 429): no retry. 401 will not fix itself on
	// repeat. 429 IS retried because RateLimitLinearJitterBackoff honors
	// the IdP's Retry-After header uncapped — we defer to the IdP's rate-
	// limit guidance rather than second-guessing it. The chain deadline
	// (context.WithTimeout in Exchanger.Exchange) still fences a hostile
	// Retry-After.
	return false, nil
}

// attemptsKey is the context key used by WithAttemptsCounter. Unexported so
// the counter can only be attached and read through this package's own API
// (WithAttemptsCounter, AttemptsFromContext) rather than by any caller that
// guesses the key.
type attemptsKey struct{}

// WithAttemptsCounter attaches a fresh attempts counter to ctx and returns
// the derived context along with a reader that returns the current count.
// The counter increments on every HTTP attempt (both retries and the
// original attempt, including the successful one).
//
// Nil-safe: if ctx doesn't carry a counter, the transport wrapper skips the
// increment and calls that don't need the count pay nothing. Callers that
// don't want the count simply don't call WithAttemptsCounter.
//
// Typical use:
//
//	ctx, attempts := idpclient.WithAttemptsCounter(ctx)
//	resp, err := httpClient.Do(req.WithContext(ctx))
//	log.Printf("attempts=%d", attempts())
func WithAttemptsCounter(ctx context.Context) (context.Context, func() int) {
	counter := new(int)
	ctx = context.WithValue(ctx, attemptsKey{}, counter)
	return ctx, func() int { return *counter }
}

// AttemptsFromContext returns the attempt count for a ctx that came from
// WithAttemptsCounter, for code that holds the context but not the reader the
// constructor returned. Returns 0 when ctx carries no counter, which is
// indistinguishable from "attached but nothing sent yet" — read it only where
// a request has already completed.
//
// The count is live, not a snapshot: it reflects the attempts made so far, so
// reading it mid-flight gives a number that is still moving.
func AttemptsFromContext(ctx context.Context) int {
	if c, ok := ctx.Value(attemptsKey{}).(*int); ok && c != nil {
		return *c
	}
	return 0
}

// attemptsRecorder is a RoundTripper wrapper that increments the counter
// on ctx (attached via WithAttemptsCounter) on every RoundTrip call. Placed
// on the inner *http.Client so retryablehttp's per-attempt c.HTTPClient.Do
// walks through it exactly once per attempt.
type attemptsRecorder struct {
	inner http.RoundTripper
}

func (a *attemptsRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if c, ok := req.Context().Value(attemptsKey{}).(*int); ok && c != nil {
		*c++
	}
	return a.inner.RoundTrip(req)
}

// attemptSpanKey is the context key for the per-request attempt-span state.
// Unexported, and seeded only by attemptSpanSeeder, so no caller can plant a
// state this package would then write spans into.
type attemptSpanKey struct{}

// attemptSpanState is the per-request state the attempt spans need. One
// instance per logical request, created by attemptSpanSeeder and shared by
// every attempt of that request.
//
// No lock: retryablehttp drives one request's whole loop on a single goroutine,
// and the transport wrapper and CheckRetry both run there in sequence.
type attemptSpanState struct {
	// attempt is the 1-based counter for the try now in flight. Separate from
	// the WithAttemptsCounter counter on purpose: that one is opt-in, is read
	// by the exchange layer to classify exhaustion, and counts redirect hops,
	// where this one counts retry attempts.
	attempt int
	// retryMax is the client's configured retry allowance, captured at
	// construction so retriesExhausted can tell "wanted to retry" from "had
	// nothing left to retry with".
	retryMax int
	// span is the span opened for the attempt now in flight, parked here so
	// checkRetry can tag it with its own decision and close it. nil between
	// attempts.
	span oteltrace.Span
}

// attemptSpanSeeder is the outermost RoundTripper on the returned client. It
// runs once per logical request — retryablehttp's own RoundTripper is inside
// it and owns the retry loop — and does two things:
//
//   - Seeds the per-request attempt-span state onto the context, which is what
//     makes it visible to CheckRetry and to the inner per-attempt transport.
//   - Backstops the span lifecycle after the whole chain returns. On today's
//     retryablehttp every dispatch is followed by a CheckRetry call, so nothing
//     should be left open; an unended span is dropped entirely rather than
//     exported, so being wrong here would cost the attempt its place in the
//     trace.
type attemptSpanSeeder struct {
	inner    http.RoundTripper
	retryMax int
}

func (s *attemptSpanSeeder) RoundTrip(req *http.Request) (*http.Response, error) {
	state := &attemptSpanState{retryMax: s.retryMax}
	// Deferred rather than placed after the call so it also covers a panic
	// unwinding through the retry loop.
	defer state.closeDanglingSpan()
	ctx := context.WithValue(req.Context(), attemptSpanKey{}, state)
	return s.inner.RoundTrip(req.WithContext(ctx))
}

// attemptSpanRecorder opens one span per HTTP attempt. A sibling of
// attemptsRecorder, composed the same way and for the same reason: it sits on
// the inner *http.Client, which retryablehttp calls exactly once per attempt.
//
// The span is deliberately not ended here. `retry.decision` has to come from
// checkRetry's actual decision, and retryablehttp calls CheckRetry only after
// the transport has returned, so the handle is parked on the shared state and
// closed there.
type attemptSpanRecorder struct {
	inner http.RoundTripper
}

// RoundTrip opens the attempt span and runs the try inside it.
//
// Redirect hops (req.Response != nil) are passed straight through: a redirect
// is not a new retry attempt, and a second Start for the same attempt would
// overwrite the parked handle and leak the first span. (attemptsRecorder does
// count hops; that is pre-existing behaviour of a different counter and is left
// alone here.)
//
// A request with no seeded state did not come through the client
// NewRetryingHTTPClient returns — a test wiring retryablehttp by hand, say — and
// is passed through untraced rather than given a span nothing will ever close.
func (a *attemptSpanRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response != nil {
		return a.inner.RoundTrip(req)
	}
	state, ok := req.Context().Value(attemptSpanKey{}).(*attemptSpanState)
	if !ok || state == nil {
		return a.inner.RoundTrip(req)
	}

	state.attempt++
	// The span goes into the context the inner transport sees, so the attempt
	// is the active span for the duration of the try and anything instrumented
	// below this layer later nests under it rather than under the whole chain.
	ctx, span := tracer.Start(req.Context(), attemptSpanName, oteltrace.WithSpanKind(oteltrace.SpanKindClient))
	state.span = span

	return a.inner.RoundTrip(req.WithContext(ctx))
}

// endAttemptSpan closes the span attemptSpanRecorder opened, tagging it with
// the decision checkRetry is about to return. Called from checkRetry's own
// defer so it fires on every exit of that function.
//
// A nil parked span means the request never went through
// attemptSpanRecorder, or checkRetry ran twice for one attempt (which
// retryablehttp does when a Request carries a responseHandler — this package
// sets none). Both are no-ops.
func endAttemptSpan(ctx context.Context, resp *http.Response, retry bool, checkErr error) {
	state, ok := ctx.Value(attemptSpanKey{}).(*attemptSpanState)
	if !ok || state == nil || state.span == nil {
		return
	}
	span := state.span
	// Cleared before End so a second call cannot end the same span twice, and
	// so the seeder's backstop sees nothing left to close.
	state.span = nil

	// IsRecording guard: a non-recording span (tracing off, or this trace
	// unsampled) still needs End(), but building attributes nothing will read is
	// waste on a path every token exchange goes through.
	if span.IsRecording() {
		attrs := []attribute.KeyValue{
			attribute.Int("attempt", state.attempt),
			attribute.Bool("retry.decision", retry),
		}
		// Omitted rather than written empty when absent, matching every other
		// span and audit record. Note the limitation documented on
		// attemptSpanName: on this path the ID is the singleflight WINNER's.
		if id := correlation.From(ctx); id != "" {
			attrs = append(attrs, attribute.String("correlation_id", id))
		}
		// Absent on a connection error, where there was no response at all.
		if resp != nil {
			attrs = append(attrs, attribute.Int("http.response.status_code", resp.StatusCode))
		}
		if retriesExhausted(state.retryMax, state.attempt, retry) {
			attrs = append(attrs, attribute.Bool("retry.exhausted", true))
		}
		span.SetAttributes(attrs...)
	}

	// No `outcome` attribute and no error span status, deliberately. An attempt
	// is not a call: a 503 that was retried and then succeeded is a normal step
	// of a healthy exchange, and reporting it as outcome=error would put an
	// error span under a successful `tokenexchange.Exchange` on every retried
	// exchange. The call's outcome lives on that parent span.
	span.End()
}

// retriesExhausted reports whether the retry allowance ran out on this attempt.
// retryablehttp breaks out of its loop when `remain := RetryMax - i` is
// non-positive, with i the 0-based loop index, so the last attempt it makes is
// attempt RetryMax+1: past that a decision to retry stands but is never acted
// on, and this attribute is what tells the two apart in a trace.
//
// A decision NOT to retry is never exhaustion — this policy has no equivalent
// of the SEMP transient cap, so every terminal "no" here is a policy decision
// (2xx/3xx/4xx) or a context that ended, not a budget running out. That is why
// this takes no checkRetry error, where the SEMP version does.
func retriesExhausted(retryMax, attempt int, retry bool) bool {
	return retry && attempt > retryMax
}

// closeDanglingSpan ends an attempt span that was opened but never closed by
// checkRetry, so it is exported (undecided) rather than dropped. See
// attemptSpanSeeder for why this is a backstop and not a normal path.
//
// No attributes are written: a span reaching here has no decision to report,
// and inventing one would be exactly the re-derivation this design forbids.
func (s *attemptSpanState) closeDanglingSpan() {
	if s.span == nil {
		return
	}
	span := s.span
	s.span = nil
	span.End()
}
