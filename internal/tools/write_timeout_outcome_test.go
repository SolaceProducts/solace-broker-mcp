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

package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// executeCreateQueue sends a create (POST) through a real SEMPv2 client with a
// short semp.request_timeout_duration to a broker played by handler, and
// returns the error the tools layer would see.
func executeCreateQueue(t *testing.T, handler http.HandlerFunc) error {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	retries := 2
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 150 * time.Millisecond,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       time.Millisecond,
		RetryMaxInterval:       10 * time.Millisecond,
	}
	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	client, err := sempv2.NewHTTPClient(
		&config.BrokerConfig{URL: server.URL, Auth: config.AuthConfig{Mode: "basic"}},
		sempCfg, resilience.NewSemaphore(10), resilience.NewRateLimiter(0),
		auth.NewBasicAuthenticator("admin", "secret", jar), jar)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}

	op := &sempv2.Operation{
		ID:         "createMsgVpnQueue",
		Method:     http.MethodPost,
		Path:       "/SEMP/v2/config/msgVpns/default/queues",
		Parameters: []sempv2.Parameter{{Name: "body", In: "body"}},
	}
	_, execErr := client.Execute(context.Background(), op,
		map[string]any{"body": map[string]any{"queueName": "mcp-test-timeout-1"}})
	if execErr == nil {
		t.Fatal("expected the create to fail, got nil")
	}
	return execErr
}

// requireOutcomeUnknown asserts the agent is told the write may have been
// applied, and is not invited to repeat it.
func requireOutcomeUnknown(t *testing.T, err error) {
	t.Helper()
	msg, _ := buildErrorMessage(err, "ucd-editor")
	if !strings.Contains(msg, "may have already applied it") {
		t.Errorf("message does not warn that the create may have been applied: %q", msg)
	}
	for _, banned := range []string{"HTTP 0", "try again later"} {
		if strings.Contains(msg, banned) {
			t.Errorf("message still contains %q for a write whose outcome is unknown: %q", banned, msg)
		}
	}
	if isRetryable(err) {
		t.Error("isRetryable is true; the agent would be invited to repeat a create the broker may already have applied")
	}
}

// TestBuildErrorMessage_CreateTimedOutAfterSend_ReportsOutcomeUnknown is the
// SOL-155411 reproduction end to end: the broker reads the create and never
// answers within semp.request_timeout_duration. The agent-facing message must
// say the broker may already have applied it — not "(HTTP 0) ... try again
// later", which reported a failure for a queue that had in fact been created.
func TestBuildErrorMessage_CreateTimedOutAfterSend_ReportsOutcomeUnknown(t *testing.T) {
	requireOutcomeUnknown(t, executeCreateQueue(t, func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
}

// The broker answers 200 — so it applied the create — but the connection
// stalls partway through the body. The failure surfaces while reading the
// response, after the Sender has returned, and must get the same treatment.
func TestBuildErrorMessage_CreateResponseBodyLost_ReportsOutcomeUnknown(t *testing.T) {
	requireOutcomeUnknown(t, executeCreateQueue(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":{"queueName":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
}
