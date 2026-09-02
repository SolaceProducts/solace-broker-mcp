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
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	const wantTransient = "Authentication is unavailable — the identity provider is not responding."

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
			// Broker alias is deliberately NOT interpolated into the
			// transient message — IdP-class failures affect every
			// broker at once, so naming one would be misleading.
			// Confirm the message is stable regardless of the alias.
			if got := e.AgentMessage("any-broker"); got != wantTransient {
				t.Errorf("AgentMessage(any-broker) = %q, want %q", got, wantTransient)
			}
			if got := e.AgentMessage(""); got != wantTransient {
				t.Errorf("AgentMessage(empty alias) = %q, want %q (alias must not influence transient message)", got, wantTransient)
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

// TestExchangeError_LogAttrs_EndpointSurvivesRedaction pins the two properties
// that make the IdP endpoint usable to an operator debugging a failed token
// exchange (SOL-153827):
//
//  1. The endpoint SURVIVES the redaction net. It is logged under
//     "idp_endpoint" rather than "token_endpoint" because redactSecretAttr in
//     cmd/server/main.go blanks the value of any key containing "token" — so
//     the old key reached operators as [REDACTED] on every token-exchange
//     failure, hiding the one field that separates a misconfigured IdP URL
//     from an unreachable one.
//  2. The endpoint is SANITIZED. Any userinfo is stripped before logging, so
//     a credentialed URL cannot ride the (now un-redacted) key into the log
//     stream. Defense in depth: config.validateBrokerURL already rejects a
//     credentialed idp_token_endpoint at load, mirroring the credentialed-URL
//     case in sempv1/client_test.go's TestHTTPClient_LogValue_ExcludesCredentials.
//
// Assertions run against the whole rendered line, so the [REDACTED] count also
// pins the rest of the key namespace: a future field whose key collides with
// the net fails here rather than silently going blank in production. Two of
// LogAttrs' keys are sentinel-gated (breaker_state, gate) and unreachable from
// a single error value, so the cases below vary the sentinel to render every
// key LogAttrs can emit.
func TestExchangeError_LogAttrs_EndpointSurvivesRedaction(t *testing.T) {
	t.Parallel()

	const (
		// Host and path that must survive sanitizing intact.
		wantEndpoint = "https://idp.example.com/token"
		// Userinfo spliced into wantEndpoint; must not appear in the output.
		credentialedEndpoint = "https://idp-user:idp-pass@idp.example.com/token"
		controlSecret        = "CONTROL_SECRET_VALUE"
	)

	cases := []struct {
		name     string
		sentinel error
		// gatedKey is the sentinel-gated key this case exists to render, and
		// is asserted present so the case cannot silently become vacuous.
		gatedKey string
	}{
		{name: "unconditional fields", sentinel: ErrExchangeRejected},
		{name: "breaker open", sentinel: ErrExchangeCircuitOpen, gatedKey: "breaker_state"},
		{name: "rate limited", sentinel: ErrExchangeRateLimited, gatedKey: "gate"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := &ExchangeError{
				Sentinel:      tc.sentinel,
				Message:       "IdP rejected the grant",
				TokenEndpoint: credentialedEndpoint,
				BrokerAlias:   "my-broker",
				Audience:      "https://broker.example.com",
				HTTPStatus:    502,
				FailureClass:  FailureClassUpstream5xx,
				Elapsed:       150 * time.Millisecond,
			}

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{
				ReplaceAttr: redactSecretAttrTestFixture,
			}))

			// The control attr is the anti-tautology guard: "subject_token"
			// MUST be redacted. Without it, a test asserting "idp_endpoint is
			// not [REDACTED]" would pass just as happily against a handler
			// whose filter never fires, and so would prove nothing about the
			// key name.
			attrs := append(err.LogAttrs(), slog.String("subject_token", controlSecret))
			logger.LogAttrs(context.Background(), slog.LevelError, "tool invoked", attrs...)
			got := buf.String()

			if tc.gatedKey != "" && !strings.Contains(got, `"`+tc.gatedKey+`":`) {
				t.Errorf("case did not render its gated key %q, so it is not covering it: %s",
					tc.gatedKey, got)
			}
			if want := `"idp_endpoint":"` + wantEndpoint + `"`; !strings.Contains(got, want) {
				t.Errorf("rendered line is missing %s\ngot: %s", want, got)
			}
			for _, credential := range []string{"idp-user", "idp-pass"} {
				if strings.Contains(got, credential) {
					t.Errorf("rendered line leaked URL userinfo %q: %s", credential, got)
				}
			}
			if strings.Contains(got, controlSecret) {
				t.Errorf("control attr was not redacted, so the test's ReplaceAttr is not "+
					"exercising the production filter: %s", got)
			}
			// Exactly one: the control attr. Any second occurrence means a
			// LogAttrs field collided with the redaction net.
			if n := strings.Count(got, "[REDACTED]"); n != 1 {
				t.Errorf("[REDACTED] count = %d, want exactly 1 (the subject_token control attr); "+
					"a LogAttrs key is colliding with the redaction net: %s", n, got)
			}
		})
	}
}
