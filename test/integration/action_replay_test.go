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
	"errors"
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
		// A config-namespace PUT, for the omitted-annotation case. validateTool
		// rejects an action/ step whose annotations.idempotent is unset, so that
		// combination cannot reach production and a fixture built on it would
		// pin behaviour for a tool that can never exist. Omission is still legal
		// outside action/, which is where the "omitted changes nothing" guarantee
		// actually has to hold.
		"config/updateTestObject": {
			ID:     "updateTestObject",
			Method: http.MethodPut,
			Path:   "/SEMP/v2/config/__private_test__/testObject",
		},
	}

	newToolFor := func(operation string, idempotent *bool) composite.CompositeTool {
		return composite.CompositeTool{
			Name:        "test-action-tool",
			Description: "fixture",
			Steps: []composite.Step{{
				ID:        "act",
				Operation: operation,
			}},
			Result:      composite.ResultStrategy{Strategy: "collect"},
			Annotations: composite.ToolAnnotations{Idempotent: idempotent},
		}
	}
	newTool := func(idempotent *bool) composite.CompositeTool {
		return newToolFor("action/doTestAction", idempotent)
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
		// A per-test limiter is correct here: these fixtures build one protocol
		// client, so there is no second Sender to share with. minInterval is 0,
		// so it never paces and cannot skew the request counts these tests assert.
		client, err := sempv2.NewHTTPClient(
			brokerCfg, sempCfg, resilience.NewSemaphore(sempCfg.MaxConcurrentPerBroker),
			resilience.NewRateLimiter(minInterval),
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
	//
	// Deliberately a config/ operation, not action/: validateTool requires an
	// explicit idempotent on every action/ step, so an action tool with the
	// annotation omitted cannot load. Testing it here would pin behaviour for a
	// configuration production rejects, and would read as if omission were an
	// option for action tools. config/ is where omission remains legal, so it is
	// where the guarantee needs holding.
	t.Run("idempotent omitted leaves the policy untouched", func(t *testing.T) {
		var hits atomic.Int32
		client, server := newBrokerAndClient(t, &hits)
		defer server.Close()

		executor := composite.NewCompositeExecutor(operations)
		_, err := executor.Execute(context.Background(),
			newToolFor("config/updateTestObject", nil), client, map[string]any{})
		if err == nil {
			t.Fatal("expected the broken connection to surface as an error, got nil")
		}
		if got := hits.Load(); got != retries+1 {
			t.Errorf("broker saw %d requests, want %d — an omitted annotation must not "+
				"narrow the retry policy", got, retries+1)
		}
	})
}

// TestActionToolNotRetried503IsNotReportedRetryable closes the other half of the
// invariant. Suppressing the replay is only half the job: the agent also has to
// be told not to reissue the call itself.
//
// On a 503 the retry policy returns "don't retry" with no error, so
// retryablehttp hands the response straight back and Sender.Do takes its success
// path — the NonIdempotent marking there only covers the connection-error case.
// The 503 then becomes an ordinary *sempv2.SEMPError, and isRetryable reports
// any 503 as retryable. The tool layer duly tells the agent "retryable": true
// for a purge the broker may already have carried out, which is the exact
// side-effect duplication the transport-error path refuses to allow.
//
// This tier is the only place the defect is observable: resilience sees a
// correctly suppressed retry, and the tools layer sees a plain 503.
func TestActionToolNotRetried503IsNotReportedRetryable(t *testing.T) {
	operations := map[string]*sempv2.Operation{
		"action/doTestAction": {
			ID:     "doTestAction",
			Method: http.MethodPut,
			Path:   "/SEMP/v2/__private_action__/testAction",
		},
	}
	falseVal := false
	tool := composite.CompositeTool{
		Name:        "test-action-tool",
		Description: "fixture",
		Steps:       []composite.Step{{ID: "act", Operation: "action/doTestAction"}},
		Result:      composite.ResultStrategy{Strategy: "collect"},
		Annotations: composite.ToolAnnotations{Idempotent: &falseVal},
	}

	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"meta":{"error":{"code":89,"description":"Replication Is Standby","status":"NOT_ALLOWED"}}}`))
	}))
	defer server.Close()

	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	attempts := 3
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
		resilience.NewRateLimiter(minInterval),
		auth.NewBasicAuthenticator("admin", "secret", jar), jar,
	)
	if err != nil {
		t.Fatalf("sempv2.NewHTTPClient: %v", err)
	}

	executor := composite.NewCompositeExecutor(operations)
	_, execErr := executor.Execute(context.Background(), tool, client, map[string]any{})
	if execErr == nil {
		t.Fatal("expected the 503 to surface as an error, got nil")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("broker saw %d requests on 503, want exactly 1", got)
	}

	// NonIdempotent on a *RetriesExhaustedError is the exported contract the
	// tools layer keys off: internal/tools.isRetryable returns !NonIdempotent for
	// this error type, so the flag is what stops "retryable": true reaching the
	// agent. A bare *sempv2.SEMPError carrying 503 takes the other branch and is
	// reported retryable regardless of the annotation.
	var exhausted *resilience.RetriesExhaustedError
	if !errors.As(execErr, &exhausted) {
		t.Fatalf("a deliberately un-retried non-idempotent action surfaced as %T, not "+
			"*RetriesExhaustedError — the tools layer will read the raw 503 and report "+
			"it retryable: %v", execErr, execErr)
	}
	if !exhausted.NonIdempotent {
		t.Error("RetriesExhaustedError.NonIdempotent is false for an action the retry " +
			"policy refused to replay, so the agent is told to retry a possibly-applied purge")
	}
	// Routing through errorHandler drains the body to release the connection,
	// which would otherwise throw away the broker's account of what happened.
	// "Replication Is Standby" is a pre-execution rejection: it is the difference
	// between "the purge may have run" and "the purge definitely did not run",
	// on the one operation where that distinction is most expensive to get wrong.
	if exhausted.Detail != "Replication Is Standby" {
		t.Errorf("broker reason lost in the error routing: Detail = %q, want %q",
			exhausted.Detail, "Replication Is Standby")
	}
}
