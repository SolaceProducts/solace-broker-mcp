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

// The broker_auth_retry audit record checkRetry emits on a 401 recovery
// attempt (SOL-152097). Each test drives the real 401 handling path through
// Sender.Do with an httptest server, so the outcome asserted is what the
// retry policy actually observed, not a value handed to a helper directly.
//
// The outcome is decided once, in Sender.Do, after the whole retry chain has
// concluded — not from inside checkRetry, which can run many more times
// after a 401 recovers (a transient 503/500 retry, a second 401) and, on some
// paths, never runs again at all. TestBrokerAuthRetry_401Then503Then401 and
// TestBrokerAuthRetry_401Then503ThenSuccess pin the first hazard: a decision
// made mid-chain either double-emits (both an outcome=success from the first
// non-401 response and an outcome=error from a later persisted 401) or emits
// the wrong record for the same request. TestBrokerAuthRetry_TransportErrorAfterRetry
// pins the second: a transport error on the retried request never re-enters
// checkRetry, so a mid-chain decision would silently drop the record.

package resilience

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
)

// captureAuditRecords runs fn with the default logger swapped for a JSON
// handler and returns every decoded audit record (event="audit") it wrote.
// Mirrors internal/tools' captureAudit/ofType helpers, duplicated here rather
// than shared across packages — the two are as small as the import they'd
// otherwise need.
func captureAuditRecords(t *testing.T, fn func()) []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	fn()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		if rec["event"] == audit.EventValue {
			out = append(out, rec)
		}
	}
	return out
}

// ofRetryType returns the captured records matching audit_event_type typ.
func ofRetryType(records []map[string]any, typ audit.EventType) []map[string]any {
	var out []map[string]any
	for _, rec := range records {
		if rec["audit_event_type"] == string(typ) {
			out = append(out, rec)
		}
	}
	return out
}

// newAuthRetryTestSender builds a Sender against handler with the given
// auth mode ("basic" or "bearer") and audit-log setting, alias "dev-broker".
// retries bounds RetryMax; the 401 cap (auth401Retried) is independent of it
// and is what these tests actually exercise.
func newAuthRetryTestSender(t *testing.T, handler http.HandlerFunc, authMode string, retries int, auditEnabled bool) (*Sender, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	httpClient := server.Client()

	var authn auth.Authenticator
	switch authMode {
	case "bearer":
		authn = auth.NewBearerAuthenticator("static-token")
	case "basic":
		jar := mustNewSafeCookieJar(t)
		httpClient.Jar = jar
		authn = auth.NewBasicAuthenticator("admin", "secret", jar)
	default:
		t.Fatalf("unknown authMode %q", authMode)
	}

	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RequestTimeoutDuration: 30 * time.Second,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	d := New(httpClient, sempCfg, authn, server.URL, NewSemaphore(10), NewRateLimiter(0),
		WithAuditLog(auditEnabled), WithBrokerAlias("dev-broker"))
	return d, server
}

// TestBrokerAuthRetry_Success pins the outcome=success case: a 401 recovery
// attempt (basic auth clears the stale cookie and retries) is followed by a
// non-401 response, so the record reports the recovery worked.
func TestBrokerAuthRetry_Success(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "stale"})
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		jsonOK(w)
	}, "basic", 3, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	retries := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retries) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record, got %d:\n%v", len(retries), records)
	}
	rec := retries[0]
	if got := rec["outcome"]; got != string(audit.OutcomeSuccess) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeSuccess)
	}
	if got := rec["broker"]; got != "dev-broker" {
		t.Errorf("broker = %v, want %q", got, "dev-broker")
	}
	for _, forbidden := range []string{"reason", "error_type"} {
		if _, present := rec[forbidden]; present {
			t.Errorf("broker_auth_retry record carries %s = %v, want absent", forbidden, rec[forbidden])
		}
	}
}

// TestBrokerAuthRetry_ErrorOnPersistedFailure pins one outcome=error case: the
// authenticator retries once (clears cookies), but the broker still returns
// 401 — the cap is reached and the record reports the recovery did not work.
func TestBrokerAuthRetry_ErrorOnPersistedFailure(t *testing.T) {
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}, "basic", 10, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	retries := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retries) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record, got %d:\n%v", len(retries), records)
	}
	if got := retries[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
}

// TestBrokerAuthRetry_ErrorWhenAuthenticatorCannotRecover pins the other
// outcome=error case: a bearer authenticator declines to retry at all
// (HandleAuthFailure returns Retry: false), so the record reports the
// failure immediately rather than waiting for a retry that never happens.
func TestBrokerAuthRetry_ErrorWhenAuthenticatorCannotRecover(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}, "bearer", 10, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	if got := requestCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (bearer never retries a 401), got %d", got)
	}
	retries := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retries) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record, got %d:\n%v", len(retries), records)
	}
	if got := retries[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
}

