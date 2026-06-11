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

package resilience

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// SEMPv1 is an RPC protocol where read-only <show> commands travel over HTTP
// POST. The method-based idempotency guard must not disable retries for
// requests the caller has explicitly marked retry-safe — otherwise SEMPv1
// monitoring tools get zero resilience (no 429/503 backoff, no connection
// retry, dead 401 re-auth) while equivalent SEMPv2 GETs ride through blips.

// Sender.Do's ctx argument governs the retry-safe marker (Do overwrites the
// request's own context), so tests pass the marked ctx to Do directly.
func newPOSTRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/SEMP",
		strings.NewReader(`<rpc><show><version/></show></rpc>`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func TestSender_RetrySafePOST_503_Retries(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		jsonOK(w)
	}, "basic", 10)
	defer server.Close()

	resp, err := sender.Do(WithRetrySafe(context.Background()), newPOSTRequest(t, server.URL))
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 1 retry), got %d", requestCount.Load())
	}
}

func TestSender_RetrySafePOST_401_BasicAuth_ClearsCookiesAndRetries(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		jsonOK(w)
	}, "basic", 10)
	defer server.Close()

	resp, err := sender.Do(WithRetrySafe(context.Background()), newPOSTRequest(t, server.URL))
	if err != nil {
		t.Fatalf("Do() error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 after 401 re-auth retry", resp.StatusCode)
	}
	if requestCount.Load() != 2 {
		t.Errorf("expected 2 requests (original + 401 retry), got %d", requestCount.Load())
	}
}

func TestSender_UnmarkedPOST_503_StillNotRetried(t *testing.T) {
	// Mutating POSTs without the retry-safe mark must keep today's behavior:
	// a double-write is worse than a visible failure.
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}, "basic", 10)
	defer server.Close()

	resp, err := sender.Do(context.Background(), newMethodRequest(t, http.MethodPost, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil && resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 passthrough or error, got status %d", resp.StatusCode)
	}
	if requestCount.Load() != 1 {
		t.Errorf("expected exactly 1 request for unmarked POST, got %d", requestCount.Load())
	}
}
