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

	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache"
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

	// In-flight dedup: collapse concurrent misses into one IdP call.
	start := e.nowFunc()
	v, err, _ := e.group.Do(key, func() (interface{}, error) {
		// Detach from the caller's context so one caller's cancellation
		// does not abort the shared IdP call for all singleflight waiters.
		// The explicit timeout bounds the detached call independently.
		exchCtx, cancel := context.WithTimeout(context.Background(), e.exchangeTimeout)
		defer cancel()
		return e.doExchange(exchCtx, input)
	})
	elapsed := e.nowFunc().Sub(start)

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
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

	// Store the exchanged token for future requests.
	pr, putErr := e.cache.Put(ctx, key, cache.CachedCredential{
		Value:     tok.Value,
		ExpiresAt: tok.ExpiresAt,
	})
	if putErr != nil {
		slog.WarnContext(ctx, "token cache put failed", "broker", input.BrokerAlias, "error", putErr)
	} else {
		slog.Log(ctx, pr.Status.Level(), "token cache put", "broker", input.BrokerAlias, "status", pr.Status)
	}

	return tok, nil
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

func (e *Exchanger) doExchange(ctx context.Context, input ExchangeInput) (*Token, error) {
	req, err := e.buildIdPRequest(ctx, input)
	if err != nil {
		return nil, &ExchangeError{
			Sentinel: ErrExchangeTransport,
			Message:  fmt.Sprintf("token exchange transport failure: %v", err),
		}
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ExchangeError{
			Sentinel: ErrExchangeTransport,
			Message:  fmt.Sprintf("token exchange transport failure: IdP request failed: %v", err),
		}
	}

	return e.parseIdPResponse(resp, e.nowFunc())
}