// TestBrokerAuthRetry_AuditDisabled_emitsNoRecord pins the flag-off contract:
// checkRetry's 401 handling and its request counts are unaffected, and no
// broker_auth_retry record is produced.
func TestBrokerAuthRetry_AuditDisabled_emitsNoRecord(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		jsonOK(w)
	}, "basic", 3, false)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	if got := requestCount.Load(); got != 2 {
		t.Fatalf("expected 2 requests (original + 1 retry) regardless of the audit flag, got %d", got)
	}
	if got := len(ofRetryType(records, audit.EventBrokerAuthRetry)); got != 0 {
		t.Errorf("emitted %d broker_auth_retry record(s) with the audit log off, want 0:\n%v", got, records)
	}
}

// TestBrokerAuthRetry_NotEmittedWithoutA401 pins scope: a call with no 401 at
// all produces no broker_auth_retry record.
func TestBrokerAuthRetry_NotEmittedWithoutA401(t *testing.T) {
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w)
	}, "basic", 3, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	if got := len(ofRetryType(records, audit.EventBrokerAuthRetry)); got != 0 {
		t.Errorf("emitted %d broker_auth_retry record(s) for a call with no 401, want 0:\n%v", got, records)
	}
}

// TestBrokerAuthRetry_401Then503Then401 is the regression pinned by the
// review: 401 (recovery attempted) → 503 (transient retry) → 401 again
// (persisted). A decision made from inside checkRetry, at the point the
// first non-401-looking retry fires, has no way to see this second 401
// coming and emits outcome=success there, then outcome=error again when the
// persisted branch fires — two contradictory records for one request.
// Deciding once, from Do's final result (401, so error), must produce
// exactly one.
func TestBrokerAuthRetry_401Then503Then401(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		switch requestCount.Add(1) {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}, "basic", 10, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	if got := requestCount.Load(); got != 3 {
		t.Fatalf("expected 3 requests (401, retry->503, retry->401), got %d", got)
	}
	retries := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retries) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record for a 401->503->401 chain, got %d:\n%v", len(retries), records)
	}
	if got := retries[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q — the chain ended in a persisted 401", got, audit.OutcomeError)
	}
}

// TestBrokerAuthRetry_401Then503ThenSuccess is the success-side twin: the
// transient retry that follows the 401 recovery eventually succeeds. Still
// exactly one record, now outcome=success, because the chain's final
// response is not a 401.
func TestBrokerAuthRetry_401Then503ThenSuccess(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		switch requestCount.Add(1) {
		case 1:
			w.WriteHeader(http.StatusUnauthorized)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			jsonOK(w)
		}
	}, "basic", 10, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if err != nil {
			t.Fatalf("Do() error: %v", err)
		}
		resp.Body.Close()
	})

	if got := requestCount.Load(); got != 3 {
		t.Fatalf("expected 3 requests (401, retry->503, retry->200), got %d", got)
	}
	retries := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retries) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record for a 401->503->200 chain, got %d:\n%v", len(retries), records)
	}
	if got := retries[0]["outcome"]; got != string(audit.OutcomeSuccess) {
		t.Errorf("outcome = %v, want %q — the chain ended in a non-401 response", got, audit.OutcomeSuccess)
	}
}

// TestBrokerAuthRetry_401Then503Exhausted pins the regression a follow-up
// review found: a 401 that recovers into a broker overload (503) which then
// exhausts its own maxTransientRetries cap routes through errorHandler, which
// returns a nil *http.Response on every populated path (see errorHandler).
// Deciding the outcome from Do's own final resp/err would read that nil as
// "never recovered" and misreport outcome=error — the exact chain on which
// the pre-fix code and the doc's own contract ("outcome answers whether the
// credential problem got resolved, not whether the call succeeded")
// disagreed. The credential problem was resolved by the second request; the
// call's own final disposition (a RetriesExhaustedError) is a separate
// question this record does not answer.
func TestBrokerAuthRetry_401Then503Exhausted(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newAuthRetryTestSender(t, func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "basic", 10, true)
	defer server.Close()

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatal("Do() unexpectedly succeeded despite the transient-retry cap")
		}
		var exhausted *RetriesExhaustedError
		if !errors.As(err, &exhausted) {
			t.Fatalf("Do() error = %v, want a *RetriesExhaustedError", err)
		}
	})

	// 1 initial 401, plus maxTransientRetries retried 503s, plus the 503 that
	// finally hits the cap and is not retried again.
	wantRequests := int32(1 + maxTransientRetries + 1)
	if got := requestCount.Load(); got != wantRequests {
		t.Fatalf("expected %d requests (1 401, then %d 503s), got %d", wantRequests, maxTransientRetries+1, got)
	}
	retries := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retries) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record for a 401->503 (exhausted) chain, got %d:\n%v", len(retries), records)
	}
	if got := retries[0]["outcome"]; got != string(audit.OutcomeSuccess) {
		t.Errorf("outcome = %v, want %q — the credential problem was resolved even though the call itself exhausted its transient-retry cap", got, audit.OutcomeSuccess)
	}
}

