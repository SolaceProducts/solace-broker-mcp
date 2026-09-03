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
	//
	// The caller Principal is NOT installed here: this chain's context does
	// not reach a tool handler per request (see PrincipalMiddleware).
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
			return nil, errVerificationFailed
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
		idToken, err := verifier.Verify(ctx, token)
		if err != nil {
			slog.Warn("token verification failed",
				slog.String("error", err.Error()))
			return nil, errVerificationFailed
		}

		// Single decode into map[string]json.RawMessage — identity and
		// groups claims read from one parse, eliminating the parser
		// differential that existed with the prior struct+map two-pass.
		var raw map[string]json.RawMessage
		if err := idToken.Claims(&raw); err != nil {
			slog.Warn("token claims undecodable",
				slog.String("error", err.Error()))
			return nil, errMalformedClaims
		}

		info, err := buildTokenInfo(cfg, Claims{raw: raw}, idToken.Expiry)
		if err != nil {
			slog.Warn("token rejected",
				slog.String("error", err.Error()))
			return nil, sanitizeTokenError(err)
		}
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
