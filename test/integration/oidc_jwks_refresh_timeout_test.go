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

// Invariant under test: the http.Client.Timeout bound on the IdP-bound HTTP
// client survives through go-oidc's RemoteKeySet into lazy JWKS refresh
// (SOL-150219). Both the default-trust-store path and the SSL_CERT_FILE
// corporate-CA path are covered, because the original fix left the
// custom-CA branch unbounded.
//
// These compose internal/auth's middleware, the OIDC token verifier, and
// the go-oidc library against a fake hung IdP — the assertion only makes
// sense with all three layers wired together, so per the test/integration/
// README they belong in this tier rather than next to a single component's
// unit tests.
package integration

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/idpclient"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// dummyHandler returns 200 OK for requests that make it through the auth
// middleware. Defined locally because the auth package's identically-named
// helper is unexported.
var dummyHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
})

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
		json.NewEncoder(w).Encode(map[string]string{
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

	middleware, err := auth.NewAuthMiddleware(cfg, shortClient, dummyHandler)
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
		json.NewEncoder(w).Encode(map[string]string{
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
	caPath := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(
		caPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		0600,
	); err != nil {
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

	middleware, err := auth.NewAuthMiddleware(cfg, shortClient, dummyHandler)
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
