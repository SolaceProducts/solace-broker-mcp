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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// dummyHandler returns 200 OK if the request passes through the auth middleware
var dummyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
})

// TestMain seeds the process-global system certificate pool with a clean
// environment before any test runs.
//
// On Linux (but not macOS), crypto/x509 reads SSL_CERT_FILE when it lazily
// loads the system roots, and caches the result process-wide via sync.Once
// (crypto/x509/root.go). The first trigger wins: whichever test first calls
// x509.SystemCertPool() — which idpclient.NewHTTPClient does on the
// SSL_CERT_FILE path — or performs a TLS handshake with nil RootCAs, seeds
// that cache with the SSL_CERT_FILE value in effect at that moment, for the
// rest of the binary.
//
// Without this, a test that points SSL_CERT_FILE at a test CA (e.g.
// Test_OIDCJWKSRefreshTimeout_SSLCertFile) would leak that CA into the global
// roots and break sibling tests that assert an untrusted server is rejected
// (Test_NewTokenVerifier_TLSWithSSLCertFile's "TLS fails without
// SSL_CERT_FILE"). Firing the sync.Once here, with the env cleared, makes
// those assertions deterministic regardless of test order. macOS ignores
// SSL_CERT_FILE for system roots, which is why the leak only surfaces in
// Linux CI.
func TestMain(m *testing.M) {
	os.Unsetenv("SSL_CERT_FILE")
	os.Unsetenv("SSL_CERT_DIR")
	_, _ = x509.SystemCertPool()
	os.Exit(m.Run())
}

// Test_NewAuthMiddleware_Disabled tests that all requests pass through when
// mcp_client_auth.mode is "disabled" — the explicit no-auth dev mode replacing
// the old (pre-SOL-149989) silent dev-mode + empty dev-token bypass —
// which is now structurally impossible: mcp_client_auth.mode is required and
// has no implicit "disabled if dev_token empty" fallback.
func Test_NewAuthMiddleware_Disabled(t *testing.T) {
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode: config.AuthModeDisabled,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	tests := []struct {
		name         string
		authHeader   string
		expectedCode int
	}{
		{"no auth header", "", http.StatusOK},
		{"with bearer token", "Bearer some-random-token", http.StatusOK},
		{"with garbage", "not-even-bearer", http.StatusOK},
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
		})
	}
}

// Test_StaticDevToken tests static token validation in static (dev-token) mode.
func Test_StaticDevToken(t *testing.T) {
	const validToken = "dev-secret-token-12345"

	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:        config.AuthModeStatic,
			DevToken:    validToken,
			ResourceURL: "http://localhost:9090/mcp",
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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

// Test_StaticMode_AllowsMissingIssuerAndAudience verifies that static mode
// constructs middleware successfully when OAuth-only fields (issuer, audience)
// are absent — these fields are ignored off-mode per the mcp_client_auth.mode spec.
func Test_StaticMode_AllowsMissingIssuerAndAudience(t *testing.T) {
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeStatic,
			DevToken: "dev-token",
			Issuer:   "", // Missing but OK in dev mode
			Audience: "",
		},
	}

	_, err := NewAuthMiddleware(cfg, nil, dummyHandler)
	if err != nil {
		t.Errorf("expected no error under mcp_client_auth.mode: static without issuer/audience, got: %v", err)
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
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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

// Test_NoJWTToken tests that requests without JWT tokens are rejected
func Test_NoJWTToken(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:        config.AuthModeOAuth,
			Issuer:      mock.issuer,
			Audience:    mock.audience,
			ResourceURL: "http://localhost:9090/mcp",
		},
	}

	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   "https://nonexistent.example.com",
			Audience: "test",
		},
	}

	_, err := NewTokenVerifier(cfg, nil)
	if err == nil {
		t.Error("expected error when OIDC provider is unreachable")
	}
}

