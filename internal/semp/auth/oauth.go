package auth

import (
	"context"
	"fmt"
	"net/http"

	internalauth "github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/tokenexchange"
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
	subjectToken, ok := internalauth.RawSubjectTokenFromContext(ctx)
	if !ok {
		return &tokenexchange.ExchangeError{
			Sentinel:    tokenexchange.ErrExchangeMissingSubject,
			Message:     "oauth auth: no subject token on context — Hop 1 middleware may not have run",
			BrokerAlias: a.brokerAlias,
		}
	}

	tok, err := a.exchanger.Exchange(ctx, tokenexchange.ExchangeInput{
		SubjectToken: subjectToken,
		BrokerAlias:  a.brokerAlias,
		Audience:     a.audience,
	})
	if err != nil {
		return fmt.Errorf("oauth auth: token exchange failed: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok.Value)
	return nil
}

// HandleAuthFailure evicts the cached token and signals a retry that must
// re-authenticate — the token is refreshable, so AddAuth fetches a fresh one.
// With no subject token there is nothing to exchange, so it declines to retry.
func (a *OAuthAuthenticator) HandleAuthFailure(ctx context.Context, _ http.Header) AuthFailureResult {
	subjectToken, ok := internalauth.RawSubjectTokenFromContext(ctx)
	if !ok {
		return AuthFailureResult{}
	}
	a.exchanger.Invalidate(ctx, tokenexchange.DeduplicationKeyInput{
		SubjectToken: subjectToken,
		BrokerAlias:  a.brokerAlias,
	})
	return AuthFailureResult{Retry: true, ReAuth: true}
}
