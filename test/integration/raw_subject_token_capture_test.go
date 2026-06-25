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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// TestRawSubjectTokenCapture_AfterSDKValidation pins the chain-order
// invariant for SOL-150797: when auth.InjectRawSubjectToken is wrapped
// inside the SDK's RequireBearerToken (the production wiring established
// in internal/auth/middleware.go), the raw bearer token reaches the
// request context downstream of validation, and only when validation
// succeeded.
//
// Why this lives in test/integration (not internal/auth):
// the assertion is a contract between two components — the MCP SDK's
// RequireBearerToken middleware and our auth.InjectRawSubjectToken. It
// cannot be expressed using only one component's public surface. Per
// test/integration/README.md, that is precisely the criterion for the
// integration tier.
//
// Why a hand-written verifier (not createOIDCTokenVerifier or
// createStaticTokenVerifier): we want to control the SDK's accept /
// reject decision deterministically without standing up an IdP or
// hard-coding a static dev token. The middleware's behaviour is
// mode-agnostic, so the choice of verifier does not change what is
// being tested. Validator correctness for static and OIDC modes is
// covered by internal/auth/middleware_test.go.
//
// What the rejection subtest does and does NOT assert:
// the load-bearing assertion is "our middleware was not invoked"
// (probeInvoked == false). The HTTP status code is a sanity guard
// only — we accept any 4xx/5xx and do not pin a specific value.
// The SDK returns 401 when the verifier wraps ErrInvalidToken and
// 500 otherwise; both are correct from the perspective of this
// test, which is about chain-order behaviour, not about which SDK
// branch the rejection took. Pinning a specific status code would
// be false precision — it would couple the test to an SDK internal
// while adding no signal about our middleware.
func TestRawSubjectTokenCapture_AfterSDKValidation(t *testing.T) {
	t.Parallel()
	const goodToken = "eyJhbGciOiJSUzI1NiJ9.good.sig"

	t.Run("valid token survives the chain and reaches downstream on ctx", func(t *testing.T) {
		t.Parallel()
		var verifierSawToken string
		verifier := func(_ context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
			verifierSawToken = token
			return &sdkauth.TokenInfo{
				UserID:     "test-user",
				Expiration: time.Now().Add(time.Hour),
			}, nil
		}

		var downstreamSawToken string
		var downstreamOK bool
		downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			downstreamSawToken, downstreamOK = auth.RawSubjectTokenFromContext(r.Context())
		})

		// Wiring mirrors internal/auth/middleware.go:
		//   chain = RequireBearerToken( InjectRawSubjectToken(next) )
		// Our middleware runs only after the SDK calls handler.ServeHTTP.
		chain := sdkauth.RequireBearerToken(verifier, nil)(auth.InjectRawSubjectToken(downstream))

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+goodToken)
		chain.ServeHTTP(httptest.NewRecorder(), req)

		if !downstreamOK {
			t.Fatal("downstream did not observe a raw token on ctx — InjectRawSubjectToken did not run, or it failed to stash the token")
		}
		if downstreamSawToken != verifierSawToken {
			t.Errorf("token bytes drifted between layers: SDK verifier saw %q, downstream saw %q (must be identical)", verifierSawToken, downstreamSawToken)
		}
		if downstreamSawToken != goodToken {
			t.Errorf("downstream token %q != wire token %q", downstreamSawToken, goodToken)
		}
		t.Logf("end-to-end ok: wire→verifier→ctx, %d bytes preserved exactly", len(goodToken))
	})

	t.Run("rejected token short-circuits at the SDK and never reaches our middleware", func(t *testing.T) {
		t.Parallel()
		// Any error from the verifier causes the SDK to reject the
		// request. The specific status code depends on SDK internals
		// (401 if errors.Is(err, ErrInvalidToken), 500 otherwise) and is
		// not the property under test. The property is: when the upstream
		// validator rejects, our middleware does not run.
		//
		// Earlier iterations of this test wrapped the sentinel with %w
		// and asserted status 401 exactly. That was reverted: pinning a
		// specific SDK branch is false precision when our assertion
		// is about middleware behaviour, not SDK behaviour. See the
		// function-level doc above for the full reasoning.
		verifier := func(_ context.Context, _ string, _ *http.Request) (*sdkauth.TokenInfo, error) {
			return nil, errors.New("token rejected for test")
		}

		// Probe handler under InjectRawSubjectToken. If control flow ever
		// reaches it, the chain-order invariant is broken: either the SDK
		// is not short-circuiting on rejection, or InjectRawSubjectToken
		// is wrapped on the outside (the rejected design).
		var probeInvoked bool
		probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probeInvoked = true
		})
		chain := sdkauth.RequireBearerToken(verifier, nil)(auth.InjectRawSubjectToken(probe))

		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer bogus")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)

		// Sanity guard: the SDK must respond with some 4xx/5xx. The
		// load-bearing assertion is probeInvoked == false below.
		if rec.Code < 400 {
			t.Errorf("expected SDK to reject the request with a 4xx/5xx status, got %d", rec.Code)
		}
		if probeInvoked {
			t.Fatal("downstream handler ran despite SDK rejection — chain-order invariant is broken; InjectRawSubjectToken is positioned outside the SDK")
		}
		t.Logf("rejection short-circuit ok: SDK returned %d and probe was never invoked", rec.Code)
	})
}

// TestRawSubjectTokenCapture_WiredByNewAuthMiddleware pins the production
// wiring: calling auth.NewAuthMiddleware on a real *config.ServerConfig
// installs auth.InjectRawSubjectToken into the chain. Without this test,
// a refactor that drops `InjectRawSubjectToken(next)` from
// internal/auth/middleware.go would not fail any existing test — the
// chain-order test above builds the chain by hand and so does not
// exercise NewAuthMiddleware.
//
// Static mode is used because it is the smallest production-supported
// MCPClientAuth.Mode that activates the auth chain (the disabled mode
// returns next directly without wrapping). The dev token doubles as a
// readable test fixture; production OIDC mode would require an IdP and
// would not change what is asserted here.
func TestRawSubjectTokenCapture_WiredByNewAuthMiddleware(t *testing.T) {
	t.Parallel()
	const devToken = "static-mode-dev-token-fixture"

	cfg := &config.ServerConfig{
		MCPClientAuth: config.MCPClientAuthConfig{
			Mode:     config.AuthModeStatic,
			DevToken: devToken,
		},
	}

	var downstreamSawToken string
	var downstreamOK bool
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamSawToken, downstreamOK = auth.RawSubjectTokenFromContext(r.Context())
	})

	handler, err := auth.NewAuthMiddleware(cfg, nil, downstream)
	if err != nil {
		t.Fatalf("NewAuthMiddleware returned error: %v", err)
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+devToken)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !downstreamOK {
		t.Fatal("downstream did not observe a raw token on ctx — NewAuthMiddleware is not wiring InjectRawSubjectToken into the chain")
	}
	if downstreamSawToken != devToken {
		t.Errorf("downstream token %q != dev token %q", downstreamSawToken, devToken)
	}
	t.Logf("production wiring ok: NewAuthMiddleware in static mode passes the bearer token through to ctx")
}
