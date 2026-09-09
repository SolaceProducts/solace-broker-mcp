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
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/schema"
	"github.com/coreos/go-oidc/v3/oidc"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// NewAuthMiddleware wires the auth backend selected by mcp_client_auth.mode.
// httpClient is the IdP-bound HTTP client used by the OAuth verifier path
// (OIDC discovery + lazy JWKS refresh). Pass nil in production — the OAuth
// path will build the default via idpclient.NewHTTPClient. Tests pass a
// non-nil client (built via idpclient.NewHTTPClient(idpclient.WithTimeout))
// for SOL-150219 regression coverage. Ignored on the disabled and static
// paths; nil is fine.
//
// hook receives auth_success/auth_failure audit emission (SOL-152097). nil
// (every pre-SOL-152097 call site, updated explicitly rather than left to a
// variadic default — a variadic slot here would silently accept and drop a
// second hook with no error, which is exactly the wrong failure mode for an
// audit observer) means no emission. Production wires
// audit.NewAuthHook(audit.Enabled(cfg.Observability)) in cmd/server/main.go.
// See AuthAuditHook's doc for why this is an interface injected from outside
// the package rather than a direct call to the audit package.
func NewAuthMiddleware(cfg *config.ServerConfig, httpClient *http.Client, next http.Handler, hook AuthAuditHook) (http.Handler, error) {
	// Auth backend selection mirrors mcp_client_auth.mode. Insecure-mode signaling
	// lives in cmd/server/main.go via banner.LogStartupAuthMode — DO NOT add WARN
	// logs here. See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
	switch cfg.MCPClientAuth.Mode {
	case config.AuthModeDisabled:
		return next, nil
	case config.AuthModeStatic, config.AuthModeOAuth:
		// fall through to the verifier construction below
	default:
		return nil, fmt.Errorf("internal: NewAuthMiddleware called with unsupported mcp_client_auth.mode %q (validator should have rejected this)", cfg.MCPClientAuth.Mode)
	}

	verifier, err := NewTokenVerifier(cfg, httpClient, hook)
	if err != nil {
		return nil, fmt.Errorf("failed to create token verifier: %w", err)
	}

	// Construct the metadata URL at the server root.
	// Config validation ensures ResourceURL is well-formed if set.
	var metadataURL string
	if cfg.MCPClientAuth.ResourceURL != "" {
		parsedURL, _ := url.Parse(cfg.MCPClientAuth.ResourceURL)
		metadataURL = fmt.Sprintf("%s://%s/.well-known/oauth-protected-resource", parsedURL.Scheme, parsedURL.Host)
	}

	middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
	})

	// Hop 1: RequireBearerToken validates the token (signature, issuer, audience, expiry).
	//
	// Nothing per-caller is installed on this chain's context: it does not
	// reach a tool handler per request. Both the caller Principal
	// (PrincipalMiddleware) and the Hop 2 subject token
	// (RequestExtraMiddleware) are stamped per JSON-RPC request by receiving
	// middleware instead.
	//
	// auditMissingBearerToken wraps the whole chain: RequireBearerToken's own
	// bearer-extraction check (unexported in the SDK) never calls our
	// TokenVerifier at all when the Authorization header carries no bearer
	// token, so that rejection would otherwise produce no auth_failure record
	// whatsoever — the single most common shape of unauthenticated access for
	// a story about detecting it (SOL-152097).
	return auditMissingBearerToken(hook, middleware(next)), nil
}

// NewTokenVerifier creates a TokenVerifier based on cfg.MCPClientAuth.Mode.
//   - AuthModeStatic → constant-time compare against cfg.MCPClientAuth.DevToken
//   - AuthModeOAuth  → OIDC/JWT verification with automatic key rotation
//
// httpClient follows the same nil-default contract as NewAuthMiddleware.
//
// cfg has already been validated via config.validate(); other modes are
// programming errors.
//
// hook follows NewAuthMiddleware's contract: nil means no emission.
func NewTokenVerifier(cfg *config.ServerConfig, httpClient *http.Client, hook AuthAuditHook) (sdkauth.TokenVerifier, error) {
	switch cfg.MCPClientAuth.Mode {
	case config.AuthModeStatic:
		return createStaticTokenVerifier(cfg.MCPClientAuth.DevToken, hook), nil
	case config.AuthModeOAuth:
		return createOIDCTokenVerifier(cfg, httpClient, hook)
	default:
		return nil, fmt.Errorf("internal: NewTokenVerifier called with unsupported mcp_client_auth.mode %q (validator should have rejected this)", cfg.MCPClientAuth.Mode)
	}
}

