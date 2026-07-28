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

// Verifies that OAuth errors from the token exchange are surfaced by
// logToolResult's type gate. For each sentinel error the exchanger can
// return, the "detail" field on the ERROR log line must contain the
// diagnostic message (not just the Go type name).
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	internalauth "github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/oauth/cache/cachetest"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp"
	"github.com/SolaceDev/solace-broker-mcp/internal/tokenexchange"
	"github.com/SolaceDev/solace-broker-mcp/internal/tools"
)

func TestOAuthErrorVisibility(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "ErrExchangeRejected/invalid_grant",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			},
		},
		{
			name: "ErrExchangeTransport/5xx",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte("upstream timeout"))
			},
		},
		{
			name: "ErrInvalidResponse/non-json-200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte("<html>SSO login page</html>"))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runOAuthVisibilityTest(t, tc.name, tc.handler)
		})
	}

	// "no subject token" is fmt.Errorf (not ExchangeError) — unreachable
	// in production due to config validation. Assert it stays type-only.
	t.Run("NoSubjectToken", func(t *testing.T) {
		runOAuthVisibilityTestNoSubjectToken(t)
	})
}

func runOAuthVisibilityTest(t *testing.T, label string, idpHandler http.HandlerFunc) {
	t.Helper()

	fakeIdP := httptest.NewTLSServer(idpHandler)
	defer fakeIdP.Close()

	// Fake broker — never reached because AddAuth fails first.
	fakeBroker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fake broker was hit — AddAuth should have failed first")
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeBroker.Close()

	tc := cachetest.Default(t)
	exchanger, err := tokenexchange.New(tokenexchange.Params{
		TokenURL:         fakeIdP.URL,
		ClientID:         "mcp-server",
		ClientAuthMethod: tokenexchange.ClientSecretBasic,
		ClientSecret:     "fake-secret",
		GrantType:        tokenexchange.GrantTypeTokenExchange,
		AudienceParam:    tokenexchange.AudienceParamAudience,
		HTTPClient:       fakeIdP.Client(),
		Cache:            tc,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}

	pool, mgr := buildOAuthPoolAndManager(t, fakeIdP.URL, fakeBroker.URL, exchanger)
	defer pool.Close()

	ctx := oauthInjectSubjectToken(t, "fake-jwt-for-exchange")
	detail := oauthCallToolAndCaptureDetail(t, mgr, ctx)

	if strings.HasPrefix(detail, "*") {
		t.Errorf("[%s] detail is Go type only (%q) — diagnostic message swallowed", label, detail)
	}
	if detail == "" {
		t.Errorf("[%s] detail is empty", label)
	}
}

func runOAuthVisibilityTestNoSubjectToken(t *testing.T) {
	t.Helper()

	fakeIdP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("IdP was hit — should have failed on missing subject token first")
	}))
	defer fakeIdP.Close()

	fakeBroker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("broker was hit")
	}))
	defer fakeBroker.Close()

	tc := cachetest.Default(t)
	exchanger, err := tokenexchange.New(tokenexchange.Params{
		TokenURL:         fakeIdP.URL,
		ClientID:         "mcp-server",
		ClientAuthMethod: tokenexchange.ClientSecretBasic,
		ClientSecret:     "fake-secret",
		GrantType:        tokenexchange.GrantTypeTokenExchange,
		AudienceParam:    tokenexchange.AudienceParamAudience,
		HTTPClient:       fakeIdP.Client(),
		Cache:            tc,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}

	pool, mgr := buildOAuthPoolAndManager(t, fakeIdP.URL, fakeBroker.URL, exchanger)
	defer pool.Close()

	detail := oauthCallToolAndCaptureDetail(t, mgr, context.Background())

	if !strings.Contains(detail, "no subject token") {
		t.Errorf("[NoSubjectToken] expected 'no subject token' in detail, got %q", detail)
	}
}

// --- stub handler ---

// oauthStubHandler is a minimal tools.ToolHandler that triggers the OAuth
// exchange path by calling SEMPv1.Execute. Same pattern as metaStubHandler
// in correlation_response_meta_test.go.
type oauthStubHandler struct {
	handleFn func(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error)
}

func (h *oauthStubHandler) Metadata() tools.Metadata {
	return tools.Metadata{
		Name:        "test-oauth-tool",
		Description: "integration test tool for OAuth error visibility",
		InputSchema: tools.EmptyObjectSchema(),
		OutputSchema: map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "object"},
		},
		Annotations: tools.Annotations{ReadOnly: true},
	}
}

func (h *oauthStubHandler) Handle(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
	return h.handleFn(ctx, tc, params)
}

// --- helpers ---

func buildOAuthPoolAndManager(t *testing.T, idpURL, brokerURL string, exchanger *tokenexchange.Exchanger) (*semp.BrokerPool, *tools.ToolManager) {
	t.Helper()

	yaml := fmt.Sprintf(`
# This test drives a self-signed httptest TLS broker, so insecure_skip_verify
# is required. Under mcp_client_auth.mode: oauth (production) that is refused at
# startup unless the operator opts in via allow_insecure_broker_tls: true.
allow_insecure_broker_tls: true
tls_terminated_upstream: true
mcp_client_auth:
  mode: oauth
  issuer: "https://fake-issuer.example.com"
  audience: "mcp-server"
  resource_url: "https://localhost:9999/mcp"
  tool_authorization:
    enabled: false
broker_oauth:
  idp_token_endpoint: %q
  mcp_server_client_id: mcp-server
  mcp_server_client_auth:
    client_secret_basic:
      secret: fake-secret
  grant_type: "urn:ietf:params:oauth:grant-type:token-exchange"
  audience_parameter_name: audience
brokers:
  oauth-broker:
    url: %q
    insecure_skip_verify: true
    auth:
      mode: oauth
      audience: "solace-broker"
`, idpURL, brokerURL)

	path := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	pool := semp.NewBrokerPool(cfg, exchanger)

	mgr := tools.NewToolManager(pool)
	mgr.Register(&oauthStubHandler{
		handleFn: func(ctx context.Context, tc *tools.ToolContext, params map[string]any) (*tools.ToolResult, error) {
			_, err := tc.SEMPv1Client.Execute(ctx, "<rpc><show><version/></show></rpc>")
			if err != nil {
				return nil, err
			}
			return &tools.ToolResult{StructuredContent: map[string]any{"step1": map[string]any{"ok": true}}}, nil
		},
	})

	return pool, mgr
}

func oauthInjectSubjectToken(t *testing.T, token string) context.Context {
	t.Helper()
	var ctx context.Context
	var mu sync.Mutex
	middleware := internalauth.InjectRawSubjectToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ctx = r.Context()
		mu.Unlock()
	}))
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	middleware.ServeHTTP(httptest.NewRecorder(), req)
	mu.Lock()
	defer mu.Unlock()
	if ctx == nil {
		t.Fatal("InjectRawSubjectToken middleware did not produce a context")
	}
	return ctx
}

func oauthCallToolAndCaptureDetail(t *testing.T, mgr *tools.ToolManager, ctx context.Context) string {
	t.Helper()

	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	_, _ = mgr.CallTool(ctx, "test-oauth-tool", map[string]any{"broker": "oauth-broker"}, tools.Identity{})

	var detail string
	for _, line := range bytes.Split(logBuf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		if m["msg"] == "tool invoked" && m["status"] == "error" {
			detail, _ = m["detail"].(string)
		}
	}

	return detail
}
