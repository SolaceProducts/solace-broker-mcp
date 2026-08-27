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
	"sync/atomic"
	"testing"
	"time"

	internalauth "github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache"
	"github.com/SolaceProducts/solace-broker-mcp/internal/oauth/cache/cachetest"
	sempauth "github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
)

// AddAuth obtains a broker token and puts it on an outgoing SEMP request,
// working through Exchanger and TokenCache. The tests here run all three
// against a fake IdP, which is the only way to check:
//
//  1. No secret is written to a log. AddAuth's unit tests use a fake exchanger
//     returning a placeholder, so there is no real secret there to leak.
//
//  2. AddAuth logs one start line and one finish line on every path, so a
//     reader can tell "never got a token" from "still in progress".
//
//  3. The finish line names its cause, including when the caller cancels —
//     Exchange returns a bare context error there rather than an ExchangeError.
//
//  4. The attempt count on the IdP line reflects real retries, which needs the
//     retrying client production wires rather than a plain http.Client.
//
// Checks 1 and 2 deliberately ignore the wording of the lines in between, so
// rewording one does not break them. Checks 3 and 4 do name a message, because
// they assert what that specific line carries.

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

	tokenCache := cachetest.WithConfig(t, cache.CacheConfig{
		MaxSize:   64,
		ClockSkew: clockSkew,
		MaxTTL:    time.Hour,
	})

	// The retrying client, because that is what main.go passes. A plain
	// http.Client would work for everything else here, but it does not carry
	// the transport wrapper that counts HTTP attempts, so the attempt count on
	// the "issued" log line would read 0 for a scenario that cannot occur in
	// production. Short waits keep the retry cases fast.
	httpClient, err := idpclient.NewRetryingHTTPClient(idpclient.RetryOptions{
		MaxRetries:   3,
		RetryWaitMin: 10 * time.Millisecond,
		RetryWaitMax: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	exchanger, err := tokenexchange.New(tokenexchange.Params{
		TokenURL:         idpURL,
		ClientID:         "mcp-server",
		ClientAuthMethod: tokenexchange.ClientSecretBasic,
		ClientSecret:     traceClientSecret,
		GrantType:        tokenexchange.GrantTypeTokenExchange,
		AudienceParam:    tokenexchange.AudienceParamAudience,
		HTTPClient:       httpClient,
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

// Test_BrokerTokenTrace_AttemptCount checks the attempt count on the "issued"
// line, which is the only way a reader can tell a slow exchange caused by
// retries apart from one slow call.
//
// The count comes from a transport wrapper that the retrying client installs,
// so it is only correct when the Exchanger holds that client — which is what
// main.go passes and what newTraceAuthenticator above uses. A plain
// http.Client silently reports 0.
func Test_BrokerTokenTrace_AttemptCount(t *testing.T) {
	cases := []struct {
		name string
		// fail503 is how many times the IdP answers 503 before succeeding.
		fail503      int
		wantAttempts int
	}{
		{name: "succeeds first time", fail503: 0, wantAttempts: 1},
		{name: "succeeds on the third attempt", fail503: 2, wantAttempts: 3},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int32
			idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if int(seen.Add(1)) <= tc.fail503 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				idpIssues(3600)(w, nil)
			}))
			t.Cleanup(idp.Close)

			auth := newTraceAuthenticator(t, idp.URL, 10*time.Second)
			ctx := traceCtxWithSubjectToken(t)

			recs := captureTrace(t, func() {
				if err := auth.AddAuth(ctx, httptest.NewRequestWithContext(ctx, http.MethodGet, "/SEMP/v2/monitor", nil)); err != nil {
					t.Fatalf("AddAuth: %v", err)
				}
			})

			got, ok := attemptsOnIssuedLine(recs)
			if !ok {
				t.Fatalf(`no "identity provider issued broker token" line carrying an attempt count.

%s`, renderTrace(recs))
			}
			if got != tc.wantAttempts {
				t.Errorf(`the IdP was called %d time(s) but the log line says %d.

A wrong count here misreads a retried exchange as a single slow call. If the
count is 0, the Exchanger was built with an http.Client that does not count
attempts — see newTraceAuthenticator.

%s`, tc.wantAttempts, got, renderTrace(recs))
			}
		})
	}
}

// attemptsOnIssuedLine pulls the attempt count off the IdP success line.
func attemptsOnIssuedLine(recs []traceRecord) (int, bool) {
	for _, r := range recs {
		if r.Msg != "identity provider issued broker token" {
			continue
		}
		var m struct {
			Attempts *int `json:"attempts"`
		}
		if err := json.Unmarshal([]byte(r.Raw), &m); err != nil || m.Attempts == nil {
			return 0, false
		}
		return *m.Attempts, true
	}
	return 0, false
}