// auditMissingBearerToken wraps next (the chain RequireBearerToken produced)
// with a peek at the Authorization header, so a request that never reaches
// our TokenVerifier at all — no bearer token presented — still produces an
// auth_failure record (SOL-152097). Uses parseBearerToken, the same helper
// RequestExtraMiddleware uses, which already mirrors go-sdk's own
// bearer-extraction check (raw_subject_token.go) rather than a second,
// independent copy of that parsing — the one thing this must never disagree
// with. This wrapper only ever adds a record alongside the SDK's own 401,
// never changes whether a request is accepted: it always calls next
// regardless of what it observes.
//
// reason distinguishes two shapes parseBearerToken folds into one bool: no
// Authorization header at all classifies as "missing" (nothing was
// presented to reject); a header present but not parseable as a bearer token
// — wrong scheme, malformed, an empty value after "Bearer" — classifies as
// "invalid_token", because there is a token-shaped problem to name rather
// than an absence. Conflating the two would inflate an "unauthenticated
// probe" alert on `reason: missing` with callers who did present something.
func auditMissingBearerToken(hook AuthAuditHook, next http.Handler) http.Handler {
	if hook == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if _, ok := parseBearerToken(authHeader); !ok {
			reason := ClassifyAuthFailure(nil)
			if authHeader != "" {
				reason = schema.AuthFailureReasonInvalidToken
			}
			reportAuthFailure(r.Context(), hook, reason, "", "")
		}
		next.ServeHTTP(w, r)
	})
}

// createStaticTokenVerifier returns a TokenVerifier that validates against a static token.
// This is only for development/testing purposes.
// Uses constant-time comparison to prevent timing attacks.
func createStaticTokenVerifier(expectedToken string, hook AuthAuditHook) sdkauth.TokenVerifier {
	return func(ctx context.Context, token string, req *http.Request) (*sdkauth.TokenInfo, error) {
		// Use constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
			// A static-token mismatch carries no claims to attribute — there
			// is nothing behind the token to have parsed.
			reportAuthFailure(ctx, hook, ClassifyAuthFailure(errVerificationFailed), "", "")
			return nil, errVerificationFailed
		}
		info := &sdkauth.TokenInfo{
			Scopes:     []string{},
			UserID:     "dev-user",
			Expiration: time.Now().Add(24 * time.Hour), // Dev tokens don't expire for 24 hours
		}
		reportAuthSuccess(ctx, hook, info)
		return info, nil
	}
}

// createOIDCTokenVerifier creates a TokenVerifier that validates JWTs using
// OIDC, fetching public keys from the issuer's JWKS endpoint with automatic
// rotation. httpClient may be nil (see NewAuthMiddleware's contract).
// go-oidc's RemoteKeySet strips cancellation from the construction context
// via WithoutCancel, so http.Client.Timeout is the only bound that reaches
// lazy JWKS refresh — the discovery deadline below only caps the initial
// discovery call.
func createOIDCTokenVerifier(cfg *config.ServerConfig, httpClient *http.Client, hook AuthAuditHook) (sdkauth.TokenVerifier, error) {
	if httpClient == nil {
		c, err := idpclient.NewHTTPClient()
		if err != nil {
			return nil, err
		}
		httpClient = c
	}
	clientCtx := oidc.ClientContext(context.Background(), httpClient)
	ctx, cancel := context.WithTimeout(clientCtx, 30*time.Second)
	defer cancel()

	oidcProvider, err := oidc.NewProvider(ctx, cfg.MCPClientAuth.Issuer)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to identity provider at %s (is it reachable?): %w", cfg.MCPClientAuth.Issuer, err)
	}

	// Create verifier that validates issuer, audience, and signature
	verifier := oidcProvider.Verifier(&oidc.Config{
		ClientID: cfg.MCPClientAuth.Audience,
	})

	return func(ctx context.Context, token string, req *http.Request) (*sdkauth.TokenInfo, error) {
		idToken, err := verifier.Verify(ctx, token)
		if err != nil {
			slog.Warn("token verification failed",
				slog.String("error", err.Error()))
			// Verify failed before any claims were readable, so there is
			// nothing to attribute the record to.
			reportAuthFailure(ctx, hook, ClassifyAuthFailure(err), "", "")
			return nil, errVerificationFailed
		}

		// Single decode into map[string]json.RawMessage — identity and
		// groups claims read from one parse, eliminating the parser
		// differential that existed with the prior struct+map two-pass.
		var raw map[string]json.RawMessage
		if err := idToken.Claims(&raw); err != nil {
			slog.Warn("token claims undecodable",
				slog.String("error", err.Error()))
			// The signature verified, but the payload never decoded, so no
			// claim — including sub — is readable.
			reportAuthFailure(ctx, hook, ClassifyAuthFailure(err), "", "")
			return nil, errMalformedClaims
		}

		info, err := buildTokenInfo(cfg, Claims{raw: raw}, idToken.Expiry)
		if err != nil {
			slog.Warn("token rejected",
				slog.String("error", err.Error()))
			// The token parsed and its signature verified; buildTokenInfo
			// rejected it over a specific claim (e.g. missing sub, or a
			// malformed scope/groups value). raw is still the decoded claim
			// set regardless of which claim failed, so sub/client_id are
			// attributed whenever they themselves decoded cleanly.
			sub, clientID := bestEffortIdentity(raw)
			reportAuthFailure(ctx, hook, ClassifyAuthFailure(err), sub, clientID)
			return nil, sanitizeTokenError(err)
		}
		reportAuthSuccess(ctx, hook, info)
		return info, nil
	}, nil
}

