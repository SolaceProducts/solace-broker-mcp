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

package tokenexchange

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFailureClass_String pins the log-safe labels. These strings appear
// in the failure_class log attribute; a change is operator-visible.
func TestFailureClass_String(t *testing.T) {
	t.Parallel()
	cases := map[FailureClass]string{
		FailureClassNone:        "none",
		FailureClassNetwork:     "network",
		FailureClassUpstream5xx: "upstream_5xx",
		FailureClassRateLimited: "rate_limited",
		FailureClassBodyRead:    "body_read",
		FailureClassConfig:      "config",
		FailureClass(99):        "none", // unknown falls back to "none"
	}
	for fc, want := range cases {
		if got := fc.String(); got != want {
			t.Errorf("FailureClass(%d).String() = %q, want %q", fc, got, want)
		}
	}
}

// TestParseIdPResponse_FailureClassPerStatus asserts the sub-classification
// each response path assigns. These are the raw (pre-retry-rewrap) values
// the circuit breaker ultimately reads through the ErrExchangeTransport
// sentinel.
func TestParseIdPResponse_FailureClassPerStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		status     int
		body       string
		wantClass  FailureClass
		wantStatus int
	}{
		{"429 rate limited", http.StatusTooManyRequests, "", FailureClassRateLimited, http.StatusTooManyRequests},
		{"500 upstream", http.StatusInternalServerError, "", FailureClassUpstream5xx, http.StatusInternalServerError},
		{"503 upstream", http.StatusServiceUnavailable, "", FailureClassUpstream5xx, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, err := New(validParams(t))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			resp := &http.Response{
				StatusCode: tc.status,
				Header:     http.Header{},
				Body:       &trackingBodyOK{r: strings.NewReader(tc.body)},
			}
			_, err = e.parseIdPResponse(resp, pinnedNow())

			var exchErr *ExchangeError
			if !errors.As(err, &exchErr) {
				t.Fatalf("errors.As(err, *ExchangeError) = false; err = %v", err)
			}
			if !errors.Is(err, ErrExchangeTransport) {
				t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
			}
			if exchErr.FailureClass != tc.wantClass {
				t.Errorf("FailureClass = %v, want %v", exchErr.FailureClass, tc.wantClass)
			}
			if exchErr.HTTPStatus != tc.wantStatus {
				t.Errorf("HTTPStatus = %d, want %d", exchErr.HTTPStatus, tc.wantStatus)
			}
		})
	}
}

// TestParseIdPResponse_FailureClassBodyRead asserts a mid-body read failure
// classifies as FailureClassBodyRead with no HTTP status (the read failure
// is not tied to a status code). Reuses trackingBodyError from response_test.go.
func TestParseIdPResponse_FailureClassBodyRead(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{},
		Body:       &trackingBodyError{},
	}
	_, err = e.parseIdPResponse(resp, pinnedNow())

	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) {
		t.Fatalf("errors.As(err, *ExchangeError) = false; err = %v", err)
	}
	if exchErr.FailureClass != FailureClassBodyRead {
		t.Errorf("FailureClass = %v, want BodyRead", exchErr.FailureClass)
	}
	if exchErr.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 (body-read failure carries no status)", exchErr.HTTPStatus)
	}
}

// TestDoExchange_FailureClassNetwork asserts a connection-level failure (no
// usable HTTP response) classifies as FailureClassNetwork with no status.
// The server is closed before the call so the dial fails.
func TestDoExchange_FailureClassNetwork(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // close immediately so the connection is refused

	e := newTestExchanger(t, url)
	_, err := e.Exchange(context.Background(), validInput())

	var exchErr *ExchangeError
	if !errors.As(err, &exchErr) {
		t.Fatalf("errors.As(err, *ExchangeError) = false; err = %v", err)
	}
	if !errors.Is(err, ErrExchangeTransport) {
		t.Errorf("errors.Is(err, ErrExchangeTransport) = false, want true; err = %v", err)
	}
	if exchErr.FailureClass != FailureClassNetwork {
		t.Errorf("FailureClass = %v, want Network", exchErr.FailureClass)
	}
	if exchErr.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 (network failure carries no status)", exchErr.HTTPStatus)
	}
}

