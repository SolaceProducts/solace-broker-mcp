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
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// dummyHandler returns 200 OK if the request passes through the auth middleware
var dummyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
})

// Test_AuthDisabled tests that all requests pass through when development_mode=true and dev_token=""
func Test_AuthDisabled(t *testing.T) {
	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: true,
		ClientAuth: config.ClientAuthConfig{
			DevToken: "", // Empty dev token means auth disabled
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	tests := []struct {
		name          string
		authHeader    string
		expectedCode  int
		expectedBody  string
	}{
		{
			name:         "no auth header",
			authHeader:   "",
			expectedCode: http.StatusOK,
			expectedBody: "OK",
		},
		{
			name:         "with bearer token",
			authHeader:   "Bearer some-random-token",
			expectedCode: http.StatusOK,
			expectedBody: "OK",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)

			if rec.Code != tt.expectedCode {
				t.Errorf("expected status %d, got %d", tt.expectedCode, rec.Code)
			}
			if strings.TrimSpace(rec.Body.String()) != tt.expectedBody {
				t.Errorf("expected body %q, got %q", tt.expectedBody, rec.Body.String())
			}
		})
	}
}

// Test_StaticDevToken tests static token validation in development mode
func Test_StaticDevToken(t *testing.T) {
	const validToken = "dev-secret-token-12345"

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: true,
		ClientAuth: config.ClientAuthConfig{
			DevToken:    validToken,
			ResourceURL: "http://localhost:9090/mcp",
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	tests := []struct {
		name         string
		authHeader   string
		expectedCode int
	}{
		{
			name:         "valid token",
			authHeader:   "Bearer " + validToken,
			expectedCode: http.StatusOK,
		},
		{
			name:         "wrong token",
			authHeader:   "Bearer wrong-token",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "token differs at first char",
			authHeader:   "Bearer xev-secret-token-12345",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "missing token",
			authHeader:   "",
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "malformed auth header - no bearer prefix",
			authHeader:   validToken,
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "malformed auth header - basic auth",
			authHeader:   "Basic " + validToken,
			expectedCode: http.StatusUnauthorized,
		},
		{
			name:         "case sensitive token - uppercase",
			authHeader:   "Bearer " + strings.ToUpper(validToken),
			expectedCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)

			if rec.Code != tt.expectedCode {
				t.Errorf("expected status %d, got %d", tt.expectedCode, rec.Code)
			}

			// Verify WWW-Authenticate header is present for 401/403
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				wwwAuth := rec.Header().Get("WWW-Authenticate")
				if wwwAuth == "" {
					t.Error("expected WWW-Authenticate header on 401/403 response")
				}
			}
		})
	}
}

// Test_DevelopmentModeFlexibility tests that development mode works without issuer/audience
func Test_DevelopmentModeFlexibility(t *testing.T) {
	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: true,
		ClientAuth: config.ClientAuthConfig{
			DevToken: "dev-token",
			Issuer:   "", // Missing but OK in dev mode
			Audience: "",
		},
	}

	_, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Errorf("expected no error in development mode without issuer/audience, got: %v", err)
	}
}

// Mock OIDC server for production mode tests
type mockOIDCServer struct {
	server   *httptest.Server
	issuer   string
	audience string
	signer   jose.Signer
}

func newMockOIDCServer(t *testing.T) *mockOIDCServer {
	t.Helper()

	// Generate RSA key for signing (go-oidc expects RS256 by default)
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: privateKey},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key-1"),
	)
	if err != nil {
		t.Fatalf("failed to create signer: %v", err)
	}

	mock := &mockOIDCServer{
		signer: signer,
	}

	// Create test server
	mux := http.NewServeMux()

	// OIDC discovery endpoint
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		config := map[string]interface{}{
			"issuer":   mock.issuer,
			"jwks_uri": mock.issuer + "/jwks",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(config)
	})

	// JWKS endpoint
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		jwks := jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{
				{
					Key:       &privateKey.PublicKey,
					KeyID:     "test-key-1",
					Algorithm: string(jose.RS256),
					Use:       "sig",
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwks)
	})

	mock.server = httptest.NewServer(mux)
	mock.issuer = mock.server.URL
	mock.audience = "test-audience"

	return mock
}