// buildTokenInfo assembles the TokenInfo for a verified token. Split from
// the verifier closure so claim extraction is unit-testable without an IdP.
//
// Claim policy: sub is hard-required (RFC 9068 §2.2); scope, client_id,
// jti are tolerated-absent but fatal-if-malformed (real IdPs often omit
// them); groups absent → authz denies by default.
func buildTokenInfo(cfg *config.ServerConfig, claims Claims, expiry time.Time) (*sdkauth.TokenInfo, error) {
	sub, err := claims.String("sub")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sub) == "" {
		// TrimSpace for rejection only — UserID stores the exact bytes.
		return nil, errNoSubject
	}

	scopeStr, err := claims.String("scope")
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	if scopeStr != "" {
		scopes = strings.Split(scopeStr, " ")
	}

	iss, err := claims.String("iss")
	if err != nil {
		return nil, err
	}
	clientID, err := claims.String("client_id")
	if err != nil {
		return nil, err
	}
	jti, err := claims.String("jti")
	if err != nil {
		return nil, err
	}

	extra := map[string]any{
		"iss":       iss,
		"client_id": clientID,
		"jti":       jti,
	}

	if config.ToolAuthorizationEnabled(cfg) {
		// GroupsClaimName is guaranteed non-nil here: config.validate
		// defaults it to "groups" when the tool_authorization block is present.
		name := *cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName
		val, exists, err := claims.Value(name)
		if err != nil {
			return nil, err
		}
		if exists {
			groups, ok := resolveGroupsValue(val)
			if ok {
				extra[authz.TokenInfoExtraKeyGroups] = groups
			} else {
				slog.Debug("groups claim present but not resolvable",
					slog.String("claim", name))
			}
		} else {
			slog.Debug("groups claim not found in token",
				slog.String("claim", name))
		}
	}

	return &sdkauth.TokenInfo{
		UserID:     sub,
		Scopes:     scopes,
		Expiration: expiry,
		Extra:      extra,
	}, nil
}

// NewProtectedResourceMetadataHandler creates an HTTP handler that serves
// OAuth 2.0 Protected Resource Metadata (RFC 9728) for the MCP server.
// This endpoint enables MCP clients to discover the authorization server
// and initiate browser-based OAuth flows (Authorization Code + PKCE).
// Only served under mcp_client_auth.mode == "oauth"; returns nil otherwise.
func NewProtectedResourceMetadataHandler(cfg *config.ServerConfig) http.Handler {
	if cfg.MCPClientAuth.Mode != config.AuthModeOAuth {
		return nil
	}

	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               cfg.MCPClientAuth.ResourceURL,
		AuthorizationServers:   []string{cfg.MCPClientAuth.Issuer},
		ScopesSupported:        []string{"openid"},
		BearerMethodsSupported: []string{"header"},
	}

	return sdkauth.ProtectedResourceMetadataHandler(metadata)
}
