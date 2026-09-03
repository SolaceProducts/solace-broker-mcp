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

// Coverage for every site that projects auth.Principal onto an audit record
// (SOL-152087).
//
// There are five, and before this file only one of them — withAuthorization —
// had a test: replacing auth.PrincipalFrom(ctx) with auth.Principal{} at the
// other four left the whole suite green, including at the "tool invoked"
// chokepoint every registered tool flows through. Total loss of caller
// identity from the primary audit line was a passing build.
//
// Each test below asserts the field VALUES, not merely that the keys exist.
// The failure this guards against is a record with the right shape naming the
// wrong caller, which a presence-only assertion cannot see.

package tools

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// principalFixture is the caller every test in this file audits. Values are
// distinct per field so a projection that crosses two of them is visible.
func principalFixture() auth.Principal {
	return auth.NewPrincipal(context.Background(), &sdkauth.TokenInfo{
		UserID: "auth0|emit-site",
		Extra: map[string]any{
			"iss":       "https://idp.emit.example",
			"client_id": "emit-client",
			"jti":       "jti-emit",
		},
	})
}

// wantIdentity is principalFixture as it must appear on an audit record.
var wantIdentity = map[string]string{
	"sub":       "auth0|emit-site",
	"iss":       "https://idp.emit.example",
	"client_id": "emit-client",
	"jti":       "jti-emit",
}

func assertIdentityValues(t *testing.T, rec map[string]any, where string) {
	t.Helper()
	for k, want := range wantIdentity {
		got, ok := rec[k]
		if !ok {
			t.Errorf("%s: audit record has no %q field; identity was lost on this path", where, k)
			continue
		}
		if got != want {
			t.Errorf("%s: %s = %v, want %q", where, k, got, want)
		}
	}
}

// sessionWithPrincipal starts an in-memory MCP server whose handler context
// carries principalFixture, and returns a connected client session plus the
// captured log buffer.
//
// Seeding at server.Run is what a test can do: the SDK derives every handler
// context from the one the connection was created with. In production the
// per-request auth.PrincipalMiddleware supplies it instead, which is covered
// in internal/auth and end to end in
// test/integration/principal_freshness_test.go.
func sessionWithPrincipal(t *testing.T, register func(*mcp.Server, *ToolManager)) (*mcp.ClientSession, *bytes.Buffer) {
	t.Helper()

	buf := &bytes.Buffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(old) })

	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(newStubHandler("test-tool"))
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	register(server, mgr)

	ctx := auth.WithPrincipal(context.Background(), principalFixture())
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, buf
}

// TestEmitSite_toolInvoked_carriesPrincipal covers register.go's dispatch
// closure — the audit chokepoint for every tool registered through the
// manager.
func TestEmitSite_toolInvoked_carriesPrincipal(t *testing.T) {
	session, buf := sessionWithPrincipal(t, func(s *mcp.Server, mgr *ToolManager) {
		RegisterWithServer(mgr, s, newRegTestPool(t), true, nil, "")
	})

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "test-tool",
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	lines := auditLines(t, buf, "test-tool")
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), buf.String())
	}
	assertIdentityValues(t, lines[0], "tool invoked (register.go dispatch closure)")
}

// TestEmitSite_toolInvoked_badArguments_carriesPrincipal covers the
// argument-parse branch of the same closure, which emits its own audit line
// (SOL-153765) and reads the principal separately.
func TestEmitSite_toolInvoked_badArguments_carriesPrincipal(t *testing.T) {
	session, buf := sessionWithPrincipal(t, func(s *mcp.Server, mgr *ToolManager) {
		RegisterWithServer(mgr, s, newRegTestPool(t), true, nil, "")
	})

	// A non-object arguments payload fails json.Unmarshal inside the closure.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "test-tool",
		Arguments: []any{"not", "an", "object"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	lines := auditLines(t, buf, "test-tool")
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), buf.String())
	}
	if lines[0]["error_type"] != "bad_request" {
		t.Fatalf("want the bad_request branch, got error_type=%v", lines[0]["error_type"])
	}
	assertIdentityValues(t, lines[0], "tool invoked (bad-arguments branch)")
}

// TestEmitSite_listBrokers_carriesPrincipal covers RegisterListBrokers, which
// emits its own audit line because it bypasses ToolManager.CallTool.
func TestEmitSite_listBrokers_carriesPrincipal(t *testing.T) {
	session, buf := sessionWithPrincipal(t, func(s *mcp.Server, _ *ToolManager) {
		RegisterListBrokers(s, newRegTestPool(t))
	})

	if _, err := session.CallTool(context.Background(),
		&mcp.CallToolParams{Name: "list-brokers"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	lines := auditLines(t, buf, "list-brokers")
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), buf.String())
	}
	assertIdentityValues(t, lines[0], "tool invoked (list-brokers)")
}

// TestEmitSite_describeSempSchema_carriesPrincipal covers the third handler
// that emits its own audit line.
func TestEmitSite_describeSempSchema_carriesPrincipal(t *testing.T) {
	session, buf := sessionWithPrincipal(t, func(s *mcp.Server, _ *ToolManager) {
		if err := RegisterDescribeSempSchema(s, specs.FS); err != nil {
			t.Fatalf("RegisterDescribeSempSchema: %v", err)
		}
	})

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      describeSempSchemaToolName,
		Arguments: map[string]any{"operation": "config/createMsgVpnQueue"},
	}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	lines := auditLines(t, buf, describeSempSchemaToolName)
	if len(lines) != 1 {
		t.Fatalf("want 1 audit line, got %d: %s", len(lines), buf.String())
	}
	assertIdentityValues(t, lines[0], "tool invoked (describe-semp-schema)")
}

// TestEmitSite_listFilter_carriesPrincipal covers the tools/list filter's
// audit record, which had no identity assertion at all.
func TestEmitSite_listFilter_carriesPrincipal(t *testing.T) {
	buf, cleanup := captureSlog(t)
	defer cleanup()

	next, _ := listHandler(toolsResult(listBrokersToolName, "get-broker-status"), nil)
	wrapped := WithListFiltering(policyGranting(t, []string{"Ops"}, "get-broker-status"), "groups")(next)

	ctx := auth.WithPrincipal(context.Background(), principalFixture())
	if _, err := wrapped(ctx, methodToolsList, listToolsRequest([]string{"Ops"})); err != nil {
		t.Fatalf("middleware returned error: %v", err)
	}

	assertIdentityValues(t, assertOneRecord(t, buf), "tool list filter")
}
