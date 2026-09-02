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
	"fmt"
	"log/slog"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// ExchangeError is a structured error returned by the token exchange
// layer. It carries diagnostic context (endpoint, broker, audience,
// HTTP status, elapsed time) through the fmt.Errorf wrapping chain so
// logToolResult can extract and log it on the single per-tool-invocation
// error line — same pattern as sempv1.Error and sempv2.SEMPError.
//
// Sentinel is one of ErrExchangeRejected, ErrExchangeTransport,
// ErrInvalidResponse, ErrExchangeMissingSubject, ErrExchangeRequestBuild,
// ErrExchangeRetriesExhausted, ErrExchangeCircuitOpen, or
// ErrExchangeRateLimited. Unwrap returns it so errors.Is works through any
// number of wrapping layers.
//
// Message is human-readable and safe to log (no tokens, secrets, or
// error_description fields). It is the value Error() returns.
//
// Fields populated at construction (response.go / doExchange):
//   - Sentinel, Message, HTTPStatus, FailureClass
//
// Fields enriched by Exchange() before returning:
//   - TokenEndpoint, BrokerAlias, Audience, Elapsed
type ExchangeError struct {
	Sentinel      error
	Message       string
	TokenEndpoint string
	BrokerAlias   string
	Audience      string
	HTTPStatus    int
	// FailureClass survives the ErrExchangeRetriesExhausted rewrap, which
	// replaces Sentinel — it is the only signal of the underlying transport
	// cause the breaker still has once retries are exhausted.
	FailureClass FailureClass
	Elapsed      time.Duration
	// RetryAfterResult carries the outcome of parsing the 429 response's
	// Retry-After header (present only when HTTPStatus == 429). Populated in
	// parseIdPResponse and copied forward by classifyRetryOutcome's rewrap
	// to ErrExchangeRetriesExhausted — the same "survive the rewrap"
	// treatment FailureClass gets, and for the same reason: this is the
	// last attempt's signal, and it is what runExchangeOnce uses to raise
	// the shared rate-limit gate on exhaustion. Nil when the response was
	// not a 429 at all (as opposed to a 429 with no usable header, which
	// is a non-nil result with ok == false).
	RetryAfterResult *retryAfterResult
}

func (e *ExchangeError) Error() string { return e.Message }
func (e *ExchangeError) Unwrap() error { return e.Sentinel }

// AgentMessage returns the sanitized string safe to surface to the MCP
// agent. Two categories: transient (Transport, RetriesExhausted) →
// "try again"; permanent (everything else, including default) →
// "server-side issue" named with brokerAlias. The Message field is
// never embedded — sentinel-specific detail belongs on LogAttrs, not
// on the agent surface. See PR SOL-151520 for the rationale.
func (e *ExchangeError) AgentMessage(brokerAlias string) string {
	if errors.Is(e, ErrExchangeTransport) || errors.Is(e, ErrExchangeRetriesExhausted) || errors.Is(e, ErrExchangeCircuitOpen) || errors.Is(e, ErrExchangeRateLimited) {
		// Deliberately not broker-named: the IdP is shared, so naming one
		// broker would mislead the agent into thinking another might work.
		return "Authentication is unavailable — the identity provider is not responding."
	}
	return fmt.Sprintf("Authentication failed for broker %q. This is a server-side issue.", brokerAlias)
}

// LogAttrs returns structured slog attributes for this error. Called by
// logToolResult — field ownership stays with the type so the manager
// doesn't need to know which fields an exchange error carries.
func (e *ExchangeError) LogAttrs() []slog.Attr {
	var attrs []slog.Attr
	if e.TokenEndpoint != "" {
		// Key is "idp_endpoint", not "token_endpoint": redactSecretAttr in
		// cmd/server/main.go replaces the value of any attribute whose
		// lowercased key contains one of its redactedKeys substrings, and
		// "token" is on that list. Under the old key this field reached
		// operators as [REDACTED] on every token-exchange failure, hiding the
		// one value that separates a misconfigured IdP endpoint from an
		// unreachable one (SOL-153827). Params.LogValue in types.go names the
		// same URL the same way, for the same reason.
		//
		// SanitizeURLString is defense in depth on top of that rename: now that
		// the key no longer trips the redaction net, nothing downstream would
		// strip userinfo if a credentialed URL ever reached here.
		// config.validateBrokerURL already rejects one at config load — the
		// same guarantee sempv1/sempv2 HTTPClient.LogValue sanitize behind.
		attrs = append(attrs, slog.String("idp_endpoint", config.SanitizeURLString(e.TokenEndpoint)))
	}
	if e.BrokerAlias != "" {
		attrs = append(attrs, slog.String("exchange_broker", e.BrokerAlias))
	}
	if e.Audience != "" {
		attrs = append(attrs, slog.String("audience", e.Audience))
	}
	if e.HTTPStatus != 0 {
		attrs = append(attrs, slog.Int("idp_http_status", e.HTTPStatus))
	}
	if e.FailureClass != FailureClassNone {
		attrs = append(attrs, slog.String("failure_class", e.FailureClass.String()))
	}
	// A circuit-open rejection made no IdP call, so it carries no FailureClass
	// or HTTPStatus. Emit a structured marker so an operator can filter/alert on
	// "the breaker fast-failed this" rather than substring-matching the message.
	if errors.Is(e, ErrExchangeCircuitOpen) {
		attrs = append(attrs, slog.String("breaker_state", "open"))
	}
	// Distinct marker from breaker_state: this was the Retry-After gate, not
	// the breaker, refusing the call.
	if errors.Is(e, ErrExchangeRateLimited) {
		attrs = append(attrs, slog.String("gate", "retry_after"))
	}
	if e.Elapsed != 0 {
		attrs = append(attrs, slog.String("exchange_total_elapsed", e.Elapsed.String()))
	}
	return attrs
}