// TestExchangeError_LogAttrsIncludesFailureClass asserts the failure_class
// attribute is emitted when set and omitted when FailureClassNone, matching
// the zero-value-omission pattern of the other LogAttrs fields.
func TestExchangeError_LogAttrsIncludesFailureClass(t *testing.T) {
	t.Parallel()

	withClass := &ExchangeError{Sentinel: ErrExchangeTransport, FailureClass: FailureClassUpstream5xx}
	if !hasLogAttr(withClass.LogAttrs(), "failure_class", "upstream_5xx") {
		t.Errorf("LogAttrs missing failure_class=upstream_5xx; got %v", withClass.LogAttrs())
	}

	noClass := &ExchangeError{Sentinel: ErrExchangeRejected} // FailureClassNone
	for _, a := range noClass.LogAttrs() {
		if a.Key == "failure_class" {
			t.Errorf("LogAttrs emitted failure_class for FailureClassNone; want omitted")
		}
	}
}

// TestExchangeError_LogAttrsBreakerState asserts the circuit-open case gets a
// structured breaker_state=open marker (so operators can filter/alert on it),
// carries no failure_class (no IdP call was attempted), and that non-open
// errors omit the marker entirely.
func TestExchangeError_LogAttrsBreakerState(t *testing.T) {
	t.Parallel()

	open := &ExchangeError{Sentinel: ErrExchangeCircuitOpen}
	if !hasLogAttr(open.LogAttrs(), "breaker_state", "open") {
		t.Errorf("LogAttrs missing breaker_state=open for circuit-open error; got %v", open.LogAttrs())
	}
	for _, a := range open.LogAttrs() {
		if a.Key == "failure_class" {
			t.Errorf("circuit-open error must not carry failure_class (no IdP call was made); got %v", open.LogAttrs())
		}
	}

	// A genuine transport failure must NOT get the breaker_state marker.
	notOpen := &ExchangeError{Sentinel: ErrExchangeTransport, FailureClass: FailureClassNetwork}
	for _, a := range notOpen.LogAttrs() {
		if a.Key == "breaker_state" {
			t.Errorf("non-open error emitted breaker_state; want omitted; got %v", notOpen.LogAttrs())
		}
	}
}

// TestClassifyRetryOutcome_DeadlinePathHasNoFailureClass asserts that when
// the exhaustion rewrap fires on the chain-deadline path (no underlying
// *ExchangeError to copy from), FailureClass stays None — the honest signal
// that the last attempt received no classifiable response. This is the
// counterpart to the transport-path tests that assert the class IS carried.
func TestClassifyRetryOutcome_DeadlinePathHasNoFailureClass(t *testing.T) {
	t.Parallel()

	e, err := New(validParams(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// attempts >= 1 and a bare context.DeadlineExceeded triggers the
	// deadline branch of classifyRetryOutcome, where exchErr is nil.
	out := e.classifyRetryOutcome(context.Background(), context.DeadlineExceeded, 2, "test-broker")

	if !errors.Is(out, ErrExchangeRetriesExhausted) {
		t.Fatalf("errors.Is(out, ErrExchangeRetriesExhausted) = false; out = %v", out)
	}
	var exchErr *ExchangeError
	if !errors.As(out, &exchErr) {
		t.Fatalf("errors.As(out, *ExchangeError) = false; out = %v", out)
	}
	if exchErr.FailureClass != FailureClassNone {
		t.Errorf("FailureClass = %v, want None (deadline path has no underlying transport class)", exchErr.FailureClass)
	}
	if exchErr.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 (deadline path has no status)", exchErr.HTTPStatus)
	}
}

// hasLogAttr reports whether attrs contains a String attr with the given
// key and value.
func hasLogAttr(attrs []slog.Attr, key, val string) bool {
	for _, a := range attrs {
		if a.Key == key && a.Value.String() == val {
			return true
		}
	}
	return false
}
