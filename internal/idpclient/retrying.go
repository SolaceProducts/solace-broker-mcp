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

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/attemptspan"
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
	inner.Transport = newAttemptSpanTransport(&attemptCountTransport{inner: inner.Transport})

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
	std.Transport = &attemptSpanSeedTransport{inner: std.Transport, retryMax: retry.MaxRetries}

	return std, nil
}

// checkRetry is the token-exchange retry policy. It reads raw HTTP, not
// parsed sentinels — the token-exchange package owns sentinel classification
// separately (see internal/tokenexchange/response.go).
//
// The named return exists for the deferred endAttemptSpan call: the span for
// the attempt that just finished is tagged with the decision THIS function
// returns, never with one re-derived from the status code at the span site
// (SOL-152422). Re-deriving would let the span disagree with the retry the
// client actually performed — a 500 answered while ctx is already done reports
// no retry here, and a status-derived attribute would claim one.
//
// checkErr is returned but never consulted by endAttemptSpan: this policy has
// no equivalent of SEMP's sub-caps, so nothing here is derived from the
// error, only from ctx.Err() and the response — see retriesExhausted.
func checkRetry(ctx context.Context, resp *http.Response, err error) (retry bool, checkErr error) {
	defer func() {
		endAttemptSpan(ctx, resp, retry)
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

// attemptCountTransport is a RoundTripper wrapper that increments the counter
// on ctx (attached via WithAttemptsCounter) on every RoundTrip call. Placed
// on the inner *http.Client so retryablehttp's per-attempt c.HTTPClient.Do
// walks through it exactly once per attempt.
type attemptCountTransport struct {
	inner http.RoundTripper
}

func (a *attemptCountTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if c, ok := req.Context().Value(attemptsKey{}).(*int); ok && c != nil {
		*c++
	}
	return a.inner.RoundTrip(req)
}

// CloseIdleConnections forwards to the wrapped transport. See
// attemptspan.ForwardCloseIdleConnections for why every wrapper in the chain
// has to — shared with internal/semp/resilience rather than duplicated, per
// review.
func (a *attemptCountTransport) CloseIdleConnections() {
	attemptspan.ForwardCloseIdleConnections(a.inner)
}

// attemptSpanKey is the context key for the per-request attempt-span state.
// Unexported, and seeded only by attemptSpanSeedTransport, so no caller can
// plant a state this package would then write spans into.
type attemptSpanKey struct{}

// attemptSpanState is the per-request state the attempt spans need. One
// instance per logical request, created by attemptSpanSeedTransport and
// shared by every attempt of that request.
//
// The shared half — the attempt counter and the parked span — lives in
// attemptspan.State, which internal/semp/resilience's retryState also
// carries (SOL-152422 review: the two packages had near-identical copies).
// retryMax stays local: this policy has no equivalent of SEMP's sub-caps, so
// retriesExhausted here is a genuinely different derivation, not a shared
// one with two callers.
//
// spanState.Attempt is the ONLY counter that feeds the `attempt` span
// attribute. It is a separate counter from attemptCountTransport's — that one
// backs WithAttemptsCounter/AttemptsFromContext and, by design, also counts
// redirect hops, which are not new attempts and never get a span (see
// Transport.RoundTrip in package attemptspan). The two are expected to
// disagree whenever a redirect occurs; that is not a bug to reconcile.
//
// No lock: retryablehttp drives one request's whole loop on a single goroutine,
// and the transport wrapper and CheckRetry both run there in sequence.
type attemptSpanState struct {
	spanState attemptspan.State
	// retryMax is the client's configured retry allowance, captured at
	// construction so retriesExhausted can tell "wanted to retry" from "had
	// nothing left to retry with".
	retryMax int
}

// closeDanglingSpan ends an attempt span that was opened but never closed by
// checkRetry, so it is exported (undecided) rather than dropped. See
// attemptSpanSeedTransport for why this is a backstop and not a normal path.
// Delegates to attemptspan.State.CloseDangling rather than duplicating it.
func (s *attemptSpanState) closeDanglingSpan() {
	s.spanState.CloseDangling()
}

// attemptSpanSeedTransport is the outermost RoundTripper on the returned client. It
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
type attemptSpanSeedTransport struct {
	inner    http.RoundTripper
	retryMax int
}

func (s *attemptSpanSeedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	state := &attemptSpanState{retryMax: s.retryMax}
	// Deferred rather than placed after the call so it also covers a panic
	// unwinding through the retry loop.
	defer state.closeDanglingSpan()
	ctx := context.WithValue(req.Context(), attemptSpanKey{}, state)
	return s.inner.RoundTrip(req.WithContext(ctx))
}

// CloseIdleConnections forwards to the wrapped transport. See
// attemptspan.ForwardCloseIdleConnections.
func (s *attemptSpanSeedTransport) CloseIdleConnections() {
	attemptspan.ForwardCloseIdleConnections(s.inner)
}

// newAttemptSpanTransport wraps base in the shared attemptspan.Transport,
// seeded from this request's attemptSpanState (attemptSpanSeedTransport plants it
// under attemptSpanKey). A sibling of attemptCountTransport, composed the same
// way and for the same reason: it sits on the inner *http.Client, which
// retryablehttp calls exactly once per attempt.
func newAttemptSpanTransport(base http.RoundTripper) *attemptspan.Transport {
	return &attemptspan.Transport{
		Base:     base,
		Tracer:   tracer,
		SpanName: attemptSpanName,
		GetState: func(ctx context.Context) *attemptspan.State {
			s, ok := ctx.Value(attemptSpanKey{}).(*attemptSpanState)
			if !ok || s == nil {
				return nil
			}
			return &s.spanState
		},
	}
}

// endAttemptSpan closes the span the attempt-span transport opened, tagging
// it with the decision checkRetry is about to return. Called from
// checkRetry's own defer so it fires on every exit of that function.
//
// A nil parked span means the request never went through the attempt-span
// transport, or checkRetry ran twice for one attempt (which retryablehttp
// does when a Request carries a responseHandler — this package sets none).
// Both are no-ops, handled by attemptspan.Finish.
func endAttemptSpan(ctx context.Context, resp *http.Response, retry bool) {
	state, ok := ctx.Value(attemptSpanKey{}).(*attemptSpanState)
	if !ok || state == nil {
		return
	}
	exhausted := retriesExhausted(state.retryMax, state.spanState.Attempt, retry)
	attemptspan.Finish(ctx, &state.spanState, resp, retry, exhausted)
}

// retriesExhausted reports whether the retry allowance ran out on this attempt.
// retryablehttp breaks out of its loop when `remain := RetryMax - i` is
// non-positive, with i the 0-based loop index, so the last attempt it makes is
// attempt RetryMax+1: past that a decision to retry stands but is never acted
// on, and this attribute is what tells the two apart in a trace.
//
// A decision NOT to retry is never exhaustion — this policy has no equivalent
// of SEMP's sub-caps (a transient-retry cap, a once-only non-429/503 5xx
// replay, a once-only 401 re-auth), so every terminal "no" here is a policy
// decision (2xx/3xx/4xx) or a context that ended, not a budget running out.
// This derivation stays local to this package rather than moving into
// attemptspan: the two policies are genuinely different, not one rule with
// two callers — see that package's doc comment.
func retriesExhausted(retryMax, attempt int, retry bool) bool {
	return retry && attempt > retryMax
}
