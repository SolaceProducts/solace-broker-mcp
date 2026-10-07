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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SOL-155411: a create/update (POST/PATCH) is never replayed, but until the
// fix only a caller-declared non-idempotent request was reported as "may have
// already applied it". A create that timed out after the broker received it
// was reported as "try again later" while the queue already existed. These
// tests pin the line the fix draws: once the request has been written, the
// outcome is unknown; before that, nothing reached the broker and the failure
// stays an ordinary, retryable one.

// stallAfterReadHandler reads the whole request — so the broker side has it —
// and then never answers, which is what a slow broker looks like to a client
// whose timeout fires first. It returns once the client gives up, so
// server.Close does not hang.
func stallAfterReadHandler(counter *atomic.Int32) http.HandlerFunc {
	return func(_ http.ResponseWriter, r *http.Request) {
		counter.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}
}

// newTimeoutSender points a Sender at handler through an http.Client whose
// Timeout is short. That exercises the http.Client.Timeout path; in production
// the transport's ResponseHeaderTimeout (half of semp.request_timeout_duration)
// usually fires first, which the tools-layer test exercises through a real
// sempv2 client.
func newTimeoutSender(t *testing.T, handler http.HandlerFunc) (*Sender, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	httpClient := server.Client()
	httpClient.Timeout = 100 * time.Millisecond
	d := newTestSenderBasic(t, httpClient, 0)
	d.brokerURL = server.URL
	return d, server
}

func newBodyRequest(t *testing.T, method, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url+"/test",
		strings.NewReader(`{"queueName":"q1"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

func requireExhausted(t *testing.T, resp *http.Response, err error) *RetriesExhaustedError {
	t.Helper()
	if resp != nil {
		resp.Body.Close()
	}
	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("want *RetriesExhaustedError, got %T: %v", err, err)
	}
	return exhausted
}

// The ticket's reproduction: the broker received the create and applied it,
// but the response did not arrive within the request timeout.
func TestSender_POSTTimeoutAfterSend_MarksOutcomeUnknown(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTimeoutSender(t, stallAfterReadHandler(&requestCount))
	defer server.Close()

	resp, err := sender.Do(context.Background(), newBodyRequest(t, http.MethodPost, server.URL))
	exhausted := requireExhausted(t, resp, err)

	if !exhausted.NonIdempotent {
		t.Error("NonIdempotent is false for a POST that timed out after the broker received it; " +
			"the agent is told to try again although the create may already have been applied")
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("POST reached the broker %d times, want exactly 1", got)
	}
	if msg := exhausted.Error(); strings.Contains(msg, "caller declared") {
		t.Errorf("Error() attributes the refusal to a caller declaration nobody made: %q", msg)
	}
}

// Same outcome for an update whose connection drops after it was sent.
func TestSender_PATCHConnectionLostAfterSend_MarksOutcomeUnknown(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTestSenderWithServer(t, abortHandler(&requestCount), "basic", 10)
	defer server.Close()

	resp, err := sender.Do(context.Background(), newBodyRequest(t, http.MethodPatch, server.URL))
	exhausted := requireExhausted(t, resp, err)

	if !exhausted.NonIdempotent {
		t.Error("NonIdempotent is false for a PATCH whose connection dropped after it was sent")
	}
	if got := requestCount.Load(); got != 1 {
		t.Errorf("PATCH reached the broker %d times, want exactly 1", got)
	}
}

// Control: a POST that never left the client (connection refused) cannot have
// been applied. Flagging it would tell the agent to check state for a change
// that never happened, and would wrongly make a safe retry non-retryable.
func TestSender_POSTDialFailure_NotMarkedOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached; the server should be closed")
	}))
	httpClient := server.Client()
	url := server.URL
	server.Close()

	sender := newTestSenderBasic(t, httpClient, 0)
	sender.brokerURL = url

	resp, err := sender.Do(context.Background(), newBodyRequest(t, http.MethodPost, url))
	exhausted := requireExhausted(t, resp, err)

	if exhausted.NonIdempotent {
		t.Error("NonIdempotent set for a POST that was never sent (connection refused)")
	}
}

// Control: SEMPv1 sends read-only <show> commands over POST and marks them
// retry-safe. A timed-out read changed nothing, so it must keep the ordinary
// "try again" treatment.
func TestSender_RetrySafePOSTTimeout_NotMarkedOutcomeUnknown(t *testing.T) {
	var requestCount atomic.Int32
	sender, server := newTimeoutSender(t, stallAfterReadHandler(&requestCount))
	defer server.Close()

	resp, err := sender.Do(WithRetrySafe(context.Background()), newBodyRequest(t, http.MethodPost, server.URL))
	exhausted := requireExhausted(t, resp, err)

	if exhausted.NonIdempotent {
		t.Error("NonIdempotent set for a retry-safe POST (a SEMPv1 show command)")
	}
}
