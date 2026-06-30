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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// metaTestServer builds a real MCP server with the given handler registered,
// fronts it with a StreamableHTTPHandler, and optionally wraps that in
// correlation.Middleware. It returns a connected client session driving a full
// HTTP round-trip — the same path production uses — so a tool call exercises
// the real register.go chokepoint where Meta is stamped. When withCorrelation
// is false the middleware is absent, modelling the capability-off case where
// correlation.From(ctx) is "" at the chokepoint.
func metaTestServer(t *testing.T, h ToolHandler, withCorrelation bool) *mcp.ClientSession {
	t.Helper()
	pool := newRegTestPool(t)
	mgr := NewToolManager(pool)
	mgr.Register(h)

	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.1.0"}, nil)
	RegisterWithServer(mgr, server, pool, true)

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
		h := newStubHandler("ok-tool")
		h.handleFn = func(ctx context.Context, _ *ToolContext, _ map[string]any) (*ToolResult, error) {
			seenCtxID = correlation.From(ctx)
			return &ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
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
		got, ok := res.Meta[metaKeyCorrelationID].(string)
		if !ok {
			t.Fatalf("result.Meta[%q] missing or not a string: %#v", metaKeyCorrelationID, res.Meta)
		}
		if got != seenCtxID {
			t.Errorf("result.Meta[%q] = %q, want it to equal the request correlation ID %q", metaKeyCorrelationID, got, seenCtxID)
		}
	})

	t.Run("error result carries correlation_id", func(t *testing.T) {
		h := newStubHandler("err-tool")
		h.handleFn = func(_ context.Context, _ *ToolContext, _ map[string]any) (*ToolResult, error) {
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
		got, ok := res.Meta[metaKeyCorrelationID].(string)
		if !ok || got == "" {
			t.Errorf("error result.Meta[%q] missing/empty: %#v (error paths must carry it too)", metaKeyCorrelationID, res.Meta)
		}
	})
}

// TestStampCorrelationID_Unit exercises stampCorrelationID's full input domain
// directly: nil result, empty ctx ID, nil-Meta init, and the
// preserve-existing-entries guarantee.
func TestStampCorrelationID_Unit(t *testing.T) {
	t.Run("nil result is a no-op (no panic)", func(t *testing.T) {
		stampCorrelationID(correlation.With(context.Background(), "id"), nil)
	})

	t.Run("empty ctx ID adds nothing and leaves Meta nil", func(t *testing.T) {
		res := &mcp.CallToolResult{}
		stampCorrelationID(context.Background(), res)
		if res.Meta != nil {
			t.Errorf("Meta = %#v, want nil when ctx has no correlation ID", res.Meta)
		}
	})

	t.Run("nil Meta is initialized and stamped", func(t *testing.T) {
		res := &mcp.CallToolResult{}
		stampCorrelationID(correlation.With(context.Background(), "the-id"), res)
		if got := res.Meta[metaKeyCorrelationID]; got != "the-id" {
			t.Errorf("Meta[%q] = %v, want %q", metaKeyCorrelationID, got, "the-id")
		}
	})

	t.Run("existing Meta entries are preserved", func(t *testing.T) {
		res := &mcp.CallToolResult{Meta: mcp.Meta{"keep": "value"}}
		stampCorrelationID(correlation.With(context.Background(), "the-id"), res)
		if got := res.Meta["keep"]; got != "value" {
			t.Errorf("pre-existing Meta[\"keep\"] = %v, want it preserved as %q", got, "value")
		}
		if got := res.Meta[metaKeyCorrelationID]; got != "the-id" {
			t.Errorf("Meta[%q] = %v, want %q", metaKeyCorrelationID, got, "the-id")
		}
	})
}

// TestCallToolResultMeta_CorrelationIDAbsent pins the capability-off contract:
// with no correlation middleware in the chain, correlation.From(ctx) is "" at
// the chokepoint, so no correlation_id key is added to Meta.
func TestCallToolResultMeta_CorrelationIDAbsent(t *testing.T) {
	h := newStubHandler("ok-tool")
	session := metaTestServer(t, h, false)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ok-tool",
		Arguments: map[string]any{"broker": "dev", "msgVpnName": "default"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if v, present := res.Meta[metaKeyCorrelationID]; present {
		t.Errorf("result.Meta[%q] = %v present with correlation off, want absent", metaKeyCorrelationID, v)
	}
}
