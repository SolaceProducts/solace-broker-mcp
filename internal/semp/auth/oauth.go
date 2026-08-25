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

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	internalauth "github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// tokenExchanger is the capability OAuthAuthenticator needs from the token
// exchange layer. Unexported because only *tokenexchange.Exchanger
// implements it in production; the interface exists for test fakes.
type tokenExchanger interface {
	Exchange(ctx context.Context, input tokenexchange.ExchangeInput) (*tokenexchange.Token, error)
	Invalidate(ctx context.Context, input tokenexchange.DeduplicationKeyInput)
}

// OAuthAuthenticator obtains a broker-bound access token by exchanging
// the agent's inbound token (from Hop 1) via RFC 8693 token exchange.
// Fields are set at construction and never written again, so AddAuth
// and HandleAuthFailure are safe to call concurrently from any number
// of goroutines. Per-request state (the subject token) flows through ctx.
type OAuthAuthenticator struct {
	exchanger   tokenExchanger
	audience    string
	brokerAlias string
}

// NewOAuthAuthenticator returns an OAuthAuthenticator that will exchange
// the agent's inbound token for a broker-scoped token on every SEMP
// request. The non-nil exchanger invariant is owned by config validation
// (see internal/config validateBroker + validateBrokerOAuthConfig): a
// broker with auth.mode: oauth is rejected at startup unless the
// preconditions that cause main.go to build a real exchanger hold.
//
// Failure mode if the invariant is violated: a nil-pointer dereference
// inside AddAuth on the first SEMP request against the affected broker.
// If that ever surfaces in production, the bug is upstream — either
// main.go swallowed newTokenExchanger's error, or Hop2OAuthActive
// returned true without an exchanger being constructed. Do not add a
// nil-check here; fix the upstream invariant.
func NewOAuthAuthenticator(exchanger tokenExchanger, audience string, brokerAlias string) *OAuthAuthenticator {
	return &OAuthAuthenticator{
		exchanger:   exchanger,
		audience:    audience,
		brokerAlias: brokerAlias,
	}
}

// AddAuth exchanges the agent's inbound token (carried on ctx by the
// Hop 1 middleware) for a broker-scoped token and sets it as the
// Authorization: Bearer header on req.
func (a *OAuthAuthenticator) AddAuth(ctx context.Context, req *http.Request) error {
	// Opens the trace. Everything between this and the finish line below
	// belongs to one broker call, whichever route the token took.
	slog.DebugContext(ctx, "broker token needed",
		slog.String("broker", a.brokerAlias))

	subjectToken, ok := internalauth.RawSubjectTokenFromContext(ctx)
	if !ok {
		err := &tokenexchange.ExchangeError{
			Sentinel:    tokenexchange.ErrExchangeMissingSubject,
			Message:     "oauth auth: no subject token on context — Hop 1 middleware may not have run",
			BrokerAlias: a.brokerAlias,
		}
		a.logUnavailable(ctx, err)
		return err
	}

	tok, err := a.exchanger.Exchange(ctx, tokenexchange.ExchangeInput{
		SubjectToken: subjectToken,
		BrokerAlias:  a.brokerAlias,
		Audience:     a.audience,
	})
	if err != nil {
		a.logUnavailable(ctx, err)
		return fmt.Errorf("oauth auth: token exchange failed: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok.Value)

	// Closes the trace opened above, on the success path.
	slog.DebugContext(ctx, "broker token attached to request",
		slog.String("broker", a.brokerAlias))

	return nil
}

// logUnavailable closes the trace on every failure path, so a start line always
// has a partner and a dangling one means something abnormal.
//
// It carries the sentinel and nothing else: the tool layer already reports the
// same failure at ERROR with the full diagnostics, so repeating them here would
// log one failure twice.
//
// A cancelled caller does not get an ExchangeError — Exchange returns the bare
// context error from its entry guard, its cache-hit re-check, and the select
// that notices the caller left — so that case is read separately. It is
// ordinary traffic, not an edge case: any client that disconnects mid-request
// arrives here.
func (a *OAuthAuthenticator) logUnavailable(ctx context.Context, err error) {
	reason := "unknown"
	var exchErr *tokenexchange.ExchangeError
	switch {
	case errors.As(err, &exchErr) && exchErr.Sentinel != nil:
		reason = exchErr.Sentinel.Error()
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		reason = err.Error()
	}
	slog.DebugContext(ctx, "broker token unavailable",
		slog.String("broker", a.brokerAlias),
		slog.String("reason", reason))
}

// HandleAuthFailure evicts the cached token and signals a retry that must
// re-authenticate — the token is refreshable, so AddAuth fetches a fresh one.
// With no subject token there is nothing to exchange, so it declines to retry.
func (a *OAuthAuthenticator) HandleAuthFailure(ctx context.Context, _ http.Header) AuthFailureResult {
	subjectToken, ok := internalauth.RawSubjectTokenFromContext(ctx)
	if !ok {
		return AuthFailureResult{}
	}
	// Building the same ExchangeInput shape AddAuth does above, then
	// converting via DedupKeyInput, keeps this structurally unable to drift
	// from Exchange's own key for the same logical call — a field added to
	// one without updating the other fails to compile (SOL-152981).
	a.exchanger.Invalidate(ctx, tokenexchange.ExchangeInput{
		SubjectToken: subjectToken,
		BrokerAlias:  a.brokerAlias,
		Audience:     a.audience,
	}.DedupKeyInput())
	return AuthFailureResult{Retry: true, ReAuth: true}
}
