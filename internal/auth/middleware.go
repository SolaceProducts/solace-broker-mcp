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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
	"github.com/coreos/go-oidc/v3/oidc"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// oidcHTTPClientTimeout bounds the HTTP client injected into go-oidc for both
// startup discovery and lazy JWKS refresh. Package-private to allow tests to
// override with a shorter value; production reads defaults.DefaultOIDCHTTPTimeout.
var oidcHTTPClientTimeout = defaults.DefaultOIDCHTTPTimeout

func NewAuthMiddleware(cfg *config.ServerConfig, next http.Handler) (http.Handler, error) {
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

	verifier, err := NewTokenVerifier(cfg)
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
// cfg has already been validated via config.validate(); other modes are
// programming errors.
func NewTokenVerifier(cfg *config.ServerConfig) (sdkauth.TokenVerifier, error) {
	switch cfg.MCPClientAuth.Mode {
	case config.AuthModeStatic:
		return createStaticTokenVerifier(cfg.MCPClientAuth.DevToken), nil
	case config.AuthModeOAuth:
		return createOIDCTokenVerifier(cfg)
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

// createOIDCTokenVerifier creates a TokenVerifier that validates JWTs using OIDC.
// It fetches public keys from the issuer's JWKS endpoint and handles automatic key rotation.
func createOIDCTokenVerifier(cfg *config.ServerConfig) (sdkauth.TokenVerifier, error) {
	// oidcHTTPClient always returns a client bounded by oidcHTTPClientTimeout,
	// so attaching it here covers both startup discovery and the lazy JWKS
	// refresh path (RemoteKeySet, triggered by an unknown kid). The discovery
	// deadline below caps total discovery time; http.Client.Timeout caps each
	// individual request and is the only bound that reaches lazy refresh
	// (go-oidc's RemoteKeySet strips cancellation from the construction context
	// via WithoutCancel).
	httpClient, err := oidcHTTPClient()
	if err != nil {
		return nil, err
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

		// Extract claims.
		//
		// iss, client_id, and jti feed the per-invocation audit-log identity
		// surface (SOL-149606). They are stashed in TokenInfo.Extra under the
		// exact keys "iss", "client_id", "jti"; internal/tools/identity.go
		// reads them by those keys. Drift-detection tests in that package pin
		// the allowlist.
		var claims struct {
			Sub      string `json:"sub"`
			Scope    string `json:"scope"`
			Iss      string `json:"iss"`
			ClientID string `json:"client_id"`
			Jti      string `json:"jti"`
		}

		if err := idToken.Claims(&claims); err != nil {
			return nil, fmt.Errorf("failed to extract claims: %w", err)
		}

		// Parse scopes from JWT (space-separated string per OAuth 2.0 spec)
		scopes := []string{}
		if claims.Scope != "" {
			scopes = strings.Split(claims.Scope, " ")
		}

		return &sdkauth.TokenInfo{
			UserID:     claims.Sub,
			Scopes:     scopes,
			Expiration: idToken.Expiry,
			Extra: map[string]any{
				"iss":       claims.Iss,
				"client_id": claims.ClientID,
				"jti":       claims.Jti,
			},
		}, nil
	}, nil
}

// oidcHTTPClient returns an HTTP client for OIDC discovery and JWKS refresh.
// The client is always bounded by oidcHTTPClientTimeout so that neither
// startup discovery nor lazy JWKS refresh can wedge on a slow or hung IdP
// (SOL-150219). When SSL_CERT_FILE is set the client additionally trusts that
// CA bundle: on macOS and Windows, Go's default TLS verification delegates to
// the OS-native trust store and ignores SSL_CERT_FILE, so an explicit RootCAs
// pool bypasses that delegation and makes the env var work consistently on
// every platform.
//
// This is the single place the OIDC client's timeout is established; callers
// must attach the returned client via oidc.ClientContext rather than building
// their own, so the bound cannot be silently dropped.
func oidcHTTPClient() (*http.Client, error) {
	client := &http.Client{Timeout: oidcHTTPClientTimeout}

	certFile := os.Getenv("SSL_CERT_FILE")
	if certFile == "" {
		return client, nil
	}
	certPEM, err := os.ReadFile(filepath.Clean(certFile))
	if err != nil {
		return nil, fmt.Errorf("SSL_CERT_FILE %q: %w", certFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("SSL_CERT_FILE %q contains no valid PEM certificates", certFile)
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	client.Transport = t
	return client, nil
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
