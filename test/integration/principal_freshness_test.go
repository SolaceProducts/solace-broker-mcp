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

// Freshness of the audited caller identity across one MCP session
// (SOL-152087).
//
// This is the test the story needed and did not have. Identity population
// first shipped in review as an http.Handler middleware reading
// sdkauth.TokenInfoFromContext, which looks right and passes every unit test.
// It is wrong in the transport this server actually runs: in stateful
// streamable HTTP the go-sdk builds the jsonrpc2 connection once, from the
// POST that established the session, and every later message is dispatched on
// a context descended from that one. Per-POST context values therefore never
// reach a tool handler, so the audit line named the token that opened the
// session for as long as the session lived — through every client token
// refresh. Only the SDK's per-message RequestExtra tracks the caller.
//
// The whole point of the story is that an audit record names the credential
// that acted, so the property is asserted here at the seam where it broke:
// two tool calls on one session presenting different tokens must produce two
// audit lines naming their own tokens.

package integration_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rotatingVerifier accepts any bearer token and reports it back as the jti and
// client_id, with a constant sub. Standing in for an IdP whose access tokens
// are refreshed mid-session: the caller is the same person throughout, but each
// token is a distinct credential. sub must stay constant because the SDK
// rejects a session whose requests present a different user.
func rotatingVerifier(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
	return &sdkauth.TokenInfo{
		UserID:     "auth0|stable-subject",
		Scopes:     []string{"openid"},
		Expiration: time.Now().Add(time.Hour),
		Extra: map[string]any{
			"iss":       "https://idp.example.com",
			"client_id": "client-of-" + token,
			"jti":       token,
		},
	}, nil
}

// tokenSwapTransport sets the bearer token on every outbound request from a
// pointer the test controls, so one client session can present a different
// credential per call.
type tokenSwapTransport struct{ token *atomic.Pointer[string] }

func (t *tokenSwapTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+*t.token.Load())
	return http.DefaultTransport.RoundTrip(clone)
}

// TestPrincipalFreshness_AuditLineNamesTheCallingToken pins per-request
// identity through the real composition: SDK bearer verification, the
// raw-subject-token injection DEP-001 relies on, and the principal middleware,
// in front of a real tool registration.
func TestPrincipalFreshness_AuditLineNamesTheCallingToken(t *testing.T) {
	pool := metaTestPool(t)
	mgr := tools.NewToolManager(pool)
	mgr.Register(&metaStubHandler{name: "test-tool"})

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	tools.RegisterWithServer(mgr, server, pool, true, nil, "")
	server.AddReceivingMiddleware(auth.PrincipalMiddleware())

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server }, nil)
	// Same order as auth.NewAuthMiddleware composes them.
	handler := sdkauth.RequireBearerToken(rotatingVerifier, nil)(
		auth.InjectRawSubjectToken(mcpHandler))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	current := &atomic.Pointer[string]{}
	first, second := "token-first", "token-second"
	current.Store(&first)

	var buf strings.Builder
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: &tokenSwapTransport{token: current}},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	args := map[string]any{"broker": "dev", "msgVpnName": "default"}

	// Call 1 on the token that opened the session.
	if _, err := session.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "test-tool", Arguments: args}); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	// Call 2 after a token refresh — same subject, new credential.
	current.Store(&second)
	if _, err := session.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "test-tool", Arguments: args}); err != nil {
		t.Fatalf("call 2: %v", err)
	}

	lines := auditLinesFor(t, buf.String(), "tool invoked")
	if len(lines) != 2 {
		t.Fatalf("want 2 %q lines, got %d:\n%s", "tool invoked", len(lines), buf.String())
	}

	for i, want := range []string{first, second} {
		rec := lines[i]
		if got := rec["sub"]; got != "auth0|stable-subject" {
			t.Errorf("line %d: sub = %v, want the verified subject", i+1, got)
		}
		if got := rec["jti"]; got != want {
			t.Errorf("line %d: jti = %v, want %q — the audit line must name the token presented on THIS call, not the one that opened the session",
				i+1, got, want)
		}
		if got := rec["client_id"]; got != "client-of-"+want {
			t.Errorf("line %d: client_id = %v, want %q", i+1, got, "client-of-"+want)
		}
	}
}

// auditLinesFor returns the parsed JSON records whose msg matches, in order.
func auditLinesFor(t *testing.T, logged, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(logged, "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue // interleaved non-JSON output is not what this asserts
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}