// Test_OIDCJWKSRefreshTimeout verifies that lazy JWKS refresh during token
// verification respects the bounded HTTP client timeout instead of blocking
// indefinitely on a hung IdP. Reproduces SOL-150219.
func Test_OIDCJWKSRefreshTimeout(t *testing.T) {
	// 500ms = fast-enough feedback without flaking on a busy CI runner.
	shortClient, err := idpclient.NewHTTPClient(idpclient.WithTimeout(500 * time.Millisecond))
	if err != nil {
		t.Fatalf("idpclient.NewHTTPClient: %v", err)
	}

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

	// Mock IdP: discovery responds fast; /jwks blocks until either the
	// request context is canceled (the fixed path: http.Client.Timeout fires
	// at 500ms) or the test cleanup releases it (safety net for the buggy
	// path so the test process doesn't hang on server.Close()).
	mux := http.NewServeMux()
	var issuer string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"issuer":   issuer,
			"jwks_uri": issuer + "/jwks",
		})
	})
	releaseJWKS := make(chan struct{})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseJWKS:
		}
	})

	server := httptest.NewServer(mux)
	// Cleanup order (LIFO): releaseJWKS closes FIRST so any in-flight handler
	// unblocks before server.Close() waits on its handler wg.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseJWKS) })
	issuer = server.URL

	audience := "test-audience"
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   issuer,
			Audience: audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, shortClient, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	token, err := jwt.Signed(signer).Claims(map[string]interface{}{
		"iss": issuer,
		"aud": audience,
		"sub": "test-user",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}).Serialize()
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	// 2s budget = 4x the 500ms HTTP client timeout. Without the SOL-150219
	// fix /jwks hangs and this fails the budget; with it, verification
	// completes (returning 401) at ~500ms when http.Client.Timeout fires.
	done := make(chan struct{})
	go func() {
		middleware.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 after JWKS timeout, got %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("token verification did not return within 2s — JWKS refresh likely hung (SOL-150219 regression)")
	}
}

