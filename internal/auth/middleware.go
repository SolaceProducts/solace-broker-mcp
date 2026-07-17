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
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/authz"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
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
func NewAuthMiddleware(cfg *config.ServerConfig, httpClient *http.Client, next http.Handler) (http.Handler, error) {
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

	verifier, err := NewTokenVerifier(cfg, httpClient)
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

	// InjectRawSubjectToken is wrapped INSIDE the SDK middleware so it runs
	// only after sdkauth.RequireBearerToken has validated the bearer token
	// (signature, issuer, audience, expiry). A value reaching ctx under
	// rawSubjectTokenKey{} therefore carries the SDK-validated invariant,
	// which the future OAuth Authenticator relies on as the subject_token
	// for RFC 8693 exchange. See SOL-150797 and
	// docs/superpowers/plans/oauth-token-exchange/2026-06-21-SOL-150797-T3-raw-subject-token.md.
	return middleware(InjectRawSubjectToken(next)), nil
}

// NewTokenVerifier creates a TokenVerifier based on cfg.MCPClientAuth.Mode.
//   - AuthModeStatic → constant-time compare against cfg.MCPClientAuth.DevToken
//   - AuthModeOAuth  → OIDC/JWT verification with automatic key rotation
//
// httpClient follows the same nil-default contract as NewAuthMiddleware.
//
// cfg has already been validated via config.validate(); other modes are
// programming errors.
func NewTokenVerifier(cfg *config.ServerConfig, httpClient *http.Client) (sdkauth.TokenVerifier, error) {
	switch cfg.MCPClientAuth.Mode {
	case config.AuthModeStatic:
		return createStaticTokenVerifier(cfg.MCPClientAuth.DevToken), nil
	case config.AuthModeOAuth:
		return createOIDCTokenVerifier(cfg, httpClient)
	default:
		return nil, fmt.Errorf("internal: NewTokenVerifier called with unsupported mcp_client_auth.mode %q (validator should have rejected this)", cfg.MCPClientAuth.Mode)
	}
}

// createStaticTokenVerifier returns a TokenVerifier that validates against a static token.
// This is only for development/testing purposes.
// Uses constant-time comparison to prevent timing attacks.
func createStaticTokenVerifier(expectedToken string) sdkauth.TokenVerifier {
	return func(ctx context.Context, token string, req *http.Request) (*sdkauth.TokenInfo, error) {
		// Use constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
			return nil, sdkauth.ErrInvalidToken
		}
		return &sdkauth.TokenInfo{
			Scopes:     []string{},
			UserID:     "dev-user",
			Expiration: time.Now().Add(24 * time.Hour), // Dev tokens don't expire for 24 hours
		}, nil
	}
}

// createOIDCTokenVerifier creates a TokenVerifier that validates JWTs using
// OIDC, fetching public keys from the issuer's JWKS endpoint with automatic
// rotation. httpClient may be nil (see NewAuthMiddleware's contract).
// go-oidc's RemoteKeySet strips cancellation from the construction context
// via WithoutCancel, so http.Client.Timeout is the only bound that reaches
// lazy JWKS refresh — the discovery deadline below only caps the initial
// discovery call.
func createOIDCTokenVerifier(cfg *config.ServerConfig, httpClient *http.Client) (sdkauth.TokenVerifier, error) {
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
		// Verify JWT signature and standard claims (iss, aud, exp)
		idToken, err := verifier.Verify(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}

		// SINGLE claim decode.
		//
		// The claims payload is parsed exactly once, into
		// map[string]json.RawMessage. All downstream extraction — the
		// spec-defined identity claims AND the admin-configured groups
		// claim — reads from this one view.
		//
		// This deliberately replaces the previous two-pass approach
		// (typed struct + generic map). encoding/json v1 matches struct
		// fields case-insensitively with last-match-wins semantics and
		// folds ſ (U+017F) / K (U+212A) to s / k, while map keys match
		// exactly. Two decodes of the same bytes could therefore
		// disagree about which claims exist — a parser differential
		// between the audit-log identity surface (SOL-149606) and the
		// authz groups path. One parse means one interpretation; the
		// exact, case-sensitive key lookup below matches RFC 7519's
		// case-sensitive claim names and the remediation the MCP go-sdk
		// itself applied in GHSA-wvj2-96wp-fq3f.
		//
		// Residual v1 limitation: exact-duplicate top-level keys still
		// resolve last-wins silently. Both identity and groups now see
		// the same winner, so no differential remains; encoding/json/v2
		// (jsontext duplicate-name rejection) can close this fully once
		// adopted.
		var raw map[string]json.RawMessage
		if err := idToken.Claims(&raw); err != nil {
			// FAIL CLOSED. Verification is not complete until the claims
			// this server depends on are extracted and well-formed
			// (RFC 8725 §3: reject the entire JWT when validation fails).
			// The previous warn-and-continue produced authenticated
			// requests with an empty UserID and blank audit identity.
			return nil, fmt.Errorf("%w: decoding claims: %v", sdkauth.ErrInvalidToken, err)
		}

		return buildTokenInfo(cfg, Claims{raw: raw}, idToken.Expiry)
	}, nil
}

