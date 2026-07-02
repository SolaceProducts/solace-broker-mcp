// Package auth handles broker-side authentication for SEMP clients
// (Hop 2: MCP Server → Broker). One Authenticator implementation
// exists per auth mode (basic, bearer, and — added later — oauth);
// the SEMP transport layer holds an Authenticator behind the interface
// and never branches on auth mode.
//
// Distinct from internal/auth, which handles Hop 1 (MCP Client → MCP
// Server) and is unrelated to SEMP traffic.
package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
)

// Authenticator attaches the right Authorization header to an outbound
// SEMP request for a single broker and declares how to recover when
// authentication fails. One implementation exists per auth mode (basic,
// bearer, and — added later — oauth). The SEMP transport layer holds an
// Authenticator behind this interface and never branches on auth mode;
// adding a new mode is one new implementation plus one new case in
// NewAuthenticator.
//
// Implementations must be safe for concurrent use: many goroutines may
// call AddAuth and HandleAuthFailure simultaneously with their own ctx.
// This is achieved structurally — implementations hold only fields set
// at construction and never written again, so concurrent reads of struct
// state are safe by Go's memory model. Per-request data flows through
// ctx and req; no per-request state lives on the Authenticator.
type Authenticator interface {
	AddAuth(ctx context.Context, req *http.Request) error
	HandleAuthFailure(ctx context.Context, resp *http.Response) (retry bool)
}

// CookieJarClearer is accepted by BasicAuthenticator to clear stale
// session cookies on auth failure. Defined here (consumer side) rather
// than in the resilience package (provider side) to avoid an import
// cycle. resilience.SafeCookieJar satisfies this implicitly.
type CookieJarClearer interface {
	Clear() error
}

// NewAuthenticator returns the Authenticator implementation matching
// cfg.Mode. It is the single place that knows which concrete type goes
// with which mode — callers receive the interface and never branch on
// auth mode themselves.
//
// jar is passed to BasicAuthenticator for 401 cookie-jar clearing;
// nil is safe (HandleAuthFailure returns retry=false when jar is nil).
//
// Returns an error if cfg.Mode is not a recognized auth mode. Required
// per-mode fields (e.g. username/password for basic) are not validated
// here; config.validate() enforces those at startup.
func NewAuthenticator(cfg config.AuthConfig, jar CookieJarClearer) (Authenticator, error) {
	switch cfg.Mode {
	case config.AuthModeBasic:
		return NewBasicAuthenticator(cfg.Username, cfg.Password, jar), nil
	case config.AuthModeBearer:
		return NewBearerAuthenticator(cfg.Token), nil
	default:
		return nil, fmt.Errorf("auth: unsupported auth mode %q", cfg.Mode)
	}
}
