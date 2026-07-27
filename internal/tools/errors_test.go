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
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceDev/solace-broker-mcp/internal/tokenexchange"
)

func TestSanitizeBrokerText(t *testing.T) {
	var tests = []struct {
		name  string
		input string
		want  string
	}{
		{"plain text unchanged", "Queue 'myQueue' not found", "Queue 'myQueue' not found"},
		{"ipv4 replaced", "Connected to 192.168.1.100 on port 8080", "Connected to [ip] on port 8080"},
		{"ipv6 bracketed replaced", "SSL rejected from [2001:db8::1] port 55443", "SSL rejected from [ip] port 55443"},
		{"ipv6 full form replaced", "from 2001:db8:0:0:0:0:2:1 closed", "from [ip] closed"},
		{"ipv6 with zone replaced", "bind fe80::1%eth0 failed", "bind [ip] failed"},
		{"ipv6 v4-mapped replaced", "mapped ::ffff:1.2.3.4 seen", "mapped [ip] seen"},
		// Must NOT trip on colon-hex that isn't an address: no "::", not eight groups,
		// or a "::" separator with a non-hextet side (e.g. C++ scope resolution).
		{"clock time preserved", "last seen at 12:34:56 UTC", "last seen at 12:34:56 UTC"},
		{"mac address preserved", "device 12:34:56:78:9a:bc joined", "device 12:34:56:78:9a:bc joined"},
		{"double-colon separator preserved", "namespace foo::bar referenced", "namespace foo::bar referenced"},
		{"cpp scope resolution preserved", "use std::string here", "use std::string here"},
		// A bracketed token with no colon is not an address and must be left alone.
		{"bracketed non-address preserved", "queue [deadbeef] is full", "queue [deadbeef] is full"},
		{"fs path replaced", "Error reading /opt/solace/config.json", "Error reading [path]"},
		// Broadened FS prefixes: /home, /tmp, /root must redact too.
		{"home path replaced", "dump written to /home/solace/core.123", "dump written to [path]"},
		{"tmp path replaced", "spool at /tmp/sol/spool.db locked", "spool at [path] locked"},
		{"root path replaced", "key at /root/.ssh/id_rsa missing", "key at [path] missing"},
		// SEMP object paths (no FS prefix) must not be redacted.
		{"semp path preserved", "Object /msgVpns/default/queues/myQueue not found", "Object /msgVpns/default/queues/myQueue not found"},
		// The regex intentionally swallows the adjacent ':' — over-redaction is safe; the
		// alternative (stopping early) would risk leaking the tail of a real path.
		{"ip and path together", "Error at /opt/conf: connect to 10.0.0.1 failed", "Error at [path] connect to [ip] failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output = sanitizeBrokerText(tt.input)
			if output != tt.want {
				t.Errorf("%s: got %q, want %q", tt.name, output, tt.want)
			}
		})
	}
}

