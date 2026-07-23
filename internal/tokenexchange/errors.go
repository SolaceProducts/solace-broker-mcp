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
)

// ExchangeError is a structured error returned by the token exchange
// layer. It carries diagnostic context (endpoint, broker, audience,
// HTTP status, elapsed time) through the fmt.Errorf wrapping chain so
// logToolResult can extract and log it on the single per-tool-invocation
// error line — same pattern as sempv1.Error and sempv2.SEMPError.
//
// Sentinel is one of ErrExchangeRejected, ErrExchangeTransport,
// ErrInvalidResponse, ErrExchangeMissingSubject, ErrExchangeRequestBuild,
// ErrExchangeRetriesExhausted, or ErrExchangeCircuitOpen. Unwrap returns
// it so errors.Is works through any number of wrapping layers.
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
	if errors.Is(e, ErrExchangeTransport) || errors.Is(e, ErrExchangeRetriesExhausted) || errors.Is(e, ErrExchangeCircuitOpen) {
		// Deliberately not broker-named: the IdP is a shared component,
		// so a transport-class failure affects every broker at once.
		// Naming a broker here would mislead the agent into thinking a
		// different broker might work.
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
		attrs = append(attrs, slog.String("token_endpoint", e.TokenEndpoint))
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
	if e.Elapsed != 0 {
		attrs = append(attrs, slog.Duration("exchange_elapsed", e.Elapsed))
	}
	return attrs
}
