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
	"time"
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
		return e.doExchange(ctx, input)
	})
	elapsed := e.nowFunc().Sub(start)

	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		e.logExchangeError(err, input, elapsed)
		return nil, err
	}

	return v.(*Token), nil
}

func (e *Exchanger) doExchange(ctx context.Context, input ExchangeInput) (*Token, error) {
	req, err := e.buildIdPRequest(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExchangeTransport, err)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: IdP request failed: %v", ErrExchangeTransport, err)
	}

	return e.parseIdPResponse(resp, e.nowFunc())
}

func (e *Exchanger) logExchangeError(err error, input ExchangeInput, elapsed time.Duration) {
	attrs := []slog.Attr{
		slog.String("broker_alias", input.BrokerAlias),
		slog.String("audience", input.Audience),
		slog.String("token_endpoint", e.tokenURL),
		slog.Duration("elapsed", elapsed),
		slog.String("error", err.Error()),
	}

	switch {
	case errors.Is(err, ErrExchangeRejected):
		slog.LogAttrs(context.Background(), slog.LevelWarn, "token exchange: rejected by IdP", attrs...)
	case errors.Is(err, ErrExchangeTransport):
		slog.LogAttrs(context.Background(), slog.LevelError, "token exchange: transport failure", attrs...)
	case errors.Is(err, ErrInvalidResponse):
		slog.LogAttrs(context.Background(), slog.LevelError, "token exchange: invalid IdP response", attrs...)
	default:
		slog.LogAttrs(context.Background(), slog.LevelError, "token exchange: unexpected error", attrs...)
	}
}
