// Package auth handles broker-side authentication for SEMP clients
// (Hop 2: MCP Server → Broker). One Authenticator implementation
// exists per auth mode (basic, bearer, oauth); the SEMP transport
// layer holds an Authenticator behind the interface and never branches
// on auth mode.
//
// Distinct from internal/auth, which handles Hop 1 (MCP Client → MCP
// Server). The OAuth authenticator reads the Hop 1 subject token from
// internal/auth's context key — the Hop 1 token is the RFC 8693 subject
// for the Hop 2 exchange, so this coupling is inherent to the design.
package auth

import (
	"context"
	"net/http"
)

// Authenticator attaches the right Authorization header to an outbound
// SEMP request for a single broker and declares how to recover when
// authentication fails. One implementation exists per auth mode (basic,
// bearer, oauth). The SEMP transport layer holds an Authenticator behind
// this interface and never branches on auth mode; adding a new mode is
// one new concrete type plus one new case in semp.newAuthenticator.
//
// Implementations must be safe for concurrent use: many goroutines may
// call AddAuth and HandleAuthFailure simultaneously with their own ctx.
// Current implementations (basic, bearer) achieve this structurally —
// they hold only fields set at construction and never written again.
// Implementations that mutate state after construction (e.g. OAuth
// token refresh) must provide their own synchronization.
type Authenticator interface {
	AddAuth(ctx context.Context, req *http.Request) error

	// HandleAuthFailure is called by the resilience layer when the broker
	// rejects a request with 401 Unauthorized. It performs any recovery the
	// auth mode allows (evicting a cached token, clearing session cookies)
	// and returns how the resilience layer should proceed.
	HandleAuthFailure(ctx context.Context, respHeader http.Header) AuthFailureResult
}

// AuthFailureResult tells the resilience layer how to respond to a 401.
// Retry reports whether the request should be retried at all. ReAuth reports
// whether that retry must first re-invoke AddAuth to attach refreshed
// credentials. ReAuth is only consulted when Retry is true.
type AuthFailureResult struct {
	Retry  bool
	ReAuth bool
}

// CookieJarClearer is accepted by BasicAuthenticator to clear stale
// session cookies on auth failure. Defined here (consumer side) rather
// than in the resilience package (provider side) to avoid an import
// cycle. resilience.SafeCookieJar satisfies this implicitly.
type CookieJarClearer interface {
	Clear() error
}
