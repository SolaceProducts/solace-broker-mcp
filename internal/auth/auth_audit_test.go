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

// The AuthAuditHook calls a TokenVerifier makes on success/failure
// (SOL-152097), and the ClassifyAuthFailure reason each failure carries.
//
// fakeAuthHook stands in for audit.NewAuthHook so these tests assert on what
// internal/auth calls the hook with, without importing
// internal/observability/audit — that package already imports internal/auth
// for identity, so the reverse import here would be a cycle (see
// AuthAuditHook's doc in audit_emit.go). The schema of the actual emitted
// audit record is covered separately in
// internal/observability/audit/auth_hook_test.go.

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// authFailureCall records one Failure invocation.
type authFailureCall struct {
	reason, sub, clientID string
}

// fakeAuthHook implements AuthAuditHook, recording every call it receives.
type fakeAuthHook struct {
	successes []*sdkauth.TokenInfo
	failures  []authFailureCall
}

func (f *fakeAuthHook) Success(_ context.Context, info *sdkauth.TokenInfo) {
	f.successes = append(f.successes, info)
}

func (f *fakeAuthHook) Failure(_ context.Context, reason, sub, clientID string) {
	f.failures = append(f.failures, authFailureCall{reason, sub, clientID})
}

// oidcCfg returns a MCPClientAuthConfig pointed at mock, for tests that only
// need NewTokenVerifier (not the full HTTP middleware).
func oidcCfg(mock *mockOIDCServer) *config.ServerConfig {
	return &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeOAuth,
			Issuer:   mock.issuer,
			Audience: mock.audience,
		},
	}
}

// TestAuthHook_OIDC_Success pins the happy path: Success is called exactly
// once with the TokenInfo the verifier is about to return, Failure not at
// all.
func TestAuthHook_OIDC_Success(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{"sub": "auth0|user1"})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	info, err := verifier(context.Background(), token, nil)
	if err != nil {
		t.Fatalf("verifier returned error on a valid token: %v", err)
	}

	if len(hook.failures) != 0 {
		t.Errorf("Failure called %d time(s) on a valid token, want 0: %v", len(hook.failures), hook.failures)
	}
	if len(hook.successes) != 1 {
		t.Fatalf("Success called %d time(s), want 1", len(hook.successes))
	}
	if hook.successes[0] != info {
		t.Errorf("Success was not called with the same *TokenInfo the verifier returned")
	}
	if hook.successes[0].UserID != "auth0|user1" {
		t.Errorf("Success info.UserID = %q, want %q", hook.successes[0].UserID, "auth0|user1")
	}
}

// TestAuthHook_OIDC_ExpiredToken_ClassifiesExpired pins go-oidc's one
// exported error category: a token that verifies but has expired.
func TestAuthHook_OIDC_ExpiredToken_ClassifiesExpired(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{
		"exp": time.Now().Add(-1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	if _, err := verifier(context.Background(), token, nil); err == nil {
		t.Fatal("verifier accepted an expired token")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "expired", sub: "", clientID: ""})
}

// TestAuthHook_OIDC_WrongAudience_ClassifiesAudienceMismatch pins the
// audience-check failure, distinguished from the general invalid_token
// catch-all.
func TestAuthHook_OIDC_WrongAudience_ClassifiesAudienceMismatch(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{"aud": "wrong-audience"})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	if _, err := verifier(context.Background(), token, nil); err == nil {
		t.Fatal("verifier accepted a token with the wrong audience")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "audience_mismatch", sub: "", clientID: ""})
}

// TestAuthHook_OIDC_TamperedSignature_ClassifiesSignatureInvalid pins the
// signature-verification failure.
func TestAuthHook_OIDC_TamperedSignature_ClassifiesSignatureInvalid(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	tampered := token[:len(token)-5] + "XXXXX"

	if _, err := verifier(context.Background(), tampered, nil); err == nil {
		t.Fatal("verifier accepted a tampered signature")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "signature_invalid", sub: "", clientID: ""})
}

// TestAuthHook_OIDC_WrongIssuer_ClassifiesInvalidToken pins the catch-all
// bucket for a go-oidc failure with no more specific classification (see
// ClassifyAuthFailure's doc on issuer mismatch).
func TestAuthHook_OIDC_WrongIssuer_ClassifiesInvalidToken(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{"iss": "https://wrong-issuer.example.com"})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	if _, err := verifier(context.Background(), token, nil); err == nil {
		t.Fatal("verifier accepted a token from the wrong issuer")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "invalid_token", sub: "", clientID: ""})
}