func TestIsRetryable(t *testing.T) {
	var tests = []struct {
		name  string
		input error
		want  bool
	}{
		// RetriesExhaustedError is retryable by default: having exhausted the
		// internal retry budget on a transient condition, the same request may
		// well succeed later. The one exception is NonIdempotent (below).
		{"retries exhausted, http status", &resilience.RetriesExhaustedError{StatusCode: 503, Attempts: 3}, true},
		// A bare 500 is non-retryable (see sempv2/sempv1 cases below); a 500 that
		// exhausted internal retries is trusted as transient.
		{"retries exhausted, non-transient status still true", &resilience.RetriesExhaustedError{StatusCode: 500, Attempts: 3}, true},
		{"wrapped retries exhausted (errors.As traversal)", fmt.Errorf("executing tool %q: %w", "list-queues", &resilience.RetriesExhaustedError{StatusCode: 503, Attempts: 3}), true},
		// NonIdempotent means the retry policy deliberately did NOT replay the
		// request, because the broker may already have carried it out. Reporting
		// it retryable would invite the agent to repeat the exact side effect the
		// policy refused to duplicate — for a queue purge, destroying everything
		// spooled since the original call (SOL-152400).
		{"non-idempotent, not replayed", &resilience.RetriesExhaustedError{Attempts: 1, NonIdempotent: true, Err: errors.New("EOF")}, false},
		{"wrapped non-idempotent (errors.As traversal)", fmt.Errorf("executing tool %q: %w", "delete-queue-messages", &resilience.RetriesExhaustedError{Attempts: 1, NonIdempotent: true, Err: errors.New("EOF")}), false},

		// SEMPv2: retryable on 429/503 or a retryable comRc_t code (229).
		{"sempv2 429 (live, e.g. fronting proxy)", &sempv2.SEMPError{StatusCode: 429}, true},
		{"sempv2 503", &sempv2.SEMPError{StatusCode: 503}, true},
		{"sempv2 503 with non-retryable code (status wins)", &sempv2.SEMPError{StatusCode: 503, SEMPCode: 6}, true},
		{"sempv2 retryable code 229, non-503 status", &sempv2.SEMPError{StatusCode: 400, SEMPCode: 229}, true},
		{"sempv2 500 internal", &sempv2.SEMPError{StatusCode: 500}, false},
		{"sempv2 code not in table", &sempv2.SEMPError{StatusCode: 400, SEMPCode: 999}, false},
		{"sempv2 zero code (lookup miss safe)", &sempv2.SEMPError{StatusCode: 400, SEMPCode: 0}, false},

		// SEMPv1: retryable on 429/503 or a retryable reasonCode (229).
		{"sempv1 429 (HTTP kind)", &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 429}, true},
		{"sempv1 503 (HTTP kind)", &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 503}, true},
		{"sempv1 retryable reasonCode 229 (status 200 execute-fail)", &sempv1.Error{Kind: sempv1.ErrorKindExecuteFail, StatusCode: 200, ReasonCode: 229}, true},
		{"sempv1 500", &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 500}, false},
		{"sempv1 permission, non-retryable reason", &sempv1.Error{Kind: sempv1.ErrorKindPermission, StatusCode: 200, ReasonCode: 72}, false},
		{"sempv1 zero reasonCode (lookup miss safe)", &sempv1.Error{Kind: sempv1.ErrorKindExecuteFail, StatusCode: 200, ReasonCode: 0}, false},

		// Token exchange: only transport errors are retryable.
		{"exchange transport", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeTransport, Message: "connect timeout"}, true},
		{"exchange rejected", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeRejected, Message: "invalid_grant"}, false},
		{"exchange invalid response", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrInvalidResponse, Message: "bad json"}, false},
		{"exchange missing subject", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeMissingSubject, Message: "no subject token"}, false},
		{"exchange request build", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeRequestBuild, Message: "unparseable URL"}, false},
		// Retries-exhausted is deliberately non-retryable: we already tried
		// three times and gave up. isRetryable=true here would let the agent
		// hammer the IdP right after the server-side loop exhausted itself.
		// Pins the current false-by-default fallthrough so a future rework of
		// this function cannot silently flip it.
		{"exchange retries exhausted", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeRetriesExhausted, Message: "gave up"}, false},
		{"wrapped exchange transport", fmt.Errorf("oauth auth: %w", &tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeTransport, Message: "timeout"}), true},

		// Fall-through / non-SEMP errors are never retryable.
		{"plain error", errors.New("boom"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryable(tt.input); got != tt.want {
				t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestBuildSEMPv2Message(t *testing.T) {
	var tests = []struct {
		name  string
		input *sempv2.SEMPError
		want  string
	}{
		// 5xx (except 503) is suppressed behind the generic message, even when the
		// broker supplied a description — internal detail must never reach the agent.
		{"500 suppressed even with description", &sempv2.SEMPError{StatusCode: 500, Description: "internal stack trace"}, genericInternalMessage},

		// 503 is the exception: it carries a safe, useful reason, so let it through.
		{"503 passes description through", &sempv2.SEMPError{StatusCode: 503, Description: "VPN 'x' is reconciling"}, "VPN 'x' is reconciling"},
		{"503 without description", &sempv2.SEMPError{Operation: "getMsgVpnQueue", StatusCode: 503}, "getMsgVpnQueue returned HTTP 503"},

		// 4xx client errors show the broker's own description (sanitized).
		{"4xx with description", &sempv2.SEMPError{StatusCode: 404, Description: "Queue not found"}, "Queue not found"},
		{"4xx without description", &sempv2.SEMPError{Operation: "getMsgVpnQueue", StatusCode: 400}, "getMsgVpnQueue returned HTTP 400"},

		// The description is run through sanitizeBrokerText.
		{"description is sanitized", &sempv2.SEMPError{StatusCode: 400, Description: "connect to 10.0.0.1 failed"}, "connect to [ip] failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildSEMPv2Message(tt.input); got != tt.want {
				t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestBuildSEMPv1Message(t *testing.T) {
	var tests = []struct {
		name  string
		input *sempv1.Error
		want  string
	}{
		// Envelope "command failed" errors (status 200): show the broker's reason.
		// execute-fail represents the shared ExecuteFail/Parse/Permission/Limit branch.
		{"execute-fail with message", &sempv1.Error{Kind: sempv1.ErrorKindExecuteFail, StatusCode: 200, Message: "Unknown Queue"}, "execute-fail error: Unknown Queue"},
		{"execute-fail without message", &sempv1.Error{Kind: sempv1.ErrorKindExecuteFail, StatusCode: 200}, "execute-fail error (status=200)"},

		// The "Reduce the scope" suffix is appended only in the message-present
		// branch, so a limit error without a message does NOT get it.
		{"limit error appends guidance", &sempv1.Error{Kind: sempv1.ErrorKindLimit, StatusCode: 200, Message: "too many clients"}, "limit error: too many clients. Reduce the scope of the request."},
		{"limit without message gets no suffix", &sempv1.Error{Kind: sempv1.ErrorKindLimit, StatusCode: 200}, "limit error (status=200)"},

		// HTTP-layer failures branch on status: 503 transient, other 5xx suppressed,
		// 4xx shown as a bare status.
		{"HTTP 503 transient", &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 503}, serviceUnavailableMessage},
		{"HTTP 500 suppressed", &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 500}, genericInternalMessage},
		{"HTTP 4xx shown", &sempv1.Error{Kind: sempv1.ErrorKindHTTP, StatusCode: 401}, "HTTP 401 error"},

		// An unclassified envelope yields the generic message rather than echoing detail.
		{"unknown kind", &sempv1.Error{Kind: sempv1.ErrorKindUnknown, StatusCode: 200}, genericInternalMessage},

		// The message is run through sanitizeBrokerText.
		{"message is sanitized", &sempv1.Error{Kind: sempv1.ErrorKindExecuteFail, StatusCode: 200, Message: "read /opt/x failed"}, "execute-fail error: read [path] failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildSEMPv1Message(tt.input); got != tt.want {
				t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

func TestBuildErrorMessage(t *testing.T) {
	var tests = []struct {
		name            string
		input           error
		brokerAlias     string
		wantMsg         string
		wantSuggestions []string
	}{
		// Retry-exhaustion reports attempts/status and carries no object-level hint.
		{
			"retries exhausted carries no suggestions",
			&resilience.RetriesExhaustedError{StatusCode: 503, Attempts: 3},
			"broker-eu-prod",
			"Request failed after 3 attempts (HTTP 503). Internal retries exhausted; try again later.",
			nil,
		},
		// A non-idempotent request that was deliberately not replayed must not be
		// described as "try again later" — that text is what would drive an agent
		// to re-issue a purge the broker may already have applied (SOL-152400).
		{
			"non-idempotent failure tells the agent to verify, not retry",
			&resilience.RetriesExhaustedError{Attempts: 1, NonIdempotent: true, Err: errors.New("EOF")},
			"broker-eu-prod",
			"Request failed and was deliberately not retried, because repeating this " +
				"operation is not safe: the broker may have already applied it. Check the " +
				"current state before deciding whether to issue it again.",
			nil,
		},
		// A 503 is the one 5xx whose broker text is safe to pass on, and on this
		// path it is the reason the operator most needs: "Replication Is Standby"
		// is a pre-execution rejection, so the purge definitively did not run.
		{
			"non-idempotent 503 surfaces the broker's reason",
			&resilience.RetriesExhaustedError{
				Attempts: 1, NonIdempotent: true, StatusCode: 503,
				Detail: "Replication Is Standby", Err: errors.New("not retried"),
			},
			"broker-eu-prod",
			"Request failed and was deliberately not retried, because repeating this " +
				"operation is not safe: the broker may have already applied it. Check the " +
				"current state before deciding whether to issue it again. " +
				"The broker reported: Replication Is Standby",
			nil,
		},
		// Other 5xx descriptions can carry internal detail, so this path suppresses
		// them exactly like every other broker-text path. Without the guard the
		// non-idempotent branch became a way around the 5xx rule (review on #219).
		{
			"non-idempotent 500 withholds the broker's reason",
			&resilience.RetriesExhaustedError{
				Attempts: 1, NonIdempotent: true, StatusCode: 500,
				Detail: "panic in mgmt-plane worker 3 at solace-node-7.internal",
				Err:    errors.New("not retried"),
			},
			"broker-eu-prod",
			"Request failed and was deliberately not retried, because repeating this " +
				"operation is not safe: the broker may have already applied it. Check the " +
				"current state before deciding whether to issue it again.",
			nil,
		},
		// A known comRc_t code surfaces its curated hint alongside the message.
		{
			"sempv2 code 6 yields hint",
			&sempv2.SEMPError{StatusCode: 404, SEMPCode: 6, Description: "Queue not found"},
			"broker-eu-prod",
			"Queue not found",
			[]string{"Verify the name is correct."},
		},
		// Code 72 (permission denied) with a known alias: message is replaced with
		// an alias-tagged line and the generic role/VPN-scope hint is suppressed.
		{
			"sempv2 code 72 with alias tags broker, drops hint",
			&sempv2.SEMPError{StatusCode: 403, SEMPCode: 72, Description: "not authorized"},
			"broker-oauth",
			`Authorization failed on broker "broker-oauth".`,
			nil,
		},
		{
			"sempv1 code 72 with alias tags broker, drops hint",
			&sempv1.Error{Kind: sempv1.ErrorKindPermission, StatusCode: 200, ReasonCode: 72, Message: "not authorized"},
			"broker-oauth",
			`Authorization failed on broker "broker-oauth".`,
			nil,
		},
		// Code 72 with an empty alias: unchanged — broker's own message plus the
		// existing generic code-72 hint.
		{
			"sempv2 code 72 empty alias falls back to broker text plus generic hint",
			&sempv2.SEMPError{StatusCode: 403, SEMPCode: 72, Description: "not authorized"},
			"",
			"not authorized",
			[]string{"Credentials lack permission; check the management role/VPN scope."},
		},
		// Suppression happens before the hint lookup: a suppressed 5xx gets no hint
		// even though its code (6) has one in the table.
		{
			"sempv2 500 suppressed gives no hint even with known code",
			&sempv2.SEMPError{StatusCode: 500, SEMPCode: 6, Description: "stack trace"},
			"broker-eu-prod",
			genericInternalMessage,
			nil,
		},
		// Exchange errors route through (*ExchangeError).AgentMessage. The
		// two-category collapse means transport + retries-exhausted share
		// the transient message; every other sentinel (including the
		// default catch-all) shares the permanent "server-side" message
		// with the broker alias embedded.
		{
			"exchange rejected → permanent",
			&tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeRejected, Message: "invalid_grant"},
			"broker-oauth",
			`Authentication failed for broker "broker-oauth". This is a server-side issue.`,
			nil,
		},
		{
			"exchange transport → transient (broker name deliberately absent)",
			&tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeTransport, Message: "connect timeout"},
			"broker-oauth",
			"Authentication is unavailable — the identity provider is not responding.",
			nil,
		},
		{
			"exchange invalid response → permanent",
			&tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrInvalidResponse, Message: "unexpected content-type"},
			"broker-oauth",
			`Authentication failed for broker "broker-oauth". This is a server-side issue.`,
			nil,
		},
		{
			"exchange missing subject → permanent",
			&tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeMissingSubject, Message: "no subject token on context"},
			"broker-oauth",
			`Authentication failed for broker "broker-oauth". This is a server-side issue.`,
			nil,
		},
		{
			"exchange request build → permanent",
			&tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeRequestBuild, Message: "unparseable URL"},
			"broker-oauth",
			`Authentication failed for broker "broker-oauth". This is a server-side issue.`,
			nil,
		},
		{
			"exchange retries exhausted → transient (broker name deliberately absent)",
			&tokenexchange.ExchangeError{Sentinel: tokenexchange.ErrExchangeRetriesExhausted, Message: "retries exhausted"},
			"broker-oauth",
			"Authentication is unavailable — the identity provider is not responding.",
			nil,
		},

		// Unknown/internal errors never echo raw detail and carry no hint.
		{
			"unknown error",
			errors.New("boom"),
			"broker-eu-prod",
			genericInternalMessage,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, suggestions := buildErrorMessage(tt.input, tt.brokerAlias)
			if msg != tt.wantMsg {
				t.Errorf("%s: msg = %q, want %q", tt.name, msg, tt.wantMsg)
			}
			if !slices.Equal(suggestions, tt.wantSuggestions) {
				t.Errorf("%s: suggestions = %v, want %v", tt.name, suggestions, tt.wantSuggestions)
			}
		})
	}
}

func TestBuildErrorResult_ExchangeError_StructuredFields(t *testing.T) {
	m := &ToolManager{}
	err := fmt.Errorf("oauth auth: %w", &tokenexchange.ExchangeError{
		Sentinel: tokenexchange.ErrExchangeRejected,
		Message:  "invalid_grant",
	})

	result := m.buildErrorResult(err, "my-broker")

	if !result.IsError {
		t.Fatal("expected IsError=true")
	}

	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent type = %T, want map[string]any", result.StructuredContent)
	}

	if got := structured["error_source"]; got != "token_exchange" {
		t.Errorf("error_source = %v, want %q", got, "token_exchange")
	}
	if got := structured["retryable"]; got != false {
		t.Errorf("retryable = %v, want false (rejected is not retryable)", got)
	}

	transportErr := fmt.Errorf("oauth auth: %w", &tokenexchange.ExchangeError{
		Sentinel: tokenexchange.ErrExchangeTransport,
		Message:  "dial timeout",
	})
	transportResult := m.buildErrorResult(transportErr, "my-broker")
	transportStructured := transportResult.StructuredContent.(map[string]any)
	if got := transportStructured["retryable"]; got != true {
		t.Errorf("transport retryable = %v, want true", got)
	}

	missingSubjectErr := &tokenexchange.ExchangeError{
		Sentinel:    tokenexchange.ErrExchangeMissingSubject,
		Message:     "no subject token on context",
		BrokerAlias: "my-broker",
	}
	missingResult := m.buildErrorResult(missingSubjectErr, "my-broker")
	missingStructured := missingResult.StructuredContent.(map[string]any)
	if got := missingStructured["error_source"]; got != "token_exchange" {
		t.Errorf("missing subject error_source = %v, want %q", got, "token_exchange")
	}
	if got := missingStructured["retryable"]; got != false {
		t.Errorf("missing subject retryable = %v, want false", got)
	}
}

// Note: TestAgentMessage_* and TestExchangeError_NeverContainsSecrets
// live in internal/tokenexchange/errors_test.go — they exercise methods
// on *ExchangeError, and belong with the type they test.