// A client that disconnects mid-request is ordinary traffic, and the trace it
// leaves behind should say so. Exchange returns the bare context error rather
// than an ExchangeError on its cancellation paths, so without handling that
// case the closing line reports reason=unknown for a cause that is known.
func Test_BrokerTokenTrace_CancelledCallerNamesTheCause(t *testing.T) {
	idp := httptest.NewServer(idpIssues(3600))
	t.Cleanup(idp.Close)

	auth := newTraceAuthenticator(t, idp.URL, 10*time.Second)

	ctx, cancel := context.WithCancel(traceCtxWithSubjectToken(t))
	cancel()

	var err error
	recs := captureTrace(t, func() {
		err = auth.AddAuth(ctx, httptest.NewRequestWithContext(ctx, http.MethodGet, "/SEMP/v2/monitor", nil))
	})

	if err == nil {
		t.Fatal("AddAuth: want an error for a cancelled caller, got nil")
	}

	var reason string
	var found bool
	for _, r := range recs {
		if r.Msg != msgUnavail {
			continue
		}
		found = true
		var m map[string]any
		if jsonErr := json.Unmarshal([]byte(r.Raw), &m); jsonErr != nil {
			t.Fatalf("unmarshalling %q: %v", r.Raw, jsonErr)
		}
		reason, _ = m["reason"].(string)
	}

	if !found {
		t.Fatalf("no %q line was logged for a cancelled caller.\n\n%s", msgUnavail, renderTrace(recs))
	}
	if reason == "unknown" {
		t.Errorf(`the trace closed with reason=unknown for a cancelled caller.

Exchange returned a context error, so the cause is known. Reporting it as
unknown loses the one thing this line exists to say.

%s`, renderTrace(recs))
	}
}

// cancelDuringGet wraps a TokenCache and cancels the caller partway through a
// successful lookup, which is the window Exchange's post-hit re-check exists to
// catch: TokenCache.Get takes a context but does not promise to honour it, so a
// caller can go away between the entry guard and the return.
type cancelDuringGet struct {
	inner  cache.TokenCache
	cancel context.CancelFunc
}

func (c *cancelDuringGet) Get(ctx context.Context, key string) (cache.GetResult, error) {
	res, err := c.inner.Get(ctx, key)
	if err == nil && res.Status == cache.GetHit {
		c.cancel()
	}
	return res, err
}

func (c *cancelDuringGet) Put(ctx context.Context, key string, entry cache.CachedCredential) (cache.PutResult, error) {
	return c.inner.Put(ctx, key, entry)
}

func (c *cancelDuringGet) Delete(ctx context.Context, key string) (cache.DeleteResult, error) {
	return c.inner.Delete(ctx, key)
}

func (c *cancelDuringGet) Close() error { return c.inner.Close() }

// A caller cancelled during the cache lookup gets an error rather than the token
// the lookup found. It must not be told its cached token was used.
func Test_BrokerTokenTrace_CancelledDuringCacheHitClaimsNothing(t *testing.T) {
	idp := httptest.NewServer(idpIssues(3600))
	t.Cleanup(idp.Close)

	inner := cachetest.WithConfig(t, cache.CacheConfig{
		MaxSize: 8, ClockSkew: 10 * time.Second, MaxTTL: time.Hour,
	})

	ctx, cancel := context.WithCancel(traceCtxWithSubjectToken(t))
	wrapped := &cancelDuringGet{inner: inner, cancel: cancel}

	httpClient, err := idpclient.NewRetryingHTTPClient(idpclient.RetryOptions{
		MaxRetries: 1, RetryWaitMin: 5 * time.Millisecond, RetryWaitMax: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	exchanger, err := tokenexchange.New(tokenexchange.Params{
		TokenURL:         idp.URL,
		ClientID:         "mcp-server",
		ClientAuthMethod: tokenexchange.ClientSecretBasic,
		ClientSecret:     traceClientSecret,
		GrantType:        tokenexchange.GrantTypeTokenExchange,
		AudienceParam:    tokenexchange.AudienceParamAudience,
		HTTPClient:       httpClient,
		Cache:            wrapped,
	})
	if err != nil {
		t.Fatalf("tokenexchange.New: %v", err)
	}
	auth := sempauth.NewOAuthAuthenticator(exchanger, traceAudience, traceBrokerAlias)

	// Prime the cache on an uncancelled context, so the captured run below finds
	// a hit — and the wrapper cancels it partway through that lookup.
	warm := traceCtxWithSubjectToken(t)
	if err := auth.AddAuth(warm, httptest.NewRequestWithContext(warm, http.MethodGet, "/SEMP/v2/monitor", nil)); err != nil {
		t.Fatalf("priming acquisition failed: %v", err)
	}

	var addAuthErr error
	recs := captureTrace(t, func() {
		addAuthErr = auth.AddAuth(ctx, httptest.NewRequestWithContext(ctx, http.MethodGet, "/SEMP/v2/monitor", nil))
	})

	if addAuthErr == nil {
		t.Fatal("AddAuth: want an error for a caller cancelled during the lookup, got nil")
	}

	for _, r := range recs {
		if r.Msg == "using cached broker token" {
			t.Errorf(`a cancelled caller was told its cached token was used, then handed an error.

The cache-hit line must sit below the context re-check in Exchange, so it only
fires on the path that actually returns a token.

%s`, renderTrace(recs))
		}
	}
}
