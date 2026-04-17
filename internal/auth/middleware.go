package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
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

	// Construct the metadata URL using the same logic as NewProtectedResourceMetadataHandler
	metadataURL := cfg.ClientAuth.ResourceURL
	if metadataURL == "" {
		scheme := "http"
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			scheme = "https"
		}
		metadataURL = fmt.Sprintf("%s://localhost:%d", scheme, cfg.Port)
	}
	metadataURL += "/.well-known/oauth-protected-resource"

	middleware := sdkauth.RequireBearerToken(verifier, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
		Scopes:              cfg.ClientAuth.RequiredScopes,
	})

	return middleware(next), nil
}

// NewTokenVerifier creates a TokenVerifier based on the server configuration.
// In development mode, it returns a static token verifier.
// In production mode, it uses OIDC/JWT verification with automatic key rotation.
func NewTokenVerifier(cfg *config.ServerConfig) (sdkauth.TokenVerifier, error) {
	if cfg.DevelopmentMode {
		slog.Warn("using static dev token — development mode — not for production use")
		return createStaticTokenVerifier(cfg.ClientAuth.DevToken), nil
	}

	// Validate required fields for production mode
	if cfg.ClientAuth.Issuer == "" {
		return nil, fmt.Errorf("issuer is required in production mode")
	}
	if cfg.ClientAuth.Audience == "" {
		return nil, fmt.Errorf("audience is required in production mode")
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
	oidcProvider, err := oidc.NewProvider(context.Background(), cfg.ClientAuth.Issuer)
	if err != nil {
		return nil, fmt.Errorf("failed to create OIDC provider: %w", err)
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
func NewProtectedResourceMetadataHandler(cfg *config.ServerConfig) http.Handler {
	// Only provide metadata endpoint when JWT validation is active
	if cfg.DevelopmentMode && cfg.ClientAuth.DevToken == "" {
		return nil
	}

	// Use configured resource URL, or fall back to localhost in development mode
	// (production mode requires resource_url via config validation)
	resourceURL := cfg.ClientAuth.ResourceURL
	if resourceURL == "" {
		scheme := "http"
		if cfg.TLSCertFile != "" && cfg.TLSKeyFile != "" {
			scheme = "https"
		}
		resourceURL = fmt.Sprintf("%s://localhost:%d/mcp", scheme, cfg.Port)
	}

	// Ensure scopes is always at least an empty slice (not nil) for proper JSON serialization
	scopes := cfg.ClientAuth.RequiredScopes
	if scopes == nil {
		scopes = []string{}
	}

	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               resourceURL,
		AuthorizationServers:   []string{cfg.ClientAuth.Issuer},
		ScopesSupported:        scopes,
		BearerMethodsSupported: []string{"header"},
	}

	return sdkauth.ProtectedResourceMetadataHandler(metadata)
}

