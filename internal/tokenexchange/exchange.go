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
	"log/slog"
	"runtime/debug"

	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
	"github.com/sony/gobreaker/v2"
)

// Exchange performs an RFC 8693 token exchange against the configured IdP.
// It checks the cache first; on a miss, concurrent calls with the same
// (subjectToken, brokerAlias) pair are collapsed into a single IdP
// round-trip via singleflight, and the result is cached for future calls.
func (e *Exchanger) Exchange(ctx context.Context, input ExchangeInput) (*Token, error) {
	// A caller who is already done gets its own error, never a token.
	//
	// TokenCache.Get takes a context but does not promise to honour it, and
	// the in-memory implementation shipped today ignores it outright, so the
	// cache hit below could hand a live token to a cancelled caller. Guarding
	// here rather than pushing the check into the cache is deliberate on both
	// counts: the interface reserves its error return for backend failures,
	// and Exchange treats a Get error as a warning and falls through to a full
	// IdP exchange, so a cache-level rejection would make a dead caller do
	// *more* work, not less.
	//
	// This is a contract fix, not a flake fix. The intermittent failure in
	// TestExchange_ContextDeadlineExceededReturnsDeadlineError was the test's
	// own premise — see the note there.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	key := computeDeduplicationKey(DeduplicationKeyInput{
		SubjectToken: input.SubjectToken,
		BrokerAlias:  input.BrokerAlias,
	})

	// Cross-time dedup: serve from cache if a fresh token exists.
	gr, getErr := e.cache.Get(ctx, key)
	if getErr != nil {
		slog.WarnContext(ctx, "token cache get failed", "broker", input.BrokerAlias, "error", getErr)
	} else {
		slog.Log(ctx, gr.Status.Level(), "token cache get", "broker", input.BrokerAlias, "status", gr.Status)
		if gr.Status == cache.GetHit {
			return &Token{
				Value:     gr.Entry.Value,
				ExpiresAt: gr.Entry.ExpiresAt,
			}, nil
		}
	}

	// In-flight dedup: collapse concurrent misses into one IdP call. The
	// cache Put runs INSIDE the singleflight func so it executes exactly
	// once per burst (only the winner runs the func; all waiters share its
	// return value). Doing Put outside would run it N times for N concurrent
	// callers, producing N redundant writes and log lines.
	//
	// DoChan (not Do) so each caller waits in its own select: a caller whose
	// context is cancelled bails out immediately instead of blocking until
	// the shared IdP round-trip finishes. DoChan also runs the func on a
	// singleflight-owned goroutine, so the shared call — and its cache Put —
	// completes independently of any caller's cancellation, warming the cache
	// for the next caller even when every current caller has bailed.
	//
	// The success log stays on the result branch, not inside the func,
	// because it's per-caller observability: a caller that cancelled
	// mid-exchange must not see "exchange succeeded" attributed to its
	// request. The Put log is per-exchange and stays inside with the Put.
	start := e.nowFunc()
	ch := e.group.DoChan(key, func() (_ interface{}, err error) {
		// singleflight runs this on its OWN goroutine, which none of the
		// request-path recover() nets cover — and DoChan re-raises an escaping
		// panic via `go panic(e)`, which is unrecoverable and takes down the
		// whole multi-session process. Recover here at the point of spawn, the
		// same net safego.Go applies to errgroup workers (SOL-151514): convert
		// a panic into an error so it flows back as res.Err. Log the panic's Go
		// TYPE and stack, never its value, per the secure-logging rules.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered panic in token exchange",
					slog.String("event", "panic_recovered"),
					slog.String("broker", input.BrokerAlias),
					slog.String("panic_type", fmt.Sprintf("%T", r)),
					slog.String("stack", string(debug.Stack())))
				err = fmt.Errorf("token exchange panicked (%T)", r)
			}
		}()
		return e.runProtectedExchange(key, input)
	})

	select {
	case <-ctx.Done():
		// The caller abandoned the exchange. The shared IdP call keeps running
		// on singleflight's goroutine and will still warm the cache, so log
		// here: a cancellation storm (callers bailing while detached calls keep
		// hitting the IdP) is otherwise invisible, and this line correlates an
		// abandoned caller with the later "token cache put" it leaves behind.
		// broker alias and ctx.Err() text are safe to log; no token material.
		slog.DebugContext(ctx, "token exchange abandoned by caller",
			slog.String("broker", input.BrokerAlias),
			slog.Duration("waited", e.nowFunc().Sub(start)),
			slog.String("cause", ctx.Err().Error()))
		return nil, ctx.Err()
	case res := <-ch:
		// Both cases can be ready at once — the shared exchange may complete
		// while this caller is descheduled — and the Go spec then resolves the
		// select "via a uniform pseudo-random selection". Re-check so
		// cancellation wins deterministically
		// instead of by coin flip. Rare in practice: it needs the caller to be
		// off-CPU for a whole IdP round-trip. One load on the success path is
		// cheaper than an outcome that depends on the scheduler.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		elapsed := e.nowFunc().Sub(start)
		if res.Err != nil {
			// Map the breaker's open-state rejection (returned by Execute
			// without the func running) onto our own sentinel before
			// enrichment, so upper layers see ErrExchangeCircuitOpen rather
			// than a raw gobreaker error.
			err := mapCircuitOpen(res.Err)
			var exchErr *ExchangeError
			if errors.As(err, &exchErr) {
				enriched := *exchErr
				enriched.BrokerAlias = input.BrokerAlias
				enriched.Audience = input.Audience
				enriched.TokenEndpoint = e.tokenURL
				enriched.Elapsed = elapsed
				return nil, &enriched
			}
			return nil, err
		}

		tok := res.Val.(*Token)

		slog.DebugContext(ctx, "token exchange succeeded",
			slog.String("broker", input.BrokerAlias),
			slog.Duration("exchange_elapsed", elapsed))

		return tok, nil
	}
}

