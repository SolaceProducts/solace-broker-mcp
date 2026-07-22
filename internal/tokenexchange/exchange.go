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

	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
	"github.com/sony/gobreaker/v2"
)

// Exchange performs an RFC 8693 token exchange against the configured IdP.
// It checks the cache first; on a miss, concurrent calls with the same
// (subjectToken, brokerAlias) pair are collapsed into a single IdP
// round-trip via singleflight, and the result is cached for future calls.
func (e *Exchanger) Exchange(ctx context.Context, input ExchangeInput) (*Token, error) {
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
	// The success log stays OUTSIDE the func because it's per-caller
	// observability: a caller that cancelled mid-exchange should not
	// see "exchange succeeded" attributed to their request. The Put log
	// is per-exchange and stays inside with the Put.
	start := e.nowFunc()
	v, err, _ := e.group.Do(key, func() (interface{}, error) {
		return e.runProtectedExchange(key, input)
	})
	elapsed := e.nowFunc().Sub(start)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		// Map the breaker's open-state rejection (returned by Execute
		// without the func running) onto our own sentinel before
		// enrichment, so upper layers see ErrExchangeCircuitOpen rather
		// than a raw gobreaker error.
		err = mapCircuitOpen(err)
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

	tok := v.(*Token)

	slog.DebugContext(ctx, "token exchange succeeded",
		slog.String("broker", input.BrokerAlias),
		slog.Duration("exchange_elapsed", elapsed))

	return tok, nil
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
func (e *Exchanger) runProtectedExchange(key string, input ExchangeInput) (*Token, error) {
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
		return nil, e.classifyRetryOutcome(exchCtx, err, attempts(), input.BrokerAlias)
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
	// errors.Is back to ErrExchangeTransport, so these are the only signals
	// of the underlying cause the breaker still has. On the deadline path
	// exchErr is nil, so both stay zero — the honest "no response received"
	// signal.
	rewrapped := &ExchangeError{
		Sentinel: ErrExchangeRetriesExhausted,
		Message:  fmt.Sprintf("token exchange retries exhausted after %d attempts: %s", attempts, err.Error()),
	}
	if exchErr != nil {
		rewrapped.HTTPStatus = exchErr.HTTPStatus
		rewrapped.FailureClass = exchErr.FailureClass
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
