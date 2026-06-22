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

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// runMiddleware sends a request with the given Authorization header through
// InjectRawSubjectToken and returns whatever RawSubjectTokenFromContext
// observes inside the downstream handler. ok is the second return of the
// accessor; nextInvoked reports whether the wrapped handler actually ran.
func runMiddleware(t *testing.T, authHeader string) (gotToken string, ok bool, nextInvoked bool) {
	t.Helper()
	handler := InjectRawSubjectToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextInvoked = true
		gotToken, ok = RawSubjectTokenFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return gotToken, ok, nextInvoked
}

// TestInjectRawSubjectToken_HeaderShapes pins the parse rules: which
// Authorization header values cause the token to be stashed on ctx, and
// which are silently passed through as no-ops. The middleware never
// rejects on its own — the SDK upstream is the authority.
func TestInjectRawSubjectToken_HeaderShapes(t *testing.T) {
	t.Parallel()
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ1c2VyIn0.signature"

	cases := []struct {
		name        string
		header      string
		wantToken   string
		wantOK      bool
		description string
	}{
		{"missing header", "", "", false, "no Authorization at all"},
		{"valid Bearer", "Bearer " + jwt, jwt, true, "canonical shape"},
		{"lowercase bearer", "bearer " + jwt, jwt, true, "scheme matched case-insensitively"},
		{"uppercase BEARER", "BEARER " + jwt, jwt, true, "scheme matched case-insensitively"},
		{"mixed case", "BeArEr " + jwt, jwt, true, "scheme matched case-insensitively"},
		{"Bearer alone", "Bearer", "", false, "one field after split"},
		{"Bearer trailing space", "Bearer ", "", false, "trailing whitespace collapses to one field"},
		{"Basic scheme", "Basic dXNlcjpwYXNz", "", false, "wrong scheme"},
		{"DPoP scheme", "DPoP " + jwt, "", false, "wrong scheme"},
		{"token only no scheme", jwt, "", false, "one field"},
		{"three fields", "Bearer " + jwt + " extra", "", false, "extra token"},
		{"extra spaces", "Bearer   " + jwt, jwt, true, "strings.Fields collapses whitespace"},
		{"tab separator", "Bearer\t" + jwt, jwt, true, "strings.Fields handles tabs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotToken, ok, nextInvoked := runMiddleware(t, tc.header)
			if !nextInvoked {
				t.Fatalf("next handler was not invoked; middleware must always pass through (%s)", tc.description)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (%s)", ok, tc.wantOK, tc.description)
			}
			if gotToken != tc.wantToken {
				t.Errorf("token = %q, want %q (%s)", gotToken, tc.wantToken, tc.description)
			}
		})
	}
}

// TestInjectRawSubjectToken_ConcurrentRequests is the security-critical
// test: prove that two concurrent requests with different tokens cannot
// see each other's token. The design has no shared mutable state, so a
// race is structurally impossible — this test pins that property so any
// future change that introduces shared state fails CI.
//
// Run under -race for the full guarantee.
func TestInjectRawSubjectToken_ConcurrentRequests(t *testing.T) {
	t.Parallel()

	const goroutines = 64
	const iterationsPerGoroutine = 1000

	handler := InjectRawSubjectToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := r.Header.Get("X-Expected-Token")
		got, ok := RawSubjectTokenFromContext(r.Context())
		if !ok || got != expected {
			// Write a marker so the spawning goroutine can detect the
			// cross-contamination. We do NOT log the actual token
			// values — only the fact that they did not match.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	var mismatches atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := 0; g < goroutines; g++ {
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < iterationsPerGoroutine; i++ {
				// Build a unique token per request so any cross-talk
				// would be immediately observable.
				tok := "token-w" + itoa(workerID) + "-i" + itoa(i)
				req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
				req.Header.Set("Authorization", "Bearer "+tok)
				req.Header.Set("X-Expected-Token", tok)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					mismatches.Add(1)
				}
			}
		}(g)
	}

	wg.Wait()

	if n := mismatches.Load(); n > 0 {
		t.Fatalf("cross-request token contamination detected: %d mismatches out of %d requests", n, goroutines*iterationsPerGoroutine)
	}
}

// TestInjectRawSubjectToken_RequestIsolation pins two no-mutation
// guarantees: the middleware must not alter the original request's
// Authorization header, and the parent context (the one on the original
// r before ServeHTTP was called) must not carry the stashed token —
// only the derived context handed to the next handler does.
func TestInjectRawSubjectToken_RequestIsolation(t *testing.T) {
	t.Parallel()
	const jwt = "eyJhbGciOiJSUzI1NiJ9.payload.sig"

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+jwt)
	parentCtx := req.Context()

	var nextSawHeader string
	handler := InjectRawSubjectToken(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextSawHeader = r.Header.Get("Authorization")
	}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// Header on the original request must be untouched.
	if got := req.Header.Get("Authorization"); got != "Bearer "+jwt {
		t.Errorf("original Authorization header was mutated: got %q", got)
	}
	// Header must also be visible to the next handler (i.e. the SDK
	// downstream session-affinity logic that may read it).
	if nextSawHeader != "Bearer "+jwt {
		t.Errorf("next handler did not see Authorization header: got %q", nextSawHeader)
	}
	// The parent context (the one the request had before InjectRawSubjectToken
	// derived its own) must NOT carry the stashed token — only the
	// derived ctx handed to next does.
	if _, ok := RawSubjectTokenFromContext(parentCtx); ok {
		t.Error("parent context was polluted with rawSubjectTokenKey; middleware must derive a new ctx instead")
	}
}