// TestAuthHook_OIDC_MissingSubject_ClassifiesMissing_NoIdentity pins the
// "missing" bucket: the token verified (signature, issuer, audience, expiry
// all valid), but RFC 9068's hard-required sub claim is empty. No identity
// is attributed — sub is the very thing missing.
func TestAuthHook_OIDC_MissingSubject_ClassifiesMissing_NoIdentity(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{"sub": ""})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	if _, err := verifier(context.Background(), token, nil); err == nil {
		t.Fatal("verifier accepted a token with an empty subject")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "missing", sub: "", clientID: ""})
}

// TestAuthHook_OIDC_MalformedSubjectClaim_NoIdentity pins the fully-
// malformed case: sub itself does not decode as a string, so there is
// nothing bestEffortIdentity can recover either. Covers the "malformed"
// half of "cover both the parsed-but-rejected and the malformed/missing
// cases" (SOL-152097).
//
// A non-string sub fails inside go-oidc's own Verify — it decodes the
// payload into its own idToken struct (Subject string) before our code ever
// sees raw claims — so this exercises the "Verify failed" branch, not
// buildTokenInfo's. Either branch reports no identity: neither has readable
// claims to attribute from at the point it rejects the token.
func TestAuthHook_OIDC_MalformedSubjectClaim_NoIdentity(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	// A JSON number where sub must be a string.
	token, err := mock.createToken(map[string]interface{}{"sub": 12345})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	if _, err := verifier(context.Background(), token, nil); err == nil {
		t.Fatal("verifier accepted a token with a non-string sub claim")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "invalid_token", sub: "", clientID: ""})
}

// TestAuthHook_OIDC_ParsedButRejected_IdentityPopulated pins the other half:
// the token verified and sub/client_id both decoded cleanly, but a
// different claim (scope) is malformed and buildTokenInfo rejects the token
// over it. The auth_failure record still names the caller — "the token
// parsed far enough to yield them" (SOL-152097) — because bestEffortIdentity
// reads sub/client_id directly from the decoded claim set, independent of
// which claim buildTokenInfo happened to fail on.
func TestAuthHook_OIDC_ParsedButRejected_IdentityPopulated(t *testing.T) {
	mock := newMockOIDCServer(t)
	defer mock.close()

	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(oidcCfg(mock), nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	token, err := mock.createToken(map[string]interface{}{
		"sub":       "auth0|rejected-user",
		"client_id": "cursor-ide",
		// A JSON number where scope must be a string/absent.
		"scope": 42,
	})
	if err != nil {
		t.Fatalf("createToken: %v", err)
	}
	if _, err := verifier(context.Background(), token, nil); err == nil {
		t.Fatal("verifier accepted a token with a non-string scope claim")
	}

	assertSingleFailure(t, hook, authFailureCall{reason: "invalid_token", sub: "auth0|rejected-user", clientID: "cursor-ide"})
}

// TestAuthHook_StaticMode_Success and TestAuthHook_StaticMode_Failure pin
// that the dev/static verifier path reports through the same hook.
func TestAuthHook_StaticMode_Success(t *testing.T) {
	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "s3cr3t"},
	}
	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(cfg, nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	if _, err := verifier(context.Background(), "s3cr3t", nil); err != nil {
		t.Fatalf("verifier rejected the correct dev token: %v", err)
	}
	if len(hook.failures) != 0 || len(hook.successes) != 1 {
		t.Fatalf("successes=%d failures=%d, want 1/0", len(hook.successes), len(hook.failures))
	}
}

func TestAuthHook_StaticMode_Failure(t *testing.T) {
	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "s3cr3t"},
	}
	hook := &fakeAuthHook{}
	verifier, err := NewTokenVerifier(cfg, nil, hook)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}

	if _, err := verifier(context.Background(), "wrong-token", nil); err == nil {
		t.Fatal("verifier accepted the wrong dev token")
	}
	// A static-token mismatch carries no claims at all — nothing to
	// attribute, so both identity fields stay empty.
	assertSingleFailure(t, hook, authFailureCall{reason: "invalid_token", sub: "", clientID: ""})
}

