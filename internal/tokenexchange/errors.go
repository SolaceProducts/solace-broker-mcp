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
// ErrInvalidResponse, ErrExchangeMissingSubject, or
// ErrExchangeRequestBuild. Unwrap returns it so errors.Is works
// through any number of wrapping layers.
//
// Message is human-readable and safe to log (no tokens, secrets, or
// error_description fields). It is the value Error() returns.
//
// Fields populated at construction (response.go / doExchange):
//   - Sentinel, Message, HTTPStatus
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
	Elapsed       time.Duration
}

func (e *ExchangeError) Error() string { return e.Message }
func (e *ExchangeError) Unwrap() error { return e.Sentinel }

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
	if e.Elapsed != 0 {
		attrs = append(attrs, slog.Duration("exchange_elapsed", e.Elapsed))
	}
	return attrs
}
