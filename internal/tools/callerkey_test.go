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

package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The caller key is what fair scheduling buckets on, so its derivation decides
// who shares a share with whom. Each row below is a real deployment shape.
func TestCallerKeyFromRequest(t *testing.T) {
	tests := []struct {
		name string
		req  *mcp.CallToolRequest
		want resilience.CallerKey
	}{
		{
			name: "nil request yields the shared bucket",
			req:  nil,
			want: resilience.CallerKey{},
		},
		{
			name: "disabled mode: no Extra at all",
			req:  &mcp.CallToolRequest{},
			want: resilience.CallerKey{},
		},
		{
			name: "Extra present but no token (disabled mode through the middleware)",
			req:  &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}},
			want: resilience.CallerKey{},
		},
		{
			name: "oauth mode: the raw subject becomes the top-level bucket",
			req: &mcp.CallToolRequest{
				Extra: &mcp.RequestExtra{TokenInfo: &sdkauth.TokenInfo{UserID: "auth0|abc123"}},
			},
			want: resilience.CallerKey{Subject: "auth0|abc123"},
		},
		{
			name: "static mode: every caller shares the dev-user subject",
			req: &mcp.CallToolRequest{
				Extra: &mcp.RequestExtra{TokenInfo: &sdkauth.TokenInfo{UserID: "dev-user"}},
			},
			want: resilience.CallerKey{Subject: "dev-user"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := callerKeyFromRequest(tc.req); got != tc.want {
				t.Errorf("callerKeyFromRequest() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// The subject must be the RAW token subject, not the audit-log projection.
//
// Identity's sub goes through sanitize.Claim, which caps at 256 bytes, so two
// subjects sharing a 256-byte prefix collapse to the same audit value. Using
// that as a fairness key would merge them into one bucket, which is a way for
// one caller to consume another's share. This is the specific mistake the
// design called out, so it gets its own test rather than a comment.
func TestCallerKeyUsesRawSubjectNotTheTruncatedAuditValue(t *testing.T) {
	const prefixLen = 256
	prefix := strings.Repeat("x", prefixLen)
	subA := prefix + "-caller-a"
	subB := prefix + "-caller-b"

	keyA := callerKeyFromRequest(&mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{TokenInfo: &sdkauth.TokenInfo{UserID: subA}},
	})
	keyB := callerKeyFromRequest(&mcp.CallToolRequest{
		Extra: &mcp.RequestExtra{TokenInfo: &sdkauth.TokenInfo{UserID: subB}},
	})

	if keyA == keyB {
		t.Fatalf("two subjects sharing a %d-byte prefix produced the same caller key (%q): "+
			"the key is being truncated, so one caller can be merged into another's "+
			"fairness bucket", prefixLen, keyA.Subject)
	}

	// And confirm the audit projection really does collapse them, so this test
	// keeps failing for the right reason if sanitize.Claim's cap ever moves.
	ctx := context.Background()
	auditA := NewIdentityFromPrincipal(auth.NewPrincipal(ctx, &sdkauth.TokenInfo{UserID: subA}))
	auditB := NewIdentityFromPrincipal(auth.NewPrincipal(ctx, &sdkauth.TokenInfo{UserID: subB}))
	if auditA.sub != auditB.sub {
		t.Skipf("audit projection no longer truncates these subjects (%q vs %q); "+
			"the collision this guards has moved", auditA.sub, auditB.sub)
	}
}

// Every request that can reach a broker must carry a caller key by the time it
// reaches a tool handler, because the handler's context is the one the Sender
// reads. Without this, a new dispatch path could quietly skip the stamping site
// and every request on it would share a single fairness bucket.
//
// Asserted on "was a key stamped", not on its value: through the in-memory
// transport there is no token and no session ID, so the correct key here is the
// empty one. Distinguishing that from an unstamped context is exactly why
// CallerKeyFrom reports a boolean.
func TestBrokerBoundToolCallAlwaysCarriesACallerKey(t *testing.T) {
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)

	var (
		sawCall bool
		stamped bool
		gotKey  resilience.CallerKey
	)
	probe := newStubHandler("callerkey-probe")
	probe.handleFn = func(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
		sawCall = true
		gotKey, stamped = resilience.CallerKeyFrom(ctx)
		return &ToolResult{StructuredContent: map[string]any{"ok": true}}, nil
	}
	mgr.Register(probe)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true, nil, "")

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	args := map[string]any{"broker": "dev", "msgVpnName": "default"}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "callerkey-probe", Arguments: args,
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if !sawCall {
		t.Fatal("the probe handler never ran")
	}
	if !stamped {
		t.Error("a tool handler that can reach a broker received a context with no caller key: " +
			"a dispatch path is bypassing the stamping site in RegisterWithServer, and every " +
			"request on it will share one fairness bucket")
	}
	if gotKey != (resilience.CallerKey{}) {
		t.Errorf("caller key = %+v, want the empty key: the in-memory transport supplies "+
			"neither a token subject nor a session ID", gotKey)
	}
}