// Test_OIDCJWKSRefreshTimeout_SSLCertFile is the SSL_CERT_FILE counterpart
// of Test_OIDCJWKSRefreshTimeout. The original SOL-150219 fix left this
// branch unbounded because the custom-CA client overwrote the bounded one;
// /jwks hangs to prove the bound still fires when SSL_CERT_FILE is set.
func Test_OIDCJWKSRefreshTimeout_SSLCertFile(t *testing.T) {
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

	// Mock IdP over TLS: discovery responds fast; /jwks blocks until either the
	// request context is canceled (the fixed path: http.Client.Timeout fires at
	// 500ms) or test cleanup releases it (safety net for the buggy path).
	mux := http.NewServeMux()
	var issuer string
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"issuer":   issuer,
			"jwks_uri": issuer + "/jwks",
		})
	})
	releaseJWKS := make(chan struct{})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-releaseJWKS:
		}
	})

	server := httptest.NewTLSServer(mux)
	// Cleanup order (LIFO): releaseJWKS closes FIRST so any in-flight handler
	// unblocks before server.Close() waits on its handler wg.
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(releaseJWKS) })
	issuer = server.URL

	// Trust the self-signed server cert via SSL_CERT_FILE so discovery and the
	// JWKS client validate TLS. Without this the verifier could not be built.
	serverCert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("failed to parse server cert: %v", err)
	}
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCert.Raw}), 0600); err != nil {
		t.Fatalf("failed to write CA file: %v", err)
	}
	t.Setenv("SSL_CERT_FILE", caPath)

	// Built AFTER SSL_CERT_FILE is set so the constructor picks up the
	// test CA — proves the bound still fires on the custom-CA branch.
	shortClient, err := idpclient.NewHTTPClient(idpclient.WithTimeout(500 * time.Millisecond))
	if err != nil {
		t.Fatalf("idpclient.NewHTTPClient with test CA: %v", err)
	}

	audience := "test-audience"
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   issuer,
			Audience: audience,
		},
	}

	middleware, err := NewAuthMiddleware(cfg, shortClient, dummyHandler)
	if err != nil {
		t.Fatalf("failed to create middleware: %v", err)
	}

	token, err := jwt.Signed(signer).Claims(map[string]interface{}{
		"iss": issuer,
		"aud": audience,
		"sub": "test-user",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}).Serialize()
	if err != nil {
		t.Fatalf("failed to sign token: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	// 2s budget = 4x the 500ms HTTP client timeout. Without the bound on
	// the SSL_CERT_FILE path /jwks hangs and this fails the budget; with
	// it, verification completes (returning 401) at ~500ms.
	done := make(chan struct{})
	go func() {
		middleware.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("expected 401 after JWKS timeout, got %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("token verification did not return within 2s — JWKS refresh likely hung on SSL_CERT_FILE path (SOL-150219 regression)")
	}
}

// Test_WWWAuthenticateHeaderFormat verifies that 401 responses include the correct
// WWW-Authenticate header with resource_metadata parameter per MCP spec and RFC 9728.
func Test_WWWAuthenticateHeaderFormat(t *testing.T) {
	tests := []struct {
		name        string
		port        int
		mode        string
		devToken    string
		tlsEnabled  bool
		expectHTTPS bool
	}{
		{"dev token mode", 9090, config.AuthModeStatic, "dev-secret-token", false, false},
		{"production mode", 9090, config.AuthModeOAuth, "", false, false},
		{"production with TLS", 9443, config.AuthModeOAuth, "", true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := "http"
			if tt.tlsEnabled {
				scheme = "https"
			}
			cfg := &config.ServerConfig{
				Port: tt.port,
				MCPClientAuth: config.MCPClientAuthConfig{
					Mode:        tt.mode,
					DevToken:    tt.devToken,
					ResourceURL: fmt.Sprintf("%s://localhost:%d/mcp", scheme, tt.port),
				},
			}
			if tt.tlsEnabled {
				cfg.TLSCertFile = "/path/to/cert.pem"
				cfg.TLSKeyFile = "/path/to/key.pem"
			}
			if tt.mode == config.AuthModeOAuth {
				mock := newMockOIDCServer(t)
				defer mock.close()
				cfg.MCPClientAuth.Issuer = mock.issuer
				cfg.MCPClientAuth.Audience = mock.audience
			}

			middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler)
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
		mode          string
		devToken      string
		tlsEnabled    bool
		issuer        string
		expectHandler bool
	}{
		{"production with issuer", 9090, config.AuthModeOAuth, "", false, "https://auth.example.com", true},
		{"production with TLS", 9443, config.AuthModeOAuth, "", true, "https://auth.example.com", true},
		{"static mode with token", 9090, config.AuthModeStatic, "dev-token", false, "https://auth.example.com", false},
		{"disabled mode", 9090, config.AuthModeDisabled, "", false, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := "http"
			if tt.tlsEnabled {
				scheme = "https"
			}
			cfg := &config.ServerConfig{
				Port: tt.port,
				MCPClientAuth: config.MCPClientAuthConfig{
					Mode:        tt.mode,
					DevToken:    tt.devToken,
					Issuer:      tt.issuer,
					Audience:    "solace-mcp-server",
					ResourceURL: fmt.Sprintf("%s://localhost:%d/mcp", scheme, tt.port),
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
			checkStringArray(t, metadata, "scopes_supported", []string{"openid"})
			checkStringArray(t, metadata, "bearer_methods_supported", []string{"header"})
		})
	}
}

func Test_PRMHandler_Disabled(t *testing.T) {
	cfg := &config.ServerConfig{MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeDisabled}}
	if h := NewProtectedResourceMetadataHandler(cfg); h != nil {
		t.Errorf("expected nil PRM handler for mode: disabled, got %T", h)
	}
}

func Test_PRMHandler_Static(t *testing.T) {
	cfg := &config.ServerConfig{MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "x"}}
	if h := NewProtectedResourceMetadataHandler(cfg); h != nil {
		t.Errorf("expected nil PRM handler for mode: static, got %T", h)
	}
}

func Test_PRMHandler_OAuth(t *testing.T) {
	cfg := &config.ServerConfig{MCPClientAuth: config.MCPClientAuthConfig{
		Mode:        config.AuthModeOAuth,
		Issuer:      "https://idp.example.com",
		Audience:    "mcp",
		ResourceURL: "https://mcp.example.com/mcp",
	}}
	if h := NewProtectedResourceMetadataHandler(cfg); h == nil {
		t.Error("expected non-nil PRM handler for mode: oauth")
	}
}

// Test_OIDCVerifier_PopulatesTokenInfo_AllClaims verifies that the OIDC
// verifier extracts sub/iss/client_id/jti and stashes the latter three in
// TokenInfo.Extra under fixed keys. SOL-149606 wires these into audit logs;
// this test pins the contract between the verifier and the identity layer in
// internal/tools.
func Test_OIDCVerifier_PopulatesTokenInfo_AllClaims(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	verifier, err := NewTokenVerifier(cfg, nil)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{
		"sub":       "user-42",
		"client_id": "cursor-ide",
		"jti":       "jti-abc-123",
	})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}

	info, err := verifier(context.Background(), token, nil)
	if err != nil {
		t.Fatalf("verifier returned error: %v", err)
	}

	if info.UserID != "user-42" {
		t.Errorf("UserID = %q, want %q", info.UserID, "user-42")
	}
	if got := info.Extra["iss"]; got != mock.issuer {
		t.Errorf("Extra[iss] = %v, want %q", got, mock.issuer)
	}
	if got := info.Extra["client_id"]; got != "cursor-ide" {
		t.Errorf("Extra[client_id] = %v, want %q", got, "cursor-ide")
	}
	if got := info.Extra["jti"]; got != "jti-abc-123" {
		t.Errorf("Extra[jti] = %v, want %q", got, "jti-abc-123")
	}
}