func (m *mockOIDCServer) close() {
	m.server.Close()
}

func (m *mockOIDCServer) createToken(claims map[string]interface{}) (string, error) {
	// Set standard claims if not provided
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = m.issuer
	}
	if _, ok := claims["aud"]; !ok {
		claims["aud"] = m.audience
	}
	if _, ok := claims["sub"]; !ok {
		claims["sub"] = "test-user"
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(1 * time.Hour).Unix()
	}
	if _, ok := claims["iat"]; !ok {
		claims["iat"] = time.Now().Unix()
	}

	builder := jwt.Signed(m.signer)
	builder = builder.Claims(claims)

	token, err := builder.Serialize()
	if err != nil {
		return "", fmt.Errorf("failed to serialize token: %w", err)
	}

	return token, nil
}

// Test_ValidJWTToken tests that valid JWT tokens are accepted
func Test_ValidJWTToken(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	// Create valid token
	token, err := mock.createToken(map[string]interface{}{
		"scope": "read write",
	})
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// Test_ExpiredJWTToken tests that expired JWT tokens are rejected
func Test_ExpiredJWTToken(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	// Create expired token
	token, err := mock.createToken(map[string]interface{}{
		"exp": time.Now().Add(-1 * time.Hour).Unix(), // Expired 1 hour ago
	})
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for expired token, got %d", rec.Code)
	}
}

// Test_WrongJWTAudience tests that tokens with wrong audience are rejected
func Test_WrongJWTAudience(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	// Create token with wrong audience
	token, err := mock.createToken(map[string]interface{}{
		"aud": "wrong-audience",
	})
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for wrong audience, got %d", rec.Code)
	}
}

// Test_WrongJWTIssuer tests that tokens from wrong issuer are rejected
func Test_WrongJWTIssuer(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	// Create token with wrong issuer
	token, err := mock.createToken(map[string]interface{}{
		"iss": "https://wrong-issuer.example.com",
	})
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for wrong issuer, got %d", rec.Code)
	}
}

// Test_InvalidJWTSignature tests that tokens with invalid signatures are rejected
func Test_InvalidJWTSignature(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	// Create token with valid structure but tamper with it
	token, err := mock.createToken(map[string]interface{}{})
	if err != nil {
		t.Fatalf("failed to create token: %v", err)
	}

	// Tamper with the token signature (replace last char)
	tamperedToken := token[:len(token)-5] + "XXXXX"

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+tamperedToken)

	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for invalid signature, got %d", rec.Code)
	}
}

// Test_MissingRequiredScope tests that tokens missing required scopes get 403
func Test_MissingRequiredScope(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:         mock.issuer,
			Audience:       mock.audience,
			RequiredScopes: []string{"read", "write", "admin"},
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	tests := []struct {
		name         string
		scopes       string
		expectedCode int
	}{
		{
			name:         "all required scopes present",
			scopes:       "read write admin",
			expectedCode: http.StatusOK,
		},
		{
			name:         "missing one scope",
			scopes:       "read write",
			expectedCode: http.StatusForbidden,
		},
		{
			name:         "no scopes",
			scopes:       "",
			expectedCode: http.StatusForbidden,
		},
		{
			name:         "extra scopes but all required present",
			scopes:       "read write admin delete",
			expectedCode: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token, err := mock.createToken(map[string]interface{}{
				"scope": tt.scopes,
			})
			if err != nil {
				t.Fatalf("failed to create token: %v", err)
			}

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			req.Header.Set("Authorization", "Bearer "+token)

			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)

			if rec.Code != tt.expectedCode {
				t.Errorf("expected status %d, got %d: %s", tt.expectedCode, rec.Code, rec.Body.String())
			}

			// Verify WWW-Authenticate header for 403
			if rec.Code == http.StatusForbidden {
				wwwAuth := rec.Header().Get("WWW-Authenticate")
				if wwwAuth == "" {
					t.Error("expected WWW-Authenticate header on 403 response")
				}
			}
		})
	}
}