// TestAuthHook_NilHook_NoPanic pins the nil contract: every pre-SOL-152097
// call site, passing nil explicitly, keeps working with no emission and no
// panic.
func TestAuthHook_NilHook_NoPanic(t *testing.T) {
	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeStatic, DevToken: "s3cr3t"},
	}
	verifier, err := NewTokenVerifier(cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewTokenVerifier: %v", err)
	}
	if _, err := verifier(context.Background(), "s3cr3t", nil); err != nil {
		t.Fatalf("verifier rejected the correct dev token with no hook wired: %v", err)
	}
	if _, err := verifier(context.Background(), "wrong", nil); err == nil {
		t.Fatal("verifier accepted the wrong dev token with no hook wired")
	}
}

// TestAuthHook_NoBearerToken_ReportsMissing pins the gap the review found:
// a request with no bearer token at all never reaches our TokenVerifier —
// go-sdk's RequireBearerToken rejects it before calling into the middleware
// chain we built — so without auditMissingBearerToken's peek, this shape of
// request (the most common unauthenticated-access pattern) produced no
// auth_failure record whatsoever. The 401 response itself, including
// WWW-Authenticate, must be byte-identical to a hook-less request; this
// wrapper only observes, it never changes what go-sdk decides.
func TestAuthHook_NoBearerToken_ReportsMissing(t *testing.T) {
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:        config.AuthModeStatic,
			DevToken:    "s3cr3t",
			ResourceURL: "http://localhost:9090/mcp",
		},
	}
	hook := &fakeAuthHook{}
	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler, hook)
	if err != nil {
		t.Fatalf("NewAuthMiddleware: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	// Deliberately no Authorization header.
	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("no WWW-Authenticate header on the 401 — this wrapper must not change the SDK's own response")
	}
	assertSingleFailure(t, hook, authFailureCall{reason: "missing", sub: "", clientID: ""})
}

// TestAuthHook_MalformedAuthorizationHeader_ReportsInvalidToken pins the
// other shape parseBearerToken's ok=false covers — a header present but not
// bearer-shaped (a Basic-scheme value, say) — and that it classifies
// separately from an absent header: "invalid_token", not "missing". Folding
// both into "missing" would inflate an unauthenticated-probe count with
// callers who presented something, just not a bearer token.
func TestAuthHook_MalformedAuthorizationHeader_ReportsInvalidToken(t *testing.T) {
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:        config.AuthModeStatic,
			DevToken:    "s3cr3t",
			ResourceURL: "http://localhost:9090/mcp",
		},
	}
	hook := &fakeAuthHook{}
	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler, hook)
	if err != nil {
		t.Fatalf("NewAuthMiddleware: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	assertSingleFailure(t, hook, authFailureCall{reason: "invalid_token", sub: "", clientID: ""})
}

// TestAuthHook_ValidBearerToken_DoesNotDoubleReportMissing pins the negative
// case: a well-formed Authorization header must not trip
// auditMissingBearerToken at all — only the verifier's own Success/Failure
// call should fire, exactly once.
func TestAuthHook_ValidBearerToken_DoesNotDoubleReportMissing(t *testing.T) {
	cfg := &config.ServerConfig{
		Port: 9090,
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:        config.AuthModeStatic,
			DevToken:    "s3cr3t",
			ResourceURL: "http://localhost:9090/mcp",
		},
	}
	hook := &fakeAuthHook{}
	middleware, err := NewAuthMiddleware(cfg, nil, dummyHandler, hook)
	if err != nil {
		t.Fatalf("NewAuthMiddleware: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	rec := httptest.NewRecorder()
	middleware.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if len(hook.failures) != 0 {
		t.Errorf("Failure called %d time(s) for a valid bearer token, want 0: %v", len(hook.failures), hook.failures)
	}
	if len(hook.successes) != 1 {
		t.Errorf("Success called %d time(s), want exactly 1 (not double-reported)", len(hook.successes))
	}
}

// assertSingleFailure fails the test unless hook recorded exactly one
// Failure call, matching want, and no Success calls.
func assertSingleFailure(t *testing.T, hook *fakeAuthHook, want authFailureCall) {
	t.Helper()
	if len(hook.successes) != 0 {
		t.Errorf("Success called %d time(s) on a rejected token, want 0", len(hook.successes))
	}
	if len(hook.failures) != 1 {
		t.Fatalf("Failure called %d time(s), want 1: %v", len(hook.failures), hook.failures)
	}
	if got := hook.failures[0]; got != want {
		t.Errorf("Failure call = %+v, want %+v", got, want)
	}
}

