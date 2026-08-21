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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalauth "github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache"
	sempauth "github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// AddAuth obtains a broker token and puts it on an outgoing SEMP request. It
// does that through Exchanger and TokenCache, and this test runs all three
// together against a fake IdP.
//
// It checks two things, and deliberately nothing else. It does not check the
// wording of the log lines in between, so rewording one of those does not
// break this test.
//
//  1. None of the secrets are written to a log. AddAuth handles the inbound
//     token from the request and the broker token it gets back; Exchanger also
//     holds the IdP client secret. AddAuth's own unit tests use a fake
//     exchanger that returns a placeholder token, so there is no real secret
//     there to leak. The values here are real, so a log line that prints one
//     fails the build.
//
//  2. AddAuth logs one line when it starts and one line when it finishes, on
//     every path. That is what lets someone reading the logs tell "this request
//     never got a token" apart from "this request is still in progress". A unit
//     test cannot check it, because the fake exchanger skips everything AddAuth
//     does in between.

const (
	traceBrokerAlias  = "prod-us"
	traceAudience     = "https://broker.example.com"
	traceSubjectTok   = "subject-token-do-not-log-3f9a"
	traceBrokerTok    = "broker-token-do-not-log-7c21"
	traceClientSecret = "client-secret-do-not-log-b48e"
)

// The start and finish lines AddAuth logs. These three are the only log
// messages this test looks at.
const (
	msgNeeded   = "broker token needed"
	msgAttached = "broker token attached to request"
	msgUnavail  = "broker token unavailable"
)

type traceRecord struct {
	Level string
	Msg   string
	Raw   string
}

// captureTrace collects the log lines written while fn runs. It swaps the
// default logger because all three components log through the slog
// package-level functions, the same way they do in production.
func captureTrace(t *testing.T, fn func()) []traceRecord {
	t.Helper()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	fn()

	// The cache Put runs inside the singleflight goroutine and completes
	// independently of the caller, so its log line can land after AddAuth
	// returns. Settle before reading the buffer.
	time.Sleep(50 * time.Millisecond)

	var recs []traceRecord
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("captured a non-JSON log line %q: %v", line, err)
		}
		lvl, _ := m["level"].(string)
		msg, _ := m["msg"].(string)
		recs = append(recs, traceRecord{Level: lvl, Msg: msg, Raw: line})
	}
	return recs
}

// newTraceAuthenticator wires the real three components against an httptest
// IdP. clockSkew is a parameter because the refused-cache-write path is only
// reachable when a token's remaining lifetime falls under it.
func newTraceAuthenticator(t *testing.T, idpURL string, clockSkew time.Duration) *sempauth.OAuthAuthenticator {
	t.Helper()

	tokenCache, err := cache.NewTokenCache(cache.CacheConfig{
		MaxSize:   64,
		ClockSkew: clockSkew,
		MaxTTL:    time.Hour,
	})
	if err != nil {
		t.Fatalf("NewTokenCache: %v", err)
	}
	t.Cleanup(func() { _ = tokenCache.Close() })

	exchanger, err := tokenexchange.New(tokenexchange.Params{
		TokenURL:         idpURL,
		ClientID:         "mcp-server",
		ClientAuthMethod: tokenexchange.ClientSecretBasic,
		ClientSecret:     traceClientSecret,
		GrantType:        tokenexchange.GrantTypeTokenExchange,
		AudienceParam:    tokenexchange.AudienceParamAudience,
		HTTPClient:       &http.Client{Timeout: 5 * time.Second},
		Cache:            tokenCache,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}

	return sempauth.NewOAuthAuthenticator(exchanger, traceAudience, traceBrokerAlias)
}

// traceCtxWithSubjectToken runs the real Hop 1 middleware, so the subject
// token reaches ctx by production's path rather than via a context key this
// package cannot reach.
func traceCtxWithSubjectToken(t *testing.T) context.Context {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+traceSubjectTok)

	var captured context.Context
	internalauth.InjectRawSubjectToken(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		captured = r.Context()
	})).ServeHTTP(httptest.NewRecorder(), req)

	if captured == nil {
		t.Fatal("InjectRawSubjectToken did not invoke the next handler")
	}
	return captured
}

func idpIssues(expiresIn int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w,
			`{"access_token":%q,"token_type":"Bearer","issued_token_type":%q,"expires_in":%d}`,
			traceBrokerTok, tokenexchange.URNTokenTypeAccessToken, expiresIn)
	}
}