// Test_NoJWTToken tests that requests without JWT tokens are rejected
func Test_NoJWTToken(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port:            9090,
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:      mock.issuer,
			Audience:    mock.audience,
			ResourceURL: "http://localhost:9090/mcp",
		},
	}

	middleware, err := NewAuthMiddleware(cfg, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	// No Authorization header

	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401 for missing token, got %d", rec.Code)
	}

	wwwAuth := rec.Header().Get("WWW-Authenticate")
	if wwwAuth == "" {
		t.Error("expected WWW-Authenticate header on 401 response")
	}
}

// Test_OIDCProviderUnreachable verifies that NewTokenVerifier fails fast when
// the OIDC provider is unreachable during initialization in production mode.
func Test_OIDCProviderUnreachable(t *testing.T) {
	cfg := &config.ServerConfig{
		DevelopmentMode: false,
		ClientAuth: config.ClientAuthConfig{
			Issuer:   "https://nonexistent.example.com",
			Audience: "test",
		},
	}

	_, err := NewTokenVerifier(cfg)
	if err == nil {
		t.Error("expected error when OIDC provider is unreachable")
	}
}

// Test_WWWAuthenticateHeaderFormat verifies that 401 responses include the correct
// WWW-Authenticate header with resource_metadata parameter per MCP spec and RFC 9728.
func Test_WWWAuthenticateHeaderFormat(t *testing.T) {
	tests := []struct {
		name        string
		port        int
		devMode     bool
		devToken    string
		tlsEnabled  bool
		expectHTTPS bool
	}{
		{"dev token mode", 9090, true, "dev-secret-token", false, false},
		{"production mode", 9090, false, "", false, false},
		{"production with TLS", 9443, false, "", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := "http"
			if tt.tlsEnabled {
				scheme = "https"
			}
			cfg := &config.ServerConfig{
				Port:            tt.port,
				DevelopmentMode: tt.devMode,
				ClientAuth: config.ClientAuthConfig{
					DevToken:    tt.devToken,
					ResourceURL: fmt.Sprintf("%s://localhost:%d/mcp", scheme, tt.port),
				},
			}
			if tt.tlsEnabled {
				cfg.TLSCertFile = "/path/to/cert.pem"
				cfg.TLSKeyFile = "/path/to/key.pem"
			}
			if !tt.devMode {
				mock := newMockOIDCServer(t)
				defer mock.close()
				cfg.ClientAuth.Issuer = mock.issuer
				cfg.ClientAuth.Audience = mock.audience
			}

			middleware, err := NewAuthMiddleware(cfg, dummyHandler)
			if err != nil {
				t.Fatalf("failed to create middleware: %v", err)
			}

			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
			rec := httptest.NewRecorder()
			middleware.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("expected status 401, got %d", rec.Code)
			}

			wwwAuth := rec.Header().Get("WWW-Authenticate")
			if !strings.HasPrefix(wwwAuth, "Bearer ") {
				t.Errorf("WWW-Authenticate should start with 'Bearer ', got: %q", wwwAuth)
			}
			if !strings.Contains(wwwAuth, "resource_metadata=") {
				t.Errorf("WWW-Authenticate should contain 'resource_metadata=', got: %q", wwwAuth)
			}

			scheme = "http"
			if tt.expectHTTPS {
				scheme = "https"
			}
			expectedURL := fmt.Sprintf("%s://localhost:%d/.well-known/oauth-protected-resource", scheme, tt.port)
			if !strings.Contains(wwwAuth, expectedURL) {
				t.Errorf("expected metadata URL %q in header, got: %q", expectedURL, wwwAuth)
			}
		})
	}
}