// Test_OIDCVerifier_MissingOptionalClaims_LeftEmptyString verifies that when
// the IdP omits optional claims (jti, client_id), the verifier still
// populates the Extra keys — with empty string values. The identity layer
// normalizes empty to "<absent>" for audit logs; this test pins that the keys
// are always present in Extra so the normalizer has something to read.
func Test_OIDCVerifier_MissingOptionalClaims_LeftEmptyString(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}

	verifier, err := NewTokenVerifier(cfg, nil)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	// Token with no client_id, no jti — only the required claims set by
	// createToken (sub, iss, aud, exp, iat).
	token, err := mock.createToken(map[string]interface{}{})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}

	info, err := verifier(context.Background(), token, nil)
	if err != nil {
		t.Fatalf("verifier returned error: %v", err)
	}

	// Required claim — iss is set by the mock to the issuer URL.
	if got := info.Extra["iss"]; got != mock.issuer {
		t.Errorf("Extra[iss] = %v, want %q", got, mock.issuer)
	}
	// Missing claims unmarshal to the zero value of their Go type (string→"").
	// The Extra key MUST still be present — identity normalization depends on
	// reading the key, not on probing for its existence.
	for _, key := range []string{"client_id", "jti"} {
		v, ok := info.Extra[key]
		if !ok {
			t.Errorf("Extra[%q] missing — verifier should always set the key", key)
			continue
		}
		if s, isString := v.(string); !isString || s != "" {
			t.Errorf("Extra[%q] = %v (%T), want \"\"", key, v, v)
		}
	}
}

// Test_NewTokenVerifier_TLSWithSSLCertFile proves that on the corporate-CA
// path (a self-signed HTTPS IdP trusted via SSL_CERT_FILE) NewTokenVerifier
// can complete OIDC discovery, and that the same call fails when
// SSL_CERT_FILE is unset. The per-branch behavior of the underlying HTTP
// client (timeout invariant, SSL_CERT_FILE error branches) is covered
// directly in internal/idpclient/client_test.go; this test exercises the
// integration with the OIDC verifier consumer.
func Test_NewTokenVerifier_TLSWithSSLCertFile(t *testing.T) {
	var tlsServer *httptest.Server
	tlsServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":   tlsServer.URL,
			"jwks_uri": tlsServer.URL + "/jwks",
		})
	}))
	defer tlsServer.Close()

	validCert := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(
		validCert,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}),
		0600,
	); err != nil {
		t.Fatalf("write CA file: %v", err)
	}

	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   tlsServer.URL,
			Audience: "test",
		},
	}

	t.Run("TLS fails without SSL_CERT_FILE", func(t *testing.T) {
		t.Setenv("SSL_CERT_FILE", "")
		_, err := NewTokenVerifier(cfg, nil)
		if err == nil {
			t.Error("expected TLS error when SSL_CERT_FILE is unset")
		}
	})

	t.Run("TLS succeeds with SSL_CERT_FILE", func(t *testing.T) {
		t.Setenv("SSL_CERT_FILE", validCert)
		_, err := NewTokenVerifier(cfg, nil)
		if err != nil {
			t.Errorf("expected success with SSL_CERT_FILE set, got: %v", err)
		}
	})
}