// erroringTransportAfterN wraps a transport, failing every request from the
// Nth one (1-indexed) onward with a plain connection-shaped error. Used to
// force a transport error on the retried request after a 401 recovery — a
// path that never re-enters checkRetry at all (retry.go's connection-error
// branch returns straight through retryablehttp's own default policy),
// which the pre-fix mid-chain decision silently dropped the record on.
type erroringTransportAfterN struct {
	inner http.RoundTripper
	after int32
	count atomic.Int32
}

func (t *erroringTransportAfterN) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.count.Add(1) >= t.after {
		return nil, errors.New("simulated connection error")
	}
	return t.inner.RoundTrip(req)
}

// TestBrokerAuthRetry_TransportErrorAfterRetry pins the other missing-emission
// hazard: the 401 recovers, but the retried request itself fails at the
// transport level (a reset connection, a broker restart mid-chain) rather
// than getting any HTTP response at all. Deciding the outcome in Do, from
// the chain's final error, still produces exactly one record — outcome=error,
// since the credential problem was never actually confirmed resolved.
func TestBrokerAuthRetry_TransportErrorAfterRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	httpClient := server.Client()
	httpClient.Transport = &erroringTransportAfterN{inner: httpClient.Transport, after: 2}

	jar := mustNewSafeCookieJar(t)
	httpClient.Jar = jar
	retries := 2
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RequestTimeoutDuration: 5 * time.Second,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
	}
	sender := New(httpClient, sempCfg, auth.NewBasicAuthenticator("admin", "secret", jar), server.URL,
		NewSemaphore(10), NewRateLimiter(0), WithAuditLog(true), WithBrokerAlias("dev-broker"))

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(context.Background(), req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatalf("Do() unexpectedly succeeded despite a forced transport error")
		}
	})

	retriesEmitted := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retriesEmitted) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record when the retried request fails at the transport, got %d:\n%v", len(retriesEmitted), records)
	}
	if got := retriesEmitted[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
}

// TestBrokerAuthRetry_ContextCancelledDuringBackoff pins the third
// missing-emission hazard: the caller's context is cancelled during the
// backoff that follows the 401 recovery, so retryablehttp aborts the chain —
// without checkRetry ever running a second time — and returns ctx.Err()
// directly. The cancellation is scheduled 30ms after the first request is
// received: long enough that checkRetry has already processed the 401 and
// set auth401Retried (the round trip itself completes near-instantly on
// localhost), but well inside the 200ms backoff the retry is waiting out, so
// it interrupts that wait rather than racing the first request itself.
func TestBrokerAuthRetry_ContextCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			go func() {
				time.Sleep(30 * time.Millisecond)
				cancel()
			}()
			return
		}
		jsonOK(w)
	}))
	defer server.Close()

	httpClient := server.Client()
	jar := mustNewSafeCookieJar(t)
	httpClient.Jar = jar
	retries := 3
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RequestTimeoutDuration: 5 * time.Second,
		RetryMinInterval:       200 * time.Millisecond,
		RetryMaxInterval:       500 * time.Millisecond,
	}
	sender := New(httpClient, sempCfg, auth.NewBasicAuthenticator("admin", "secret", jar), server.URL,
		NewSemaphore(10), NewRateLimiter(0), WithAuditLog(true), WithBrokerAlias("dev-broker"))

	req := newGetRequest(t, server.URL)
	records := captureAuditRecords(t, func() {
		resp, err := sender.Do(ctx, req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatal("Do() unexpectedly succeeded after the caller's context was cancelled")
		}
	})

	if got := requestCount.Load(); got != 1 {
		t.Fatalf("expected exactly 1 request (ctx cancelled before the retry could fire), got %d", got)
	}
	retriesEmitted := ofRetryType(records, audit.EventBrokerAuthRetry)
	if len(retriesEmitted) != 1 {
		t.Fatalf("want exactly 1 broker_auth_retry record when ctx is cancelled mid-chain, got %d:\n%v", len(retriesEmitted), records)
	}
	if got := retriesEmitted[0]["outcome"]; got != string(audit.OutcomeError) {
		t.Errorf("outcome = %v, want %q", got, audit.OutcomeError)
	}
}