// buildTokenInfo assembles the TokenInfo for a verified token from a single
// decode of its claim bytes. Split from the verifier closure so the
// extraction semantics (exact key match, fail-closed on malformed or missing
// security-relevant claims) are unit-testable without an IdP.
//
// Claim requirement profile (deliberate, spec-grounded — do not "fix" by
// making everything required or everything lenient):
//
//   - Enforced upstream by the go-oidc verifier before this runs:
//     signature, iss, aud, exp. Three of RFC 9068's required claims are
//     therefore already mandatory in effect.
//   - Hard-required here: sub, non-blank. Required by RFC 9068 §2.2 for
//     OAuth access tokens, and the audit surface (SOL-149606) is
//     meaningless without it. (RFC 7519 alone leaves sub optional; the
//     access-token profile is the binding one for this server.)
//   - Tolerated-absent, fatal-if-malformed: scope, client_id, jti.
//     RFC 9068 requires client_id and jti, but widely deployed IdPs do
//     not emit them on access tokens (Keycloak and Auth0's default
//     profile use azp; Azure AD uses appid/azp), so hard-requiring them
//     would reject stock deployments of real customer IdPs. A present
//     claim with the wrong JSON type is still rejected: absence is a
//     sparse token, malformation means the payload can no longer be
//     safely interpreted. Scope, when present, must be a space-delimited
//     string per RFC 6749 §3.3 / RFC 9068.
//   - Groups (authz enabled): absent claim → no groups key → authz
//     denies by default.
//
// Any fail-closed violation returns sdkauth.ErrInvalidToken.
func buildTokenInfo(cfg *config.ServerConfig, claims Claims, expiry time.Time) (*sdkauth.TokenInfo, error) {
	sub, err := claims.String("sub")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sub) == "" {
		// An authenticated principal with no visible subject is never a
		// valid state: every audit row it generated would be anonymous.
		// TrimSpace is used ONLY as a rejection predicate here — the
		// stored UserID below is the exact claim bytes. Normalizing the
		// value would conflate subjects the IdP considers distinct
		// (RFC 7519 claims compare byte-for-byte), which is an identity
		// mapping the IdP owns, not this server.
		return nil, fmt.Errorf("%w: token has no subject", sdkauth.ErrInvalidToken)
	}

	// Scope is a space-separated string per OAuth 2.0 (RFC 6749 §3.3).
	// IdPs that emit an array here (e.g. some Azure AD / Keycloak
	// configurations) previously caused a partially-populated struct plus
	// a swallowed warning; now they are rejected explicitly so the
	// misconfiguration surfaces at the door.
	scopeStr, err := claims.String("scope")
	if err != nil {
		return nil, err
	}
	scopes := []string{}
	if scopeStr != "" {
		scopes = strings.Split(scopeStr, " ")
	}

	// iss, client_id, and jti feed the per-invocation audit-log identity
	// surface (SOL-149606). They are stashed in TokenInfo.Extra under the
	// exact keys "iss", "client_id", "jti"; internal/tools/identity.go
	// reads them by those keys. Drift-detection tests pin the allowlist.
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
		name := *cfg.MCPClientAuth.ToolAuthorization.GroupsClaimName
		// The admin-configured groups claim is hydrated lazily from THE
		// SAME decoded view as the identity claims above, so the identity
		// the audit log records and the groups the authz layer evaluates
		// are by construction drawn from one interpretation of the token.
		// ResolveGroups documents flat top-level lookup only (matching
		// the broker's accessLevelGroupsClaimName stance), so exactly one
		// claim is hydrated. Future consumers with different type needs
		// get their own accessor method on Claims — never a per-consumer
		// conversion loop here at the boundary.
		val, exists, err := claims.Value(name)
		if err != nil {
			return nil, err
		}
		if exists {
			groups, ok := resolveGroupsValue(val)
			if ok {
				extra[authz.TokenInfoExtraKeyGroups] = groups
			}
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
