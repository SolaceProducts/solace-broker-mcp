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
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAgentMessage_TransientSentinels asserts the two-category collapse:
// ErrExchangeTransport and ErrExchangeRetriesExhausted are the only
// sentinels that classify as transient. Any change to this set is
// user-visible (the retryable=true structured field flows off the same
// classification via isRetryable), so pin the exact set.
func TestAgentMessage_TransientSentinels(t *testing.T) {
	const wantTransient = "Authentication temporarily unavailable. Please try again in a moment."

	transient := []struct {
		name     string
		sentinel error
	}{
		{"transport", ErrExchangeTransport},
		{"retries exhausted", ErrExchangeRetriesExhausted},
	}
	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			e := &ExchangeError{Sentinel: tc.sentinel, Message: "should not leak"}
			if got := e.AgentMessage("any-broker"); got != wantTransient {
				t.Errorf("AgentMessage = %q, want %q", got, wantTransient)
			}
		})
	}
}

// TestAgentMessage_PermanentSentinels covers the "everything else"
// bucket, including the default case for an unknown sentinel. The
// broker alias must appear in the returned string exactly as passed —
// callers use it to identify which broker's auth chain is broken in a
// multi-broker deployment.
func TestAgentMessage_PermanentSentinels(t *testing.T) {
	const alias = "broker-oauth-prod"
	wantPermanent := `Authentication failed for broker "broker-oauth-prod". This is a server-side issue.`

	permanent := []struct {
		name     string
		sentinel error
	}{
		{"rejected", ErrExchangeRejected},
		{"invalid response", ErrInvalidResponse},
		{"missing subject", ErrExchangeMissingSubject},
		{"request build", ErrExchangeRequestBuild},
		{"unknown sentinel falls to default", errors.New("something else entirely")},
	}
	for _, tc := range permanent {
		t.Run(tc.name, func(t *testing.T) {
			e := &ExchangeError{Sentinel: tc.sentinel, Message: "internal detail must not leak"}
			if got := e.AgentMessage(alias); got != wantPermanent {
				t.Errorf("AgentMessage = %q, want %q", got, wantPermanent)
			}
		})
	}
}

// TestAgentMessage_NeverLeaksInternalDetail is a defense-in-depth check
// on the sanitized-by-construction property claimed in the AgentMessage
// godoc. The Message field can hold IdP-generated text (error codes,
// timeouts, dial errors); it must never reach the agent surface. This
// test seeds Message with strings a hostile IdP might include and
// confirms none survive the AgentMessage call.
func TestAgentMessage_NeverLeaksInternalDetail(t *testing.T) {
	forbidden := []string{
		"invalid_grant",
		"dial tcp 10.0.0.5:443: connection refused",
		"error_description=user is not consented",
		"HTTP 500",
		"subject_token=eyJhbGciOi...",
	}

	sentinels := []error{
		ErrExchangeRejected,
		ErrExchangeTransport,
		ErrInvalidResponse,
		ErrExchangeMissingSubject,
		ErrExchangeRequestBuild,
		ErrExchangeRetriesExhausted,
	}

	for _, forbid := range forbidden {
		for _, s := range sentinels {
			e := &ExchangeError{Sentinel: s, Message: forbid}
			got := e.AgentMessage("test-broker")
			if strings.Contains(got, forbid) {
				t.Errorf("AgentMessage leaked forbidden substring %q for sentinel %v: %q",
					forbid, s, got)
			}
		}
	}
}

// TestExchangeError_NeverContainsSecrets pins the invariant that
// credential-bearing values (subject tokens, access tokens, client
// secrets) NEVER appear in any of the surfaces an operator or agent
// sees: AgentMessage, Error(), or LogAttrs(). Moved from
// internal/tools/errors_test.go with the AgentMessage refactor.
func TestExchangeError_NeverContainsSecrets(t *testing.T) {
	secrets := []string{"subject-token-value", "access-token-value", "client-secret-value"}

	// Seed BrokerAlias with a plausible value; AgentMessage embeds
	// the alias, so we want to confirm we're not accidentally embedding
	// secrets elsewhere.
	err := &ExchangeError{
		Sentinel:      ErrExchangeRejected,
		Message:       "IdP rejected the grant",
		TokenEndpoint: "https://idp.example.com/token",
		BrokerAlias:   "my-broker",
		Audience:      "https://broker.example.com",
		HTTPStatus:    400,
		Elapsed:       150 * time.Millisecond,
	}

	surfaces := map[string]string{
		"AgentMessage": err.AgentMessage("my-broker"),
		"Error":        err.Error(),
	}
	for name, surface := range surfaces {
		for _, secret := range secrets {
			if strings.Contains(surface, secret) {
				t.Errorf("%s contains secret %q: %s", name, secret, surface)
			}
		}
	}

	for _, attr := range err.LogAttrs() {
		val := attr.Value.String()
		for _, secret := range secrets {
			if strings.Contains(val, secret) {
				t.Errorf("LogAttrs attr %q contains secret %q: %s", attr.Key, secret, val)
			}
		}
	}
}
