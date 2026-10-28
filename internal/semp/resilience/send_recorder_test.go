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
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// SOL-156509: the operation audit record's request_sent field says whether a
// write reached the broker. NonIdempotent cannot carry that: it is set only for
// POST/PATCH (it also drives the retry rule) and only on a
// RetriesExhaustedError, which a cancel during backoff or a broker error reply
// never produces. These tests pin the separate recorder instead.

func okHandler(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }

func TestSendRecorder_MarksSentWriteOfEveryMethod(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			sender, server := newTestSenderWithServer(t, okHandler, "basic", 0)
			defer server.Close()

			ctx, rec := WithSendRecorder(context.Background())
			resp, err := sender.Do(ctx, newBodyRequest(t, method, server.URL))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if !rec.WriteSent() {
				t.Errorf("WriteSent() = false after a %s reached the broker", method)
			}
		})
	}
}

func TestSendRecorder_ReadsAreNotWrites(t *testing.T) {
	cases := map[string]func(context.Context, string) *http.Request{
		"GET": func(ctx context.Context, url string) *http.Request {
			return newGetRequest(t, url).WithContext(ctx)
		},
		// SEMPv1 <show> travels over POST but is marked retry-safe as a read.
		"retry-safe POST": func(ctx context.Context, url string) *http.Request {
			return newBodyRequest(t, http.MethodPost, url).WithContext(ctx)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			sender, server := newTestSenderWithServer(t, okHandler, "basic", 0)
			defer server.Close()

			ctx, rec := WithSendRecorder(context.Background())
			if name == "retry-safe POST" {
				ctx = WithRetrySafe(ctx)
			}
			resp, err := sender.Do(ctx, build(ctx, server.URL))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			resp.Body.Close()
			if rec.WriteSent() {
				t.Errorf("WriteSent() = true for a %s; a read must not count as a sent write", name)
			}
		})
	}
}

// A write that never left the client cannot have been applied.
func TestSendRecorder_DialFailureIsNotSent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(okHandler))
	httpClient := server.Client()
	url := server.URL
	server.Close()

	sender := newTestSenderBasic(t, httpClient, 0)
	sender.brokerURL = url

	ctx, rec := WithSendRecorder(context.Background())
	resp, err := sender.Do(ctx, newBodyRequest(t, http.MethodDelete, url))
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("Do succeeded against a closed server")
	}
	if rec.WriteSent() {
		t.Error("WriteSent() = true for a DELETE whose connection was refused")
	}
}

// The gap NonIdempotent leaves: a DELETE is retried, and a cancel during the
// backoff makes retryablehttp return a bare ctx.Err() with no
// RetriesExhaustedError. The broker still received the first attempt.
func TestSendRecorder_CancelDuringBackoffAfterSentDelete(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case received <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	retries := 3
	minInterval := time.Duration(0)
	sender := New(server.Client(), &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RequestTimeoutDuration: 30 * time.Second,
		RetryMinInterval:       10 * time.Second, // park in backoff until the cancel
		RetryMaxInterval:       10 * time.Second,
	}, bearerAuth(t), server.URL, NewSemaphore(10), NewRateLimiter(0))

	ctx, rec := WithSendRecorder(context.Background())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-received
		cancel()
	}()

	resp, err := sender.Do(ctx, newBodyRequest(t, http.MethodDelete, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if !rec.WriteSent() {
		t.Error("WriteSent() = false for a DELETE the broker received before the cancel")
	}
}

func TestSendRecorder_NilIsSafe(t *testing.T) {
	var rec *SendRecorder
	if rec.WriteSent() {
		t.Error("nil recorder reports a sent write")
	}
}

// SOL-156509 (PR #560 review): the retry budget and the caller's own deadline
// both end in context.DeadlineExceeded. CallTool classifies after the fact, so
// if the budget fires first and the caller's deadline passes before then, the
// error alone cannot say whose deadline it was. The recorder can.

// hangUntilCancelled holds every request until the client gives up on it. It
// reads the body first: net/http only notices a client hanging up once the
// body is consumed, and httptest.Server.Close waits for the handler.
func hangUntilCancelled(_ http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	<-r.Context().Done()
}

func newBudgetSender(t *testing.T, url string, client *http.Client, budget time.Duration) *Sender {
	t.Helper()
	retries := 0
	minInterval := time.Duration(0)
	return New(client, &config.SEMPConfig{
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RequestTimeoutDuration: budget, // retries=0, so the budget is exactly this
		RetryMinInterval:       time.Millisecond,
		RetryMaxInterval:       time.Millisecond,
	}, bearerAuth(t), url, NewSemaphore(10), NewRateLimiter(0))
}

func TestSendRecorder_RetryBudgetExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(hangUntilCancelled))
	defer server.Close()
	sender := newBudgetSender(t, server.URL, server.Client(), 50*time.Millisecond)

	ctx, rec := WithSendRecorder(context.Background())
	resp, err := sender.Do(ctx, newBodyRequest(t, http.MethodDelete, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if !rec.RetryBudgetExpired() {
		t.Error("RetryBudgetExpired() = false after the retry budget ended the request")
	}
}

func TestSendRecorder_CallerDeadlineIsNotTheRetryBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(hangUntilCancelled))
	defer server.Close()
	sender := newBudgetSender(t, server.URL, server.Client(), 30*time.Second)

	ctx, rec := WithSendRecorder(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	resp, err := sender.Do(ctx, newBodyRequest(t, http.MethodDelete, server.URL))
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if rec.RetryBudgetExpired() {
		t.Error("RetryBudgetExpired() = true, but the caller's own deadline ended the request")
	}
}

func TestSendRecorder_SuccessIsNotRetryBudgetExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(okHandler))
	defer server.Close()
	sender := newBudgetSender(t, server.URL, server.Client(), 30*time.Second)

	ctx, rec := WithSendRecorder(context.Background())
	resp, err := sender.Do(ctx, newBodyRequest(t, http.MethodDelete, server.URL))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if rec.RetryBudgetExpired() {
		t.Error("RetryBudgetExpired() = true for a request that succeeded")
	}
}
