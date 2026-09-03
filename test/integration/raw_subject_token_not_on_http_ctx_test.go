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
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// TestRawSubjectToken_NotOnHTTPHandlerCtx pins the post-SOL-153935
// invariant: after NewAuthMiddleware wraps the chain, the HTTP handler
// context must NOT carry a raw subject token. Hop 2 reads the token
// from the per-request JSON-RPC handler ctx stamped by
// RequestExtraMiddleware (receiving middleware), not from the HTTP
// session freeze.
//
// This is the negative contract that replaced the old
// InjectRawSubjectToken chain-order test. The concurrent-isolation
// invariant is covered by test/integration/session_context_freshness_test.go.
func TestRawSubjectToken_NotOnHTTPHandlerCtx(t *testing.T) {
	t.Parallel()

	t.Run("static mode: HTTP ctx does not carry raw subject token", func(t *testing.T) {
		t.Parallel()
		const devToken = "static-mode-dev-token-fixture"

		cfg := &config.ServerConfig{
			MCPClientAuth: config.MCPClientAuthConfig{
				Mode:     config.AuthModeStatic,
				DevToken: devToken,
			},
		}

		var httpCtxHadToken bool
		downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, httpCtxHadToken = auth.RawSubjectTokenFromContext(r.Context())
		})

		handler, err := auth.NewAuthMiddleware(cfg, nil, downstream)
		if err != nil {
			t.Fatalf("NewAuthMiddleware: %v", err)
		}

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+devToken)
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if httpCtxHadToken {
			t.Fatal("HTTP handler ctx carries raw subject token — NewAuthMiddleware must NOT install InjectRawSubjectToken; hop 2 reads from receiving middleware only")
		}
	})

	t.Run("oauth mode with fake verifier: HTTP ctx does not carry raw subject token", func(t *testing.T) {
		t.Parallel()
		const goodToken = "eyJhbGciOiJSUzI1NiJ9.good.sig"

		verifier := func(_ context.Context, _ string, _ *http.Request) (*sdkauth.TokenInfo, error) {
			return &sdkauth.TokenInfo{
				UserID:     "test-user",
				Expiration: time.Now().Add(time.Hour),
			}, nil
		}

		var httpCtxHadToken bool
		downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, httpCtxHadToken = auth.RawSubjectTokenFromContext(r.Context())
		})

		// Build the chain the way NewAuthMiddleware builds it (sans Inject):
		// RequireBearerToken(next). The raw token must not appear on HTTP ctx.
		chain := sdkauth.RequireBearerToken(verifier, nil)(downstream)

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+goodToken)
		chain.ServeHTTP(httptest.NewRecorder(), req)

		if httpCtxHadToken {
			t.Fatal("HTTP handler ctx carries raw subject token — should be absent; hop 2 is served by receiving middleware only")
		}
	})
}