// Test_ProtectedResourceMetadata tests the OAuth Protected Resource Metadata endpoint (RFC 9728)
func Test_ProtectedResourceMetadata(t *testing.T) {
	checkStringField := func(t *testing.T, m map[string]interface{}, field, expected string) {
		t.Helper()
		if actual, ok := m[field].(string); !ok || actual != expected {
			t.Errorf("%s: expected %q, got %v", field, expected, m[field])
		}
	}

	checkStringArray := func(t *testing.T, m map[string]interface{}, field string, expected []string) {
		t.Helper()
		arr, ok := m[field].([]interface{})
		if !ok {
			t.Errorf("%s: expected array, got %T", field, m[field])
			return
		}
		if len(arr) != len(expected) {
			t.Errorf("%s: expected %d items, got %d", field, len(expected), len(arr))
		}
		for i, exp := range expected {
			if i < len(arr) {
				if actual, ok := arr[i].(string); !ok || actual != exp {
					t.Errorf("%s[%d]: expected %q, got %v", field, i, exp, arr[i])
				}
			}
		}
	}

	tests := []struct {
		name          string
		port          int
		devMode       bool
		devToken      string
		tlsEnabled    bool
		issuer        string
		scopes        []string
		expectHandler bool
	}{
		{"production with issuer", 9090, false, "", false, "https://auth.example.com", []string{"read", "write"}, true},
		{"production with TLS", 9443, false, "", true, "https://auth.example.com", []string{"admin"}, true},
		{"dev mode with token", 9090, true, "dev-token", false, "https://auth.example.com", []string{"read"}, true},
		{"dev mode without token", 9090, true, "", false, "", nil, false},
		{"production empty scopes", 9090, false, "", false, "https://auth.example.com", []string{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := "http"
			if tt.tlsEnabled {
				scheme = "https"
			}
			cfg := &config.ServerConfig{
				Port:            tt.port,
				DevelopmentMode: tt.devMode,
				ClientAuth: config.ClientAuthConfig{
					DevToken:       tt.devToken,
					Issuer:         tt.issuer,
					Audience:       "solace-mcp-server",
					RequiredScopes: tt.scopes,
					ResourceURL:    fmt.Sprintf("%s://localhost:%d/mcp", scheme, tt.port),
				},
			}
			if tt.tlsEnabled {
				cfg.TLSCertFile = "/path/to/cert.pem"
				cfg.TLSKeyFile = "/path/to/key.pem"
			}

			handler := NewProtectedResourceMetadataHandler(cfg)
			if !tt.expectHandler {
				if handler != nil {
					t.Error("expected nil handler")
				}
				return
			}
			if handler == nil {
				t.Fatal("expected handler, got nil")
			}

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/.well-known/oauth-protected-resource", nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Errorf("expected status 200, got %d", rec.Code)
			}
			if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
				t.Error("expected Content-Type: application/json")
			}
			if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
				t.Error("expected Access-Control-Allow-Origin: *")
			}

			var metadata map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &metadata); err != nil {
				t.Fatalf("failed to parse JSON: %v", err)
			}

			// Compute expected resource URL
			scheme = "http"
			if tt.tlsEnabled {
				scheme = "https"
			}
			expectedResource := fmt.Sprintf("%s://localhost:%d/mcp", scheme, tt.port)
			checkStringField(t, metadata, "resource", expectedResource)
			checkStringArray(t, metadata, "authorization_servers", []string{tt.issuer})
			checkStringArray(t, metadata, "bearer_methods_supported", []string{"header"})

			// scopes_supported is optional per RFC 9728, may be omitted when empty
			if len(tt.scopes) > 0 {
				checkStringArray(t, metadata, "scopes_supported", tt.scopes)
			}
		})
	}
}
