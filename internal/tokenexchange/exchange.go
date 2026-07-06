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
)

// Exchange performs an RFC 8693 token exchange against the configured IdP.
// Concurrent calls with the same (subjectToken, brokerAlias) pair are
// collapsed into a single IdP round-trip via singleflight.
func (e *Exchanger) Exchange(ctx context.Context, input ExchangeInput) (*Token, error) {
	key := computeDeduplicationKey(DeduplicationKeyInput{
		SubjectToken: input.SubjectToken,
		BrokerAlias:  input.BrokerAlias,
	})

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

	slog.Debug("token exchange succeeded",
		slog.String("broker", input.BrokerAlias),
		slog.Duration("exchange_elapsed", elapsed))
	return v.(*Token), nil
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