// TestInjectRawSubjectToken_PositionAfterSDK pins the wiring contract:
// when chained behind the SDK's RequireBearerToken (as NewAuthMiddleware
// does), InjectRawSubjectToken runs only after the SDK has approved the
// request. A rejected request must never reach our middleware, and the
// raw token observed by downstream must equal the token the SDK passed
// to the verifier.
func TestInjectRawSubjectToken_PositionAfterSDK(t *testing.T) {
	t.Parallel()
	const goodToken = "eyJhbGciOiJSUzI1NiJ9.good.sig"

	t.Run("valid token reaches our middleware and downstream sees the raw token", func(t *testing.T) {
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
			downstreamSawToken, downstreamOK = RawSubjectTokenFromContext(r.Context())
		})
		chain := sdkauth.RequireBearerToken(verifier, nil)(InjectRawSubjectToken(downstream))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+goodToken)
		chain.ServeHTTP(httptest.NewRecorder(), req)

		if !downstreamOK {
			t.Fatal("downstream did not see a raw token; InjectRawSubjectToken did not stash it")
		}
		if downstreamSawToken != verifierSawToken {
			t.Errorf("downstream token %q != verifier token %q (must be the same bytes)", downstreamSawToken, verifierSawToken)
		}
		if downstreamSawToken != goodToken {
			t.Errorf("downstream token %q != expected %q", downstreamSawToken, goodToken)
		}
	})

	t.Run("invalid token is rejected by SDK; our middleware is never invoked", func(t *testing.T) {
		t.Parallel()
		verifier := func(_ context.Context, _ string, _ *http.Request) (*sdkauth.TokenInfo, error) {
			return nil, errors.New("token rejected for test: " + sdkauth.ErrInvalidToken.Error())
		}

		var ourMiddlewareInvoked bool
		ourMiddleware := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ourMiddlewareInvoked = true
		})
		// Wrap our handler with InjectRawSubjectToken; the SDK should
		// short-circuit before InjectRawSubjectToken's inner func runs.
		chain := sdkauth.RequireBearerToken(verifier, nil)(InjectRawSubjectToken(ourMiddleware))

		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer bogus")
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusInternalServerError {
			// We tolerate either 401 (errors.Is(err, ErrInvalidToken)) or
			// 500 (any other error), depending on how the SDK unwraps.
			t.Errorf("expected SDK to reject the request; got status %d", rec.Code)
		}
		if ourMiddlewareInvoked {
			t.Fatal("downstream handler was invoked despite SDK rejection; chain order is broken")
		}
	})
}

// TestRawSubjectTokenFromContext_AccessorContract covers the accessor's
// edge cases: nil value, wrong type, empty string. The middleware never
// writes any of these in production, but the accessor folds the checks
// in so callers branch once.
func TestRawSubjectTokenFromContext_AccessorContract(t *testing.T) {
	t.Parallel()

	t.Run("background context has no token", func(t *testing.T) {
		t.Parallel()
		s, ok := RawSubjectTokenFromContext(context.Background())
		if ok || s != "" {
			t.Errorf("got (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("context with non-string value returns not-ok", func(t *testing.T) {
		t.Parallel()
		// Only this package can construct rawSubjectTokenKey{}, so this
		// test is defensive only — guards against a future bug inside
		// this package that stores the wrong type.
		ctx := context.WithValue(context.Background(), rawSubjectTokenKey{}, 42)
		s, ok := RawSubjectTokenFromContext(ctx)
		if ok || s != "" {
			t.Errorf("got (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("context with empty string returns not-ok", func(t *testing.T) {
		t.Parallel()
		ctx := context.WithValue(context.Background(), rawSubjectTokenKey{}, "")
		s, ok := RawSubjectTokenFromContext(ctx)
		if ok || s != "" {
			t.Errorf("got (%q, %v), want (\"\", false)", s, ok)
		}
	})

	t.Run("exact round-trip preserves token bytes", func(t *testing.T) {
		t.Parallel()
		const want = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.payload.signature_with-special.chars"
		ctx := context.WithValue(context.Background(), rawSubjectTokenKey{}, want)
		got, ok := RawSubjectTokenFromContext(ctx)
		if !ok {
			t.Fatal("ok = false, want true")
		}
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// itoa avoids importing strconv in the concurrency test. Small, single-purpose.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