// runProtectedExchange runs one logical exchange under the circuit breaker.
// The breaker wraps the whole IdP-facing section — retries, classification,
// and the cache Put — so it records exactly ONE outcome per logical exchange
// regardless of how many HTTP attempts the retry loop made, and observes the
// CLASSIFIED error (so IsSuccessful/IsExcluded see the right FailureClass).
//
// It runs inside the singleflight func, so only the burst winner reaches the
// breaker — concurrent identical callers share the winner's result and do not
// each record an outcome. When the breaker is disabled (nil), the same section
// runs directly; retries and classification are unaffected.
//
// gateCheck runs FIRST, before the breaker — a gated call records no
// breaker outcome and makes no IdP round-trip, regardless of breaker state.
func (e *Exchanger) runProtectedExchange(key string, input ExchangeInput) (*Token, error) {
	if e.gateCheck() {
		return nil, &ExchangeError{
			Sentinel: ErrExchangeRateLimited,
			Message:  "token exchange rate limited: honoring IdP Retry-After, not attempting",
		}
	}
	if e.breaker == nil {
		return e.runExchangeOnce(key, input)
	}
	return e.breaker.Execute(func() (*Token, error) {
		return e.runExchangeOnce(key, input)
	})
}

// runExchangeOnce performs the detached, retry-bounded IdP call, classifies the
// outcome, and caches a success. This is the unit the breaker measures.
func (e *Exchanger) runExchangeOnce(key string, input ExchangeInput) (*Token, error) {
	// Detach from the caller's context so one caller's cancellation does not
	// abort the shared IdP call for all singleflight waiters. The explicit
	// timeout bounds the whole retry chain independently — httpClient.Timeout
	// bounds each attempt, chainDeadline bounds attempts + backoffs together.
	exchCtx, cancel := context.WithTimeout(context.Background(), e.chainDeadline)
	defer cancel()

	// Attach an attempts counter so the transport wrapper inside
	// NewRetryingHTTPClient increments it on every attempt. We read it back
	// after doExchange returns to distinguish a one-shot transport failure
	// from a retries-exhausted one. Nil-safe: if httpClient isn't the retrying
	// variant (tests with a plain *http.Client), the counter stays 0 and no
	// rewrap fires.
	exchCtx, attempts := idpclient.WithAttemptsCounter(exchCtx)

	tok, err := e.doExchange(exchCtx, input)
	if err != nil {
		classified := e.classifyRetryOutcome(exchCtx, err, attempts(), input.BrokerAlias)
		e.raiseGateOnExhaustedRateLimit(classified, input.BrokerAlias)
		return nil, classified
	}

	if n := attempts(); n > 1 {
		slog.DebugContext(exchCtx, "token exchange retried",
			slog.String("broker", input.BrokerAlias),
			slog.Int("attempts", n))
	}

	pr, putErr := e.cache.Put(exchCtx, key, cache.CachedCredential{
		Value:     tok.Value,
		ExpiresAt: tok.ExpiresAt,
	})
	if putErr != nil {
		slog.WarnContext(exchCtx, "token cache put failed", "broker", input.BrokerAlias, "error", putErr)
	} else {
		slog.Log(exchCtx, pr.Status.Level(), "token cache put", "broker", input.BrokerAlias, "status", pr.Status)
	}

	return tok, nil
}

// mapCircuitOpen converts gobreaker's open-state sentinels into our own
// ErrExchangeCircuitOpen envelope. Both mean "the breaker refused the call
// without running it"; upper layers should never see a raw gobreaker error.
// Any other error passes through unchanged.
func mapCircuitOpen(err error) error {
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return &ExchangeError{
			Sentinel: ErrExchangeCircuitOpen,
			Message:  "token exchange circuit open: identity provider recently unavailable, not attempting",
		}
	}
	return err
}

// Close releases resources held by the cache backend (e.g. Otter's
// background eviction goroutines). Must be called after all in-flight
// Exchange calls have completed.
func (e *Exchanger) Close() error {
	return e.cache.Close()
}