func Test_BrokerTokenTrace(t *testing.T) {
	cases := []struct {
		name             string
		idp              http.HandlerFunc
		clockSkew        time.Duration // zero means a sane default
		warm             bool          // acquire once before capturing, so the captured run hits the cache
		omitSubjectToken bool
		wantErr          bool
	}{
		{
			name: "cold: IdP issues a token and it is cached",
			idp:  idpIssues(3600),
		},
		{
			name: "warm: served from cache, no IdP call",
			idp:  idpIssues(3600),
			warm: true,
		},
		{
			// A one-hour skew leaves a five-second token with a negative
			// effective lifetime, so the cache refuses it. The acquisition
			// still succeeds — the token is used, just not stored.
			name:      "cold: token too short-lived to cache, still attached",
			idp:       idpIssues(5),
			clockSkew: time.Hour,
		},
		{
			name:             "failure: no subject token on the request context",
			idp:              idpIssues(3600),
			omitSubjectToken: true,
			wantErr:          true,
		},
		{
			name: "failure: IdP rejects the exchange",
			idp: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"subject token not accepted"}`)
			},
			wantErr: true,
		},
		{
			name: "failure: IdP is unavailable",
			idp: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			wantErr: true,
		},
		{
			name: "failure: IdP returns an unparseable 2xx",
			idp: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				fmt.Fprint(w, "<html>sso portal</html>")
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skew := tc.clockSkew
			if skew == 0 {
				skew = 10 * time.Second
			}

			idp := httptest.NewServer(tc.idp)
			t.Cleanup(idp.Close)

			auth := newTraceAuthenticator(t, idp.URL, skew)

			ctx := context.Background()
			if !tc.omitSubjectToken {
				ctx = traceCtxWithSubjectToken(t)
			}

			if tc.warm {
				if err := auth.AddAuth(ctx, httptest.NewRequestWithContext(ctx, http.MethodGet, "/SEMP/v2/monitor", nil)); err != nil {
					t.Fatalf("priming acquisition failed: %v", err)
				}
				// Let the priming Put land, so the captured run is a real hit.
				time.Sleep(50 * time.Millisecond)
			}

			var addAuthErr error
			recs := captureTrace(t, func() {
				addAuthErr = auth.AddAuth(ctx, httptest.NewRequestWithContext(ctx, http.MethodGet, "/SEMP/v2/monitor", nil))
			})

			if tc.wantErr && addAuthErr == nil {
				t.Fatal("AddAuth: want error, got nil")
			}
			if !tc.wantErr && addAuthErr != nil {
				t.Fatalf("AddAuth: unexpected error: %v", addAuthErr)
			}

			assertSecretsNotLogged(t, recs)
			assertStartAndFinishLogged(t, recs, tc.wantErr)
		})
	}
}

// assertSecretsNotLogged fails if any of the secrets appear in a log line.
func assertSecretsNotLogged(t *testing.T, recs []traceRecord) {
	t.Helper()

	secrets := map[string]string{
		"inbound subject token":  traceSubjectTok,
		"exchanged broker token": traceBrokerTok,
		"IdP client secret":      traceClientSecret,
	}
	for _, r := range recs {
		for name, secret := range secrets {
			if strings.Contains(r.Raw, secret) {
				t.Errorf(`the %s was written to a log line — this is a credential leak.

See docs/internal/secure-logging-rules.md. Fix the log line, not this test.

%s`, name, r.Raw)
			}
		}
	}
}

// assertStartAndFinishLogged checks AddAuth logged one start line and one
// finish line, and that the finish line matches what actually happened. It
// does not look at the lines in between.
func assertStartAndFinishLogged(t *testing.T, recs []traceRecord, wantErr bool) {
	t.Helper()

	if n := countMsg(recs, msgNeeded); n != 1 {
		t.Errorf(`AddAuth logged %d %q lines, want 1.

%s`, n, msgNeeded, renderTrace(recs))
	}

	attached := countMsg(recs, msgAttached)
	unavailable := countMsg(recs, msgUnavail)

	if attached+unavailable != 1 {
		t.Errorf(`AddAuth logged %d finish lines, want 1 (%q or %q).

Every path out of AddAuth must log one. A new early return is the usual cause.

%s`, attached+unavailable, msgAttached, msgUnavail, renderTrace(recs))
		return
	}

	if wantErr && unavailable != 1 {
		t.Errorf(`AddAuth failed but logged %q, so the logs claim the request got a token.

%s`, msgAttached, renderTrace(recs))
	}
	if !wantErr && attached != 1 {
		t.Errorf(`AddAuth succeeded but logged %q, so the logs claim a failure that did not happen.

%s`, msgUnavail, renderTrace(recs))
	}
}

func countMsg(recs []traceRecord, msg string) int {
	n := 0
	for _, r := range recs {
		if r.Msg == msg {
			n++
		}
	}
	return n
}

// renderTrace prints every captured line, so a failure shows what was actually
// logged rather than only what was missing.
func renderTrace(recs []traceRecord) string {
	var b strings.Builder
	b.WriteString("captured trace:\n")
	for _, r := range recs {
		fmt.Fprintf(&b, "  %-5s %s\n", r.Level, r.Msg)
	}
	return b.String()
}
