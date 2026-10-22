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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
)

// transportHandler builds a fresh production-shaped streamable-HTTP handler in
// the given mode over the shared conformance server. The options always come
// from newMCPStreamableOptions, so flipping the line that sets Stateless there
// is what turns these tests red. Each call is an independent handler with no
// shared session table, which is how two replicas behave.
func transportHandler(t *testing.T, stateless bool) *mcp.StreamableHTTPHandler {
	t.Helper()
	conformanceHandler(t) // builds the shared server once; fails the test if that cannot happen
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return conformanceServer
	}, newMCPStreamableOptions(stateless))
}

// bareRequest builds a request with no body, for the methods the transport
// only allows (or refuses) outside POST.
func bareRequest(method string) *http.Request {
	req := httptest.NewRequestWithContext(context.Background(), method, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	return req
}

// TestTransport_InitializeSessionID is the dangerous-line proof: an operator who
// sets MCP_TRANSPORT_STATELESS=true must get no Mcp-Session-Id back, and one who
// leaves it unset must still get one.
func TestTransport_InitializeSessionID(t *testing.T) {
	tests := []struct {
		name        string
		stateless   bool
		wantSession bool
	}{
		{name: "stateful issues a session id", stateless: false, wantSession: true},
		{name: "stateless issues no session id", stateless: true, wantSession: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postMCP(t, transportHandler(t, tt.stateless), initializeBodyFor("transport-test"), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("initialize status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			got := rec.Header().Get("Mcp-Session-Id")
			if tt.wantSession && got == "" {
				t.Error("initialize returned no Mcp-Session-Id; the stateful transport must issue one")
			}
			if !tt.wantSession && got != "" {
				t.Errorf("initialize returned Mcp-Session-Id %q; the stateless transport must not issue one", got)
			}
		})
	}
}

// TestTransport_GetAndDelete pins what a load balancer or client sees on the
// methods that only make sense with a session. Allow must be exactly "POST" in
// stateless mode; a bare 405 could come from some other rejection.
func TestTransport_GetAndDelete(t *testing.T) {
	tests := []struct {
		name      string
		stateless bool
		method    string
		wantCode  int
		wantAllow string
	}{
		{name: "stateless GET", stateless: true, method: http.MethodGet, wantCode: http.StatusMethodNotAllowed, wantAllow: "POST"},
		{name: "stateless DELETE", stateless: true, method: http.MethodDelete, wantCode: http.StatusMethodNotAllowed, wantAllow: "POST"},
		{name: "stateful GET without a session", stateless: false, method: http.MethodGet, wantCode: http.StatusBadRequest},
		{name: "stateful DELETE without a session", stateless: false, method: http.MethodDelete, wantCode: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			transportHandler(t, tt.stateless).ServeHTTP(rec, bareRequest(tt.method))
			if rec.Code != tt.wantCode {
				t.Errorf("%s status = %d, want %d; body: %s", tt.method, rec.Code, tt.wantCode, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("%s Allow = %q, want %q", tt.method, got, tt.wantAllow)
			}
			if got := rec.Header().Get("Mcp-Session-Id"); got != "" {
				t.Errorf("%s response carries Mcp-Session-Id %q, want none", tt.method, got)
			}
		})
	}
}

// TestTransport_RequestSurvivesAcrossReplicas is the operator's reason for the
// flag. Replica A answers initialize. The client's next request lands on replica
// B, a separate handler that never saw the session, carrying a session ID B
// cannot know. A stateless server answers it; a stateful one cannot.
func TestTransport_RequestSurvivesAcrossReplicas(t *testing.T) {
	const toolsList = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`
	tests := []struct {
		name      string
		stateless bool
		wantCode  int
	}{
		{name: "stateless serves a request with a session id from another replica", stateless: true, wantCode: http.StatusOK},
		{name: "stateful rejects it as an unknown session", stateless: false, wantCode: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replicaA := transportHandler(t, tt.stateless)
			replicaB := transportHandler(t, tt.stateless)

			initRec := postMCP(t, replicaA, initializeBodyFor("replica-test"), "")
			if initRec.Code != http.StatusOK {
				t.Fatalf("initialize on replica A status = %d, want 200; body: %s", initRec.Code, initRec.Body.String())
			}
			sessionID := initRec.Header().Get("Mcp-Session-Id")
			if sessionID == "" {
				sessionID = "session-issued-by-another-replica"
			}

			rec := postMCP(t, replicaB, toolsList, sessionID)
			if rec.Code != tt.wantCode {
				t.Fatalf("tools/list on replica B status = %d, want %d; body: %s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantCode != http.StatusOK {
				return
			}
			env := decodeSingleResponse(t, rec)
			if env.Error != nil || len(env.Result) == 0 {
				t.Errorf("tools/list on replica B returned no result; body: %s", rec.Body.String())
			}
		})
	}
}

// TestNewMCPStreamableOptions_Transport checks the constructor's output in
// both modes. It is the cheap secondary check: the HTTP tests above are the
// proof that the SDK honours these fields.
func TestNewMCPStreamableOptions_Transport(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		opts := newMCPStreamableOptions(stateless)
		if opts == nil {
			t.Fatalf("stateless=%v: options are nil", stateless)
		}
		if opts.Stateless != stateless {
			t.Errorf("stateless=%v: Stateless = %v", stateless, opts.Stateless)
		}
		if opts.SessionTimeout != defaults.DefaultMCPSessionIdleTimeout {
			t.Errorf("stateless=%v: SessionTimeout = %v, want %v", stateless, opts.SessionTimeout, defaults.DefaultMCPSessionIdleTimeout)
		}
		if opts.PropagateRequestCancellation != stateless {
			t.Errorf("stateless=%v: PropagateRequestCancellation = %v, want it to follow the setting",
				stateless, opts.PropagateRequestCancellation)
		}
	}
}

// TestLogConfigLoaded_NamesTransportMode loads a real config the way main does
// and reads the startup line an operator would grep. The mode must be on that
// line at the default INFO level, for both settings.
func TestLogConfigLoaded_NamesTransportMode(t *testing.T) {
	const yaml = `
mcp_client_auth:
  mode: static
  dev_token: test
brokers:
  dev:
    url: "http://localhost:8080"
    auth:
      mode: basic
      username: admin
      password: secret
`
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "default", value: "", want: false},
		{name: "stateless", value: "true", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MCP_TRANSPORT_STATELESS", tt.value)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
				t.Fatalf("writing config: %v", err)
			}
			cfg, err := config.LoadConfig(path)
			if err != nil {
				t.Fatalf("loading config: %v", err)
			}

			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			logConfigLoaded(cfg)

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("decoding log line %q: %v", buf.String(), err)
			}
			if rec["msg"] != "config loaded" || rec["level"] != "INFO" {
				t.Fatalf("unexpected record: %v", rec)
			}
			got, ok := rec["transport_stateless"].(bool)
			if !ok {
				t.Fatalf("config loaded line has no transport_stateless attribute: %v", rec)
			}
			if got != tt.want {
				t.Errorf("transport_stateless = %v, want %v", got, tt.want)
			}
		})
	}
}
