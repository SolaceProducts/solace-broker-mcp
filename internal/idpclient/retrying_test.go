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

package idpclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingHandler is a tiny test double that returns a caller-specified
// status on every hit and tracks how many hits it received.
func countingHandler(t *testing.T, status int, body string, headers map[string]string) (http.HandlerFunc, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}, &hits
}

// TestNewRetryingHTTPClient_RetriesOn5xx asserts the core retry loop:
// a server that always returns 500 gets hit exactly exchangeRetryMax+1
// times (3 attempts total), and the final response the caller sees is
// the last 500 (via PassthroughErrorHandler, not an ErrorHandler-wrapped
// error).
func TestNewRetryingHTTPClient_RetriesOn5xx(t *testing.T) {
	handler, hits := countingHandler(t, http.StatusInternalServerError, "", nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client, err := NewRetryingHTTPClient()
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	ctx, attempts := WithAttemptsCounter(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if got := hits.Load(); got != exchangeRetryMax+1 {
		t.Errorf("server hits: got %d, want %d", got, exchangeRetryMax+1)
	}
	if attempts() != exchangeRetryMax+1 {
		t.Errorf("attempts counter: got %d, want %d", attempts(), exchangeRetryMax+1)
	}
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("final status: got %d, want 500", resp.StatusCode)
	}
}

// TestNewRetryingHTTPClient_NoRetryOn2xx confirms a success returns after
// exactly one attempt.
func TestNewRetryingHTTPClient_NoRetryOn2xx(t *testing.T) {
	handler, _ := countingHandler(t, http.StatusOK, `{"ok":true}`, nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client, err := NewRetryingHTTPClient()
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}
	ctx, attempts := WithAttemptsCounter(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if attempts() != 1 {
		t.Errorf("attempts: got %d, want 1", attempts())
	}
}

// TestNewRetryingHTTPClient_NoRetryOn4xx covers the deliberate non-retry
// on 4xx — including 401 (would not fix itself) and 429 (retrying
// amplifies IdP throttling; see NewRetryingHTTPClient doc).
func TestNewRetryingHTTPClient_NoRetryOn4xx(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"400", http.StatusBadRequest},
		{"401", http.StatusUnauthorized},
		{"403", http.StatusForbidden},
		{"429", http.StatusTooManyRequests},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, hits := countingHandler(t, tc.status, "", nil)
			srv := httptest.NewServer(handler)
			defer srv.Close()

			client, err := NewRetryingHTTPClient()
			if err != nil {
				t.Fatalf("NewRetryingHTTPClient: %v", err)
			}
			ctx, attempts := WithAttemptsCounter(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()

			if got := hits.Load(); got != 1 {
				t.Errorf("server hits: got %d, want 1", got)
			}
			if attempts() != 1 {
				t.Errorf("attempts: got %d, want 1", attempts())
			}
			if resp.StatusCode != tc.status {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

// TestNewRetryingHTTPClient_RetriesRecover checks the retry loop stops
// retrying once a success arrives: 500, 500, 200 → 3 attempts, final 200.
func TestNewRetryingHTTPClient_RetriesRecover(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := NewRetryingHTTPClient()
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}
	ctx, attempts := WithAttemptsCounter(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status: got %d, want 200", resp.StatusCode)
	}
	if attempts() != 3 {
		t.Errorf("attempts: got %d, want 3", attempts())
	}
}

// TestNewRetryingHTTPClient_CtxCancelAbortsBackoff cancels ctx during the
// backoff between attempts. The retry loop must observe the cancellation
// and return promptly with ctx.Err(); the counter reflects only the
// attempts that actually dispatched.
func TestNewRetryingHTTPClient_CtxCancelAbortsBackoff(t *testing.T) {
	// Return a status the policy retries on so the loop enters backoff
	// where cancellation actually matters.
	handler, hits := countingHandler(t, http.StatusInternalServerError, "", nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client, err := NewRetryingHTTPClient()
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}

	// Cancel just after the first attempt runs — before the second attempt
	// dispatches. exchangeRetryWaitMin is 1s, so 50ms is well inside the
	// first backoff window.
	ctx, cancel := context.WithCancel(context.Background())
	ctx, attempts := WithAttemptsCounter(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, nil)
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)

	if err == nil {
		defer resp.Body.Close()
		t.Fatalf("Do: expected error from cancelled ctx, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Do: expected context.Canceled, got %v", err)
	}
	// One attempt dispatched before cancellation; the counter must not
	// climb higher because the second attempt never ran.
	if got := hits.Load(); got != 1 {
		t.Errorf("server hits: got %d, want 1", got)
	}
	if attempts() != 1 {
		t.Errorf("attempts: got %d, want 1", attempts())
	}
	// Well under the full backoff floor — asserts we didn't wait out the
	// whole 1s minimum before honoring the cancel.
	if elapsed > 900*time.Millisecond {
		t.Errorf("elapsed too long: got %v, want < 900ms (cancel should abort backoff)", elapsed)
	}
}

// TestNewRetryingHTTPClient_ConnectionErrorRetries covers the err != nil
// path in checkRetry. httptest.Server's Close ends the listener; requests
// after that produce a connection error, which is retryable.
func TestNewRetryingHTTPClient_ConnectionErrorRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // now the listener is gone; every dial fails

	// Short per-attempt timeout to keep the test fast — 3 attempts × dial
	// error is bounded by the timeout, not by real network waits.
	client, err := NewRetryingHTTPClient(WithTimeout(200 * time.Millisecond))
	if err != nil {
		t.Fatalf("NewRetryingHTTPClient: %v", err)
	}
	ctx, attempts := WithAttemptsCounter(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)

	_, err = client.Do(req)
	if err == nil {
		t.Fatalf("Do: expected connection error, got nil")
	}
	if attempts() != exchangeRetryMax+1 {
		t.Errorf("attempts: got %d, want %d", attempts(), exchangeRetryMax+1)
	}
}

// stubRoundTripper is a test RoundTripper that returns a canned response
// without touching the network. Used to unit-test attemptsRecorder in
// isolation from server / retry-loop scaffolding.
type stubRoundTripper struct{ calls int }

func (s *stubRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// TestAttemptsRecorder_NilSafe covers the transport wrapper's guard for a
// missing counter on ctx. Direct unit test — no HTTP server, no retry
// loop — because the guard is a pure Go type-assertion. Regression guard
// against a naive `*c++` that would panic when nothing was attached.
func TestAttemptsRecorder_NilSafe(t *testing.T) {
	inner := &stubRoundTripper{}
	rec := &attemptsRecorder{inner: inner}

	// Plain ctx — no WithAttemptsCounter.
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example", nil)
	if _, err := rec.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("inner calls: got %d, want 1", inner.calls)
	}
}

// TestAttemptsRecorder_IncrementsWhenAttached asserts the wrapper writes
// to the counter attached via WithAttemptsCounter. Direct unit test to
// pin the increment behavior without needing the retry loop.
func TestAttemptsRecorder_IncrementsWhenAttached(t *testing.T) {
	inner := &stubRoundTripper{}
	rec := &attemptsRecorder{inner: inner}

	ctx, attempts := WithAttemptsCounter(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://example", nil)
	if _, err := rec.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, err := rec.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := attempts(); got != 2 {
		t.Errorf("attempts: got %d, want 2", got)
	}
}

// TestCheckRetry_Table isolates the retry-decision function so the
// policy is documented at the level a reviewer can scan without reading
// through httptest choreography.
func TestCheckRetry_Table(t *testing.T) {
	ctx := context.Background()
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// Table drives the four documented branches. The status-code field
	// is only consulted when respErr is nil.
	cases := []struct {
		name    string
		ctx     context.Context
		status  int
		respErr error
		want    bool
	}{
		{"ctx cancelled → no retry", cancelledCtx, http.StatusInternalServerError, nil, false},
		{"connection error → retry", ctx, 0, errors.New("dial tcp: refused"), true},
		{"500 → retry", ctx, http.StatusInternalServerError, nil, true},
		{"502 → retry", ctx, http.StatusBadGateway, nil, true},
		{"503 → retry", ctx, http.StatusServiceUnavailable, nil, true},
		{"504 → retry", ctx, http.StatusGatewayTimeout, nil, true},
		{"200 → no retry", ctx, http.StatusOK, nil, false},
		{"301 → no retry", ctx, http.StatusMovedPermanently, nil, false},
		{"400 → no retry", ctx, http.StatusBadRequest, nil, false},
		{"401 → no retry", ctx, http.StatusUnauthorized, nil, false},
		{"429 → no retry (deliberate; see NewRetryingHTTPClient doc)", ctx, http.StatusTooManyRequests, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.respErr == nil {
				resp = &http.Response{StatusCode: tc.status}
			}
			got, err := checkRetry(tc.ctx, resp, tc.respErr)
			if got != tc.want {
				t.Errorf("checkRetry: got %v, want %v", got, tc.want)
			}
			// On a cancelled ctx, checkRetry MUST surface context.Canceled
			// (or DeadlineExceeded) so retryablehttp exits with the right
			// error rather than a generic retry-abort. Every non-cancelled
			// branch surfaces nil so the library's normal accounting runs.
			if tc.ctx.Err() != nil {
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("checkRetry on cancelled ctx: err = %v, want context.Canceled/DeadlineExceeded", err)
				}
			} else if err != nil {
				t.Errorf("checkRetry on live ctx: err = %v, want nil", err)
			}
		})
	}
}

