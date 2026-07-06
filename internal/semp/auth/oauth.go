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
}

// OAuthAuthenticator obtains a broker-bound access token by exchanging
// the agent's inbound token (from Hop 1) via RFC 8693 token exchange.
// Fields are set at construction and never written again, so AddAuth
// and HandleAuthFailure are safe to call concurrently from any number
// of goroutines. Per-request state (the subject token) flows through ctx.
type OAuthAuthenticator struct {
	exchanger   tokenExchanger
	audience    string
	scopes      []string
	brokerAlias string
}

// NewOAuthAuthenticator returns an OAuthAuthenticator that will exchange
// the agent's inbound token for a broker-scoped token on every SEMP
// request. Panics if exchanger is nil — a nil exchanger is a wiring
// bug, not a runtime condition.
func NewOAuthAuthenticator(exchanger tokenExchanger, audience string, scopes []string, brokerAlias string) *OAuthAuthenticator {
	if exchanger == nil {
		panic("NewOAuthAuthenticator: exchanger must be non-nil")
	}
	return &OAuthAuthenticator{
		exchanger:   exchanger,
		audience:    audience,
		scopes:      scopes,
		brokerAlias: brokerAlias,
	}
}

// AddAuth exchanges the agent's inbound token (carried on ctx by the
// Hop 1 middleware) for a broker-scoped token and sets it as the
// Authorization: Bearer header on req.
func (a *OAuthAuthenticator) AddAuth(ctx context.Context, req *http.Request) error {
	subjectToken, ok := internalauth.RawSubjectTokenFromContext(ctx)
	if !ok {
		return fmt.Errorf("oauth auth: no subject token on context")
	}

	tok, err := a.exchanger.Exchange(ctx, tokenexchange.ExchangeInput{
		SubjectToken: subjectToken,
		BrokerAlias:  a.brokerAlias,
		Audience:     a.audience,
		Scopes:       a.scopes,
	})
	if err != nil {
		return fmt.Errorf("oauth auth: token exchange failed: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok.Value)
	return nil
}

// HandleAuthFailure returns false unconditionally — the Exchanger is
// stateless (no cache to evict), so retrying after a broker 401 with
// the same exchanged token is pointless.
func (a *OAuthAuthenticator) HandleAuthFailure(_ context.Context, _ http.Header) bool {
	return false
}
