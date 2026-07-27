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
	"strings"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
)

// trackingBody records whether the response body was fully read (drained)
// and closed. Both must happen for the underlying connection to be returned
// to the transport's idle pool instead of being torn down.
type trackingBody struct {
	reader  io.Reader
	drained bool
	closed  bool
}

func (b *trackingBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if errors.Is(err, io.EOF) {
		b.drained = true
	}
	return n, err
}

func (b *trackingBody) Close() error {
	b.closed = true
	return nil
}

func newErrorHandlerTestSender(t *testing.T) *Sender {
	t.Helper()
	retries := 2
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	jar := mustNewSafeCookieJar(t)
	authn := auth.NewBasicAuthenticator("admin", "secret", jar)
	return New(&http.Client{Jar: jar}, sempCfg, authn, "http://broker.example:8080", NewSemaphore(10), NewRateLimiter(0))
}

func newExhaustedResponse(t *testing.T, body *trackingBody) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, "http://broker.example:8080/SEMP/v2/monitor/about", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Request:    req,
		Body:       body,
	}
}

// retryablehttp does NOT drain or close the final response body when a custom
// ErrorHandler is set (go-retryablehttp client.go:395-399: the handler owns
// the body). These tests pin the errorHandler side of that contract; without
// the drain+close the connection's persistConn is leaked until the client
// timeout fires.

func TestSender_ErrorHandler_ClosesBody_OnHTTPExhaustion(t *testing.T) {
	sender := newErrorHandlerTestSender(t)
	body := &trackingBody{reader: strings.NewReader("rate limited")}
	resp := newExhaustedResponse(t, body) //nolint:bodyclose // errorHandler closes the body; this test asserts exactly that

	_, err := sender.errorHandler(resp, nil, 3) //nolint:bodyclose // errorHandler closes the body; this test asserts exactly that
	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected *RetriesExhaustedError, got %T: %v", err, err)
	}
	if exhausted.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want %d", exhausted.StatusCode, http.StatusTooManyRequests)
	}
	if exhausted.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", exhausted.Attempts)
	}
	if !body.closed {
		t.Error("response body not closed — connection leaked until client timeout")
	}
	if !body.drained {
		t.Error("response body not drained — connection cannot be reused")
	}
}

func TestSender_ErrorHandler_ClosesBody_OnErrorWithResponse(t *testing.T) {
	// The checkRetry ctx-cancellation path returns a non-nil error while the
	// response (and its body) is still open.
	sender := newErrorHandlerTestSender(t)
	body := &trackingBody{reader: strings.NewReader("broker down")}
	resp := newExhaustedResponse(t, body) //nolint:bodyclose // errorHandler closes the body; this test asserts exactly that

	_, err := sender.errorHandler(resp, context.Canceled, 1) //nolint:bodyclose // errorHandler closes the body; this test asserts exactly that
	var exhausted *RetriesExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected *RetriesExhaustedError, got %T: %v", err, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
	if !body.closed {
		t.Error("response body not closed on error branch")
	}
	if !body.drained {
		t.Error("response body not drained on error branch — connection cannot be reused")
	}
}
