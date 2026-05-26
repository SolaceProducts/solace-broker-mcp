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
	"github.com/coreos/go-oidc/v3/oidc"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func NewAuthMiddleware(cfg *config.ServerConfig, next http.Handler) (http.Handler, error) {
	// Auth backend selection mirrors client_auth.mode. Insecure-mode signaling
	// lives in cmd/server/main.go via auth.LogStartupBanner — DO NOT add WARN
	// logs here. See docs/superpowers/specs/2026-05-20-client-auth-mode-design.md.
	switch cfg.ClientAuth.Mode {
	case config.AuthModeDisabled:
		return next, nil
	case config.AuthModeStatic, config.AuthModeOAuth:
		// fall through to the verifier construction below
	default:
		return nil, fmt.Errorf("internal: NewAuthMiddleware called with unsupported client_auth.mode %q (validator should have rejected this)", cfg.ClientAuth.Mode)
	}

	verifier, err := NewTokenVerifier(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create token verifier: %w", err)
	}

	// Construct the metadata URL at the server root.
	// Config validation ensures ResourceURL is well-formed if set.
	var metadataURL string
	if cfg.ClientAuth.ResourceURL != "" {
		parsedURL, _ := url.Parse(cfg.ClientAuth.ResourceURL)
		metadataURL = fmt.Sprintf("%s://%s/.well-known/oauth-protected-resource", parsedURL.Scheme, parsedURL.Host)
	}

	middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
	})

	return middleware(next), nil
}

// NewTokenVerifier creates a TokenVerifier based on cfg.ClientAuth.Mode.
//   - AuthModeStatic → constant-time compare against cfg.ClientAuth.DevToken
//   - AuthModeOAuth  → OIDC/JWT verification with automatic key rotation
//
// cfg has already been validated via config.validate(); other modes are
// programming errors.
func NewTokenVerifier(cfg *config.ServerConfig) (sdkauth.TokenVerifier, error) {
	switch cfg.ClientAuth.Mode {
	case config.AuthModeStatic:
		return createStaticTokenVerifier(cfg.ClientAuth.DevToken), nil
	case config.AuthModeOAuth:
		return createOIDCTokenVerifier(cfg)
	default:
		return nil, fmt.Errorf("internal: NewTokenVerifier called with unsupported client_auth.mode %q (validator should have rejected this)", cfg.ClientAuth.Mode)
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
	// Create OIDC provider - fetches .well-known/openid-configuration and JWKS
	// Use a timeout to fail fast if the issuer is unreachable
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if httpClient := oidcHTTPClient(); httpClient != nil {
		ctx = oidc.ClientContext(ctx, httpClient)
	}

	oidcProvider, err := oidc.NewProvider(ctx, cfg.ClientAuth.Issuer)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to identity provider at %s (is it reachable?): %w", cfg.ClientAuth.Issuer, err)
	}

	// Create verifier that validates issuer, audience, and signature
	verifier := oidcProvider.Verifier(&oidc.Config{
		ClientID: cfg.ClientAuth.Audience,
	})

	return func(ctx context.Context, token string, req *http.Request) (*sdkauth.TokenInfo, error) {
		// Verify JWT signature and standard claims (iss, aud, exp)
		idToken, err := verifier.Verify(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}

		// Extract claims
		var claims struct {
			Sub      string `json:"sub"`
			Scope    string `json:"scope"`
			ClientID string `json:"client_id"`
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
		}, nil
	}, nil
}

// oidcHTTPClient returns a custom HTTP client that trusts the CA bundle in
// SSL_CERT_FILE, or nil when that variable is unset/unreadable. On macOS and
// Windows, Go's default TLS verification delegates to the OS-native trust
// store and ignores SSL_CERT_FILE; an explicit RootCAs pool bypasses that
// delegation so the env var works consistently on every platform.
func oidcHTTPClient() *http.Client {
	certFile := os.Getenv("SSL_CERT_FILE")
	if certFile == "" {
		return nil
	}
	certPEM, err := os.ReadFile(filepath.Clean(certFile))
	if err != nil {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	pool.AppendCertsFromPEM(certPEM)
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs: pool,
			},
		},
	}
}

// NewProtectedResourceMetadataHandler creates an HTTP handler that serves
// OAuth 2.0 Protected Resource Metadata (RFC 9728) for the MCP server.
// This endpoint enables MCP clients to discover the authorization server
// and initiate browser-based OAuth flows (Authorization Code + PKCE).
// Only served under client_auth.mode == "oauth"; returns nil otherwise.
func NewProtectedResourceMetadataHandler(cfg *config.ServerConfig) http.Handler {
	if cfg.ClientAuth.Mode != config.AuthModeOAuth {
		return nil
	}

	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               cfg.ClientAuth.ResourceURL,
		AuthorizationServers:   []string{cfg.ClientAuth.Issuer},
		ScopesSupported:        []string{"openid"},
		BearerMethodsSupported: []string{"header"},
	}

	return sdkauth.ProtectedResourceMetadataHandler(metadata)
}
