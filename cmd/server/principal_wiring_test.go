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

// Registration order of the caller-identity middleware (SOL-152087).
//
// auth.PrincipalMiddleware must be registered AFTER tools.WithListFiltering,
// because AddReceivingMiddleware wraps the current handler — so the last
// registration is the outermost and runs first, and the filter reads the
// principal the middleware installs. Nothing in the type system enforces
// that; reordering two statements in main() would silently strip identity
// from every tools/list audit record while leaving all other tests green.
// This test pins the order against the real wiring.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tools"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestPrincipalReachesListFiltering drives a real tools/list over HTTP with a
// verified bearer token and asserts the filter's audit record names the
// caller. It fails if the two middlewares are registered in the wrong order.
func TestPrincipalReachesListFiltering(t *testing.T) {
	buf, cleanup := captureStartupLog(t)
	defer cleanup()

	cfg := makeEnabledConfig()
	on := true
	cfg.MCPClientAuth.ToolAuthorization.FilterToolsList = &on
	// Grant the caller's group one tool so the allow path emits a record.
	cfg.MCPClientAuth.ToolAuthorization.AccessLevelGroups = map[string][]string{
		"Ops": {"get-broker-status"},
	}

	pool := semp.NewBrokerPool(cfg, nil)
	t.Cleanup(pool.Close)
	mgr := tools.NewToolManager(pool)
	server := newTestServer()
	tools.RegisterWithServer(mgr, server, pool, true, policyFrom(t, cfg), "groups")
	tools.RegisterListBrokers(server, pool)

	// Exactly the order main() uses.
	installToolListFiltering(server, cfg, policyFrom(t, cfg), "groups")
	server.AddReceivingMiddleware(auth.PrincipalMiddleware())

	verifier := func(ctx context.Context, token string, r *http.Request) (*sdkauth.TokenInfo, error) {
		return &sdkauth.TokenInfo{
			UserID:     "auth0|wiring",
			Expiration: time.Now().Add(time.Hour),
			Extra: map[string]any{
				"iss":                         "https://idp.wiring.example",
				"client_id":                   "wiring-client",
				"jti":                         "jti-wiring",
				authz.TokenInfoExtraKeyGroups: []string{"Ops"},
			},
		}, nil
	}
	handler := sdkauth.RequireBearerToken(verifier, nil)(
		mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL,
		HTTPClient: &http.Client{Transport: bearer("wiring-token")},
	}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	rec := findFilterRecord(t, buf.String())
	want := map[string]string{
		"sub":       "auth0|wiring",
		"iss":       "https://idp.wiring.example",
		"client_id": "wiring-client",
		"jti":       "jti-wiring",
	}
	for k, v := range want {
		got, ok := rec[k]
		if !ok {
			t.Errorf("tool list filter record has no %q — PrincipalMiddleware must be registered AFTER installToolListFiltering so it runs first", k)
			continue
		}
		if got != v {
			t.Errorf("%s = %v, want %q", k, got, v)
		}
	}
}

// bearer returns a RoundTripper that sets a fixed bearer token.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(clone)
}

// findFilterRecord returns the single "tool list filter" record.
func findFilterRecord(t *testing.T, logged string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimRight(logged, "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] == "tool list filter" {
			found = append(found, rec)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 \"tool list filter\" record, got %d:\n%s", len(found), logged)
	}
	return found[0]
}