// Invalidate evicts a cached token for the given (subjectToken, brokerAlias)
// pair. Called by OAuthAuthenticator.HandleAuthFailure when the broker
// rejects the token with a 401 — the next Exchange call will miss the
// cache and fetch a fresh token from the IdP.
func (e *Exchanger) Invalidate(ctx context.Context, input DeduplicationKeyInput) {
	key := computeDeduplicationKey(input)
	_, err := e.cache.Delete(ctx, key)
	if err != nil {
		slog.WarnContext(ctx, "token cache delete failed", "broker", input.BrokerAlias, "error", err)
	} else {
		slog.DebugContext(ctx, "token cache invalidated", "broker", input.BrokerAlias)
	}
}

// classifyRetryOutcome decides whether an error returned by doExchange
// should be rewrapped as ErrExchangeRetriesExhausted. The rewrap fires
// only when at least one attempt was actually dispatched (attempts >= 1)
// AND the underlying failure is one the retry loop would have iterated
// over: an ErrExchangeTransport sentinel (5xx or connection error the
// loop finally gave up on), or a chain-deadline expiry that interrupted
// a retry mid-flight.
//
// The attempts >= 1 guard preserves the invariant that "exhaustion means
// we tried and failed" — a pre-dispatch chain-deadline expiry
// (attempts == 0) propagates raw context.DeadlineExceeded, which is the
// honest signal that nothing was tried. In practice the pre-dispatch
// case is nearly impossible at the derived chain deadline
// (see ComputeChainDeadline), but the invariant is what keeps the
// sentinel meaningful.
//
// context.Canceled is NOT rewrapped: the retry loop runs on a detached
// context (context.Background + WithTimeout), so the only cancellation
// reaching it is the chain deadline firing as DeadlineExceeded. A raw
// Canceled would indicate an internal bug, and the raw error is a
// better diagnostic than a misleading exhaustion wrap.
func (e *Exchanger) classifyRetryOutcome(ctx context.Context, err error, attempts int, brokerAlias string) error {
	if attempts < 1 {
		return err
	}

	var exchErr *ExchangeError
	switch {
	case errors.As(err, &exchErr) && errors.Is(err, ErrExchangeTransport):
		// Fall through to rewrap below.
	case errors.Is(err, context.DeadlineExceeded):
		// Fall through — chain deadline interrupted a retry mid-flight.
	default:
		return err
	}

	slog.DebugContext(ctx, "token exchange retries exhausted",
		slog.String("broker", brokerAlias),
		slog.Int("attempts", attempts),
		slog.String("underlying", err.Error()))

	// Copy HTTPStatus and FailureClass forward: the new sentinel severs
	// errors.Is back to ErrExchangeTransport, so FailureClass is the only
	// signal of the underlying cause the breaker still has.
	//
	// On the transport path (exchErr != nil) the surviving class is copied
	// as-is — an exhausted run of 5xx stays Upstream5xx and counts, an
	// exhausted network run stays Network, etc.
	//
	// On the deadline path exchErr is nil (the error was a bare
	// context.DeadlineExceeded): the whole retry chain hung until the budget
	// fired without ever producing a classifiable response. That is an IdP
	// availability failure — "no usable response received" — so it is
	// classified Network, the same class as a connection-level failure. Left
	// as FailureClassNone it would collapse into the breaker's "not a
	// transport outcome" bucket and be recorded as a SUCCESS, diluting the
	// failure rate during exactly the sustained hang the breaker exists to
	// catch. HTTPStatus stays zero — there genuinely was no response.
	rewrapped := &ExchangeError{
		Sentinel: ErrExchangeRetriesExhausted,
		Message:  fmt.Sprintf("token exchange retries exhausted after %d attempts: %s", attempts, err.Error()),
	}
	if exchErr != nil {
		rewrapped.HTTPStatus = exchErr.HTTPStatus
		rewrapped.FailureClass = exchErr.FailureClass
		// Survives the rewrap for the same reason FailureClass does — the
		// gate is raised only from the last attempt's outcome.
		rewrapped.RetryAfterResult = exchErr.RetryAfterResult
	} else {
		rewrapped.FailureClass = FailureClassNetwork
	}
	return rewrapped
}

func (e *Exchanger) doExchange(ctx context.Context, input ExchangeInput) (*Token, error) {
	req, err := e.buildIdPRequest(ctx, input)
	if err != nil {
		return nil, &ExchangeError{
			Sentinel: ErrExchangeRequestBuild,
			Message:  fmt.Sprintf("token exchange request build failure: %v", err),
		}
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ExchangeError{
			Sentinel:     ErrExchangeTransport,
			Message:      fmt.Sprintf("token exchange transport failure: IdP request failed: %v", err),
			FailureClass: classifyTransportError(err),
		}
	}

	return e.parseIdPResponse(resp, e.nowFunc())
}
