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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/coreos/go-oidc/v3/oidc"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func NewAuthMiddleware(cfg *config.ServerConfig, next http.Handler) (http.Handler, error) {
	if cfg.DevelopmentMode && cfg.ClientAuth.DevToken == "" {
		slog.Warn("authentication disabled — development mode with no dev token — not for production use")
		return next, nil
	}

	verifier, err := NewTokenVerifier(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create token verifier: %w", err)
	}

	// Construct the metadata URL at the server root
	// Config validation ensures ResourceURL is well-formed if set
	var metadataURL string
	if cfg.ClientAuth.ResourceURL != "" {
		// Can safely parse — config validation already checked this
		parsedURL, _ := url.Parse(cfg.ClientAuth.ResourceURL)
		metadataURL = fmt.Sprintf("%s://%s/.well-known/oauth-protected-resource", parsedURL.Scheme, parsedURL.Host)
	}

	middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
	})

	return middleware(next), nil
}

// NewTokenVerifier creates a TokenVerifier based on the server configuration.
// In development mode, it returns a static token verifier.
// In production mode, it uses OIDC/JWT verification with automatic key rotation.
// cfg has already been validated via config.validate().
func NewTokenVerifier(cfg *config.ServerConfig) (sdkauth.TokenVerifier, error) {
	if cfg.DevelopmentMode {
		slog.Warn("using static dev token — development mode — not for production use")
		return createStaticTokenVerifier(cfg.ClientAuth.DevToken), nil
	}

	slog.Info("using JWT token for authentication — production mode")
	return createOIDCTokenVerifier(cfg)
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

// NewProtectedResourceMetadataHandler creates an HTTP handler that serves
// OAuth 2.0 Protected Resource Metadata (RFC 9728) for the MCP server.
// This endpoint enables MCP clients to discover the authorization server
// and initiate browser-based OAuth flows (Authorization Code + PKCE).
// Returns nil when there's no OAuth configuration to advertise.
func NewProtectedResourceMetadataHandler(cfg *config.ServerConfig) http.Handler {
	// Only provide metadata endpoint when JWT validation is active
	if cfg.DevelopmentMode && cfg.ClientAuth.DevToken == "" {
		return nil
	}

	// Skip metadata endpoint when there's no issuer configured.
	// The endpoint's purpose is to advertise the OAuth authorization server,
	// so serving it with an empty issuer would be misleading to clients.
	if cfg.ClientAuth.Issuer == "" {
		return nil
	}

	// Use configured resource URL (required in production mode via config validation)
	resourceURL := cfg.ClientAuth.ResourceURL

	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               resourceURL,
		AuthorizationServers:   []string{cfg.ClientAuth.Issuer},
		ScopesSupported:        []string{},
		BearerMethodsSupported: []string{"header"},
	}

	return sdkauth.ProtectedResourceMetadataHandler(metadata)
}
