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
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite"
	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
)

// TestActionToolIsNotReplayedOnTransportError composes the composite executor
// with a real sempv2.HTTPClient and resilience.Sender, and verifies that a tool
// declaring `idempotent: false` reaches the broker exactly once when the
// connection breaks mid-request.
//
// The invariant is meaningless on either component alone, which is why it lives
// in this tier (see test/integration/README.md). The composite package owns the
// `idempotent` annotation; the resilience package owns the retry policy and
// infers replay safety from the HTTP method. Neither can observe the defect by
// itself: SEMPv2's action API routes destructive RPC over PUT, so the method
// check alone concludes "idempotent, safe to retry" and a broken connection
// replays the call. doMsgVpnQueueDeleteMsgs takes no message-ID range — it is
// specified as "Delete all spooled messages from the Queue" — so every replay
// destroys whatever producers spooled during the retry backoff, which the
// caller never authorized.
//
// Failure model: if the annotation stops reaching the retry policy (the wiring
// in CompositeExecutor.Execute is removed, or the marker stops being captured
// into retryState), the first subtest sees 1 + RetryMax requests instead of 1.
// The second subtest is the counterweight: it pins that an idempotent tool
// keeps its retry resilience, so the fix cannot pass by disabling retries
// wholesale.
func TestActionToolIsNotReplayedOnTransportError(t *testing.T) {
	const retries = 3

	// A PUT in the action namespace, mirroring the shape of the two shipped
	// non-idempotent action tools. No path parameters, to keep the fixture
	// focused on the retry decision rather than URL construction.
	operations := map[string]*sempv2.Operation{
		"action/doTestAction": {
			ID:     "doTestAction",
			Method: http.MethodPut,
			Path:   "/SEMP/v2/__private_action__/testAction",
		},
	}

	newTool := func(idempotent *bool) composite.CompositeTool {
		return composite.CompositeTool{
			Name:        "test-action-tool",
			Description: "fixture",
			Steps: []composite.Step{{
				ID:        "act",
				Operation: "action/doTestAction",
			}},
			Result:      composite.ResultStrategy{Strategy: "collect"},
			Annotations: composite.ToolAnnotations{Idempotent: idempotent},
		}
	}

	// Aborting the handler closes the connection without a response, which the
	// client sees as a transport-level error. This models the reachable
	// production triggers — TCP reset, broker restart mid-request, load-balancer
	// idle drop — without depending on timing.
	newBrokerAndClient := func(t *testing.T, hits *atomic.Int32) (sempv2.Client, *httptest.Server) {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			panic(http.ErrAbortHandler)
		}))

		jar, err := resilience.NewSafeCookieJar()
		if err != nil {
			t.Fatalf("NewSafeCookieJar: %v", err)
		}
		attempts := retries
		minInterval := time.Duration(0)
		sempCfg := &config.SEMPConfig{
			Retries:                &attempts,
			RequestMinInterval:     &minInterval,
			RequestTimeoutDuration: 30 * time.Second,
			RetryMinInterval:       1 * time.Millisecond,
			RetryMaxInterval:       10 * time.Millisecond,
			MaxConcurrentPerBroker: 4,
		}
		brokerCfg := &config.BrokerConfig{
			URL:  server.URL,
			Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "secret"},
		}
		client, err := sempv2.NewHTTPClient(
			brokerCfg, sempCfg, resilience.NewSemaphore(sempCfg.MaxConcurrentPerBroker),
			auth.NewBasicAuthenticator("admin", "secret", jar), jar,
		)
		if err != nil {
			t.Fatalf("sempv2.NewHTTPClient: %v", err)
		}
		return client, server
	}

	falseVal, trueVal := false, true

	t.Run("idempotent false reaches the broker exactly once", func(t *testing.T) {
		var hits atomic.Int32
		client, server := newBrokerAndClient(t, &hits)
		defer server.Close()

		executor := composite.NewCompositeExecutor(operations)
		_, err := executor.Execute(context.Background(), newTool(&falseVal), client, map[string]any{})
		if err == nil {
			t.Fatal("expected the broken connection to surface as an error, got nil")
		}
		if got := hits.Load(); got != 1 {
			t.Errorf("broker saw %d requests, want exactly 1 — a replayed destructive "+
				"action destroys messages spooled after the caller's request", got)
		}
	})

	t.Run("idempotent true keeps retry resilience", func(t *testing.T) {
		var hits atomic.Int32
		client, server := newBrokerAndClient(t, &hits)
		defer server.Close()

		executor := composite.NewCompositeExecutor(operations)
		_, err := executor.Execute(context.Background(), newTool(&trueVal), client, map[string]any{})
		if err == nil {
			t.Fatal("expected the broken connection to surface as an error, got nil")
		}
		if got := hits.Load(); got != retries+1 {
			t.Errorf("broker saw %d requests, want %d (original + %d retries) — an "+
				"idempotent action must keep riding through transport blips", got, retries+1, retries)
		}
	})

	// The annotation is a *bool so that "omitted" is distinguishable from
	// "explicitly false". Only an explicit false may narrow the retry policy;
	// a tool that says nothing must behave exactly as it did before this change.
	t.Run("idempotent omitted leaves the policy untouched", func(t *testing.T) {
		var hits atomic.Int32
		client, server := newBrokerAndClient(t, &hits)
		defer server.Close()

		executor := composite.NewCompositeExecutor(operations)
		_, err := executor.Execute(context.Background(), newTool(nil), client, map[string]any{})
		if err == nil {
			t.Fatal("expected the broken connection to surface as an error, got nil")
		}
		if got := hits.Load(); got != retries+1 {
			t.Errorf("broker saw %d requests, want %d — an omitted annotation must not "+
				"narrow the retry policy", got, retries+1)
		}
	})
}
