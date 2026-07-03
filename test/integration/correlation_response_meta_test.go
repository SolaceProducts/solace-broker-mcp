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

package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaCorrelationKey mirrors the unexported tools.metaKeyCorrelationID const
// (register.go). It is duplicated here as a literal because the const is not
// exported; the tests below assert against this string, so a drift in the
// production const would surface as a failure here.
const metaCorrelationKey = "correlation_id"

// metaTestPool builds a *semp.BrokerPool with a single "dev" broker alias,
// using the public config + semp APIs (the same path production uses). The
// tests below never reach the broker wire — the stub handler ignores the SEMP
// clients — so the URL only needs to be valid config. The "dev" alias must be
// present because the tool calls pass {"broker":"dev","msgVpnName":"default"}.
func metaTestPool(t *testing.T) *semp.BrokerPool {
	t.Helper()
	cfgYAML := "mcp_client_auth:\n  mode: disabled\nbrokers:\n" +
		"  dev:\n    url: http://localhost:8081\n    auth:\n      mode: basic\n      username: admin\n      password: admin\n"
	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := semp.NewBrokerPool(cfg, nil)
	t.Cleanup(pool.Close)
	return pool
}

// metaStubHandler is a minimal tools.ToolHandler for these tests. It satisfies
// the exported interface and lets a test override Handle via handleFn; the
// default returns a success ToolResult. It stands in for the unexported
// stubHandler used inside internal/tools.
type metaStubHandler struct {
	name     string
	handleFn func(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error)
}

func (h *metaStubHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name:        h.name,
		Description: "A test tool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msgVpnName": map[string]any{"type": "string"},
			},
			"required": []string{"msgVpnName"},
		},
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
		Annotations: tools.Annotations{ReadOnly: true},
	}
}

func (h *metaStubHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	if h.handleFn != nil {
		return h.handleFn(ctx, tc, params)
	}
	return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
}

// metaTestServer builds a real MCP server with the given handler registered,
// fronts it with a StreamableHTTPHandler, and optionally wraps that in
// correlation.Middleware. It returns a connected client session driving a full
// HTTP round-trip — the same path production uses — so a tool call exercises
// the real register.go chokepoint where Meta is stamped. When withCorrelation
// is false the middleware is absent, modelling the capability-off case where
// correlation.From(ctx) is "" at the chokepoint.
//
// This composes two internal/ components — the tools register chokepoint and
// correlation.Middleware — through their public APIs, so it belongs in the
// integration tier per this directory's README: the correlation_id invariant
// is meaningless without the middleware wired in.
func metaTestServer(t *testing.T, h tools.ToolHandler, withCorrelation bool) *mcp.ClientSession {
	t.Helper()
	pool := metaTestPool(t)
	mgr := tools.NewToolManager(pool)
	mgr.Register(h)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	tools.RegisterWithServer(mgr, server, pool, true)

	var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	if withCorrelation {
		handler = correlation.Middleware(handler)
	}
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0.1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// TestCallToolResultMeta_CorrelationIDPresent pins SOL-151282: when the
// correlation capability is on, every CallToolResult flowing back through the
// register.go chokepoint carries Meta["correlation_id"] equal to the request's
// correlation ID. Covers both a success result and a tool-error result
// (handler returns an error -> buildErrorResult), since both must carry it.
func TestCallToolResultMeta_CorrelationIDPresent(t *testing.T) {
	t.Run("success result carries correlation_id", func(t *testing.T) {
		var seenCtxID string
		h := &metaStubHandler{name: "ok-tool"}
		h.handleFn = func(ctx context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
			seenCtxID = correlation.From(ctx)
			return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
		}
		session := metaTestServer(t, h, true)

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "ok-tool",
			Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if seenCtxID == "" {
			t.Fatal("handler saw no correlation ID on its context; the request ctx must reach the handler")
		}
		got, ok := res.Meta[metaCorrelationKey].(string)
		if !ok {
			t.Fatalf("result.Meta[%q] missing or not a string: %#v", metaCorrelationKey, res.Meta)
		}
		if got != seenCtxID {
			t.Errorf("result.Meta[%q] = %q, want it to equal the request correlation ID %q", metaCorrelationKey, got, seenCtxID)
		}
	})

	t.Run("error result carries correlation_id", func(t *testing.T) {
		h := &metaStubHandler{name: "err-tool"}
		h.handleFn = func(_ context.Context, _ *tools.ToolContext, _ map[string]any) (*tools.ToolResult, error) {
			return nil, context.DeadlineExceeded // any execution error -> buildErrorResult, IsError result
		}
		session := metaTestServer(t, h, true)

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "err-tool",
			Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected an error result, got IsError=false: %#v", res)
		}
		got, ok := res.Meta[metaCorrelationKey].(string)
		if !ok || got == "" {
			t.Errorf("error result.Meta[%q] missing/empty: %#v (error paths must carry it too)", metaCorrelationKey, res.Meta)
		}
	})
}

// TestCallToolResultMeta_CorrelationIDAbsent pins the capability-off contract:
// with no correlation middleware in the chain, correlation.From(ctx) is "" at
// the chokepoint, so no correlation_id key is added to Meta.
func TestCallToolResultMeta_CorrelationIDAbsent(t *testing.T) {
	h := &metaStubHandler{name: "ok-tool"}
	session := metaTestServer(t, h, false)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ok-tool",
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if v, present := res.Meta[metaCorrelationKey]; present {
		t.Errorf("result.Meta[%q] = %v present with correlation off, want absent", metaCorrelationKey, v)
	}
}
