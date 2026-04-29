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
