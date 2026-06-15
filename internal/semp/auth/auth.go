// Package auth handles broker-side authentication for SEMP clients
// (Hop 2: MCP Server → Broker). It is the single dispatch point for
// every auth mechanism we support, so adding a new mode means adding
// one case here — call sites do not change.
//
// Distinct from internal/auth, which handles Hop 1 (MCP Client → MCP
// Server) and is unrelated to SEMP traffic.
//
// The signature accepts ctx and returns error so future modes that
// need network calls (e.g. OAuth token exchange against an IdP) fit
// without breaking call sites. The static modes (basic, bearer) ignore
// ctx and always return nil.
package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// Authenticator attaches the right Authorization header to an outbound
// SEMP request for a single broker. One implementation exists per auth
// mode (basic, bearer, and — added later — oauth). The SEMP transport
// layer holds an Authenticator behind this interface and never branches
// on auth mode; adding a new mode is one new implementation plus one
// new case in NewAuthenticator.
//
// Implementations must be safe for concurrent use: many goroutines may
// call AddAuth simultaneously with their own ctx and req. This is
// achieved structurally — implementations hold only fields set at
// construction and never written again, so concurrent reads of struct
// state are safe by Go's memory model. Per-request data flows through
// ctx and req; no per-request state lives on the Authenticator.
type Authenticator interface {
	AddAuth(ctx context.Context, req *http.Request) error
}

// NewAuthenticator returns the Authenticator implementation matching
// cfg.Mode. It is the single place that knows which concrete type goes
// with which mode — callers receive the interface and never branch on
// auth mode themselves.
//
// Returns an error if cfg.Mode is not a recognized auth mode. Required
// per-mode fields (e.g. username/password for basic) are not validated
// here; config.validate() enforces those at startup.
func NewAuthenticator(cfg config.AuthConfig) (Authenticator, error) {
	switch cfg.Mode {
	case config.AuthModeBasic:
		return NewBasicAuthenticator(cfg.Username, cfg.Password), nil
	case config.AuthModeBearer:
		return NewBearerAuthenticator(cfg.Token), nil
	default:
		return nil, fmt.Errorf("auth: unsupported auth mode %q", cfg.Mode)
	}
}

// AddAuth attaches the configured Authorization header to req based on
// the broker's auth mode. Modes:
//
//   - basic:  Authorization: Basic base64(user:pass)
//   - bearer: Authorization: Bearer <static-token>
//
// Future:
//
//   - oauth:  exchange the upstream user token for a broker-scoped token
//     via an IdP, then set Authorization: Bearer <exchanged-token>.
//     Will use ctx for cancellation and return any IdP error.
//
// Returns nil on success. Returns an error only when an auth mode's
// dispatch genuinely fails (e.g. future OAuth exchange) — basic and
// bearer always succeed.
func AddAuth(ctx context.Context, req *http.Request, cfg config.AuthConfig) error {
	switch cfg.Mode {
	case config.AuthModeBasic:
		req.SetBasicAuth(cfg.Username, cfg.Password)
	case config.AuthModeBearer:
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	default:
		// Defensive: config.validate() guarantees Mode is one of the
		// supported values, so this branch is unreachable in practice.
		// Returning a typed error rather than silently no-opping makes
		// any future configuration drift fail loudly.
		return fmt.Errorf("auth: unsupported auth mode %q", cfg.Mode)
	}
	return nil
}
