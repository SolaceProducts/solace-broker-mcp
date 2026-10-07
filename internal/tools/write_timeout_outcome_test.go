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

// TestBuildErrorMessage_CreateTimedOutAfterSend_ReportsOutcomeUnknown is the
// SOL-155411 reproduction end to end: a real SEMPv2 client sends a create
// (POST), the broker reads it and never answers within
// semp.request_timeout_duration, and the agent-facing message must say the
// broker may already have applied it — not "(HTTP 0) ... try again later",
// which led operators to retry a create that had in fact succeeded.
func TestBuildErrorMessage_CreateTimedOutAfterSend_ReportsOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()

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
		t.Fatal("expected the create to fail with a timeout, got nil")
	}

	msg, _ := buildErrorMessage(execErr, "ucd-editor")
	if !strings.Contains(msg, "may have already applied it") {
		t.Errorf("message does not warn that the create may have been applied: %q", msg)
	}
	for _, banned := range []string{"HTTP 0", "try again later"} {
		if strings.Contains(msg, banned) {
			t.Errorf("message still contains %q for a write whose outcome is unknown: %q", banned, msg)
		}
	}
	if isRetryable(execErr) {
		t.Error("isRetryable is true; the agent would be invited to repeat a create the broker may already have applied")
	}
}
