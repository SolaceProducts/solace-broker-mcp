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
	"regexp"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/tokenexchange"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// genericInternalMessage is returned to the agent in place of any 500-class
// broker description, so internal detail never leaks.
const genericInternalMessage = "The broker reported an internal error. Please try again later or contact your administrator."

// serviceUnavailableMessage is returned for a 503, which signals a transient
// condition the caller should retry rather than a permanent internal error.
const serviceUnavailableMessage = "The broker is temporarily unavailable. Please try again shortly."

// codeInfo struct is used to store a single row of the translatedErrorCodes table
type codeInfo struct {
	hint string // generic actionable hint surfaced to the agent
	// retryable marks transient codes where the same request might succeed on
	// retry (HTTP 503 and exhausted internal retries are handled separately).
	retryable bool
}

// translatedErrorCodes maps the comRc_t integer to a generic actionable hint.
// Note that the comRc_t code corresponds to the SEMPv2 error.code / SEMPv1 reasonCode.
var translatedErrorCodes = map[int]codeInfo{
	6:   {hint: "Verify the name is correct."},
	10:  {hint: "Update the existing object or choose a new name."},
	11:  {hint: "Check attribute values against the SEMP schema."},
	14:  {hint: "Not supported for this object or broker mode."},
	27:  {hint: "Check the request/query syntax."},
	72:  {hint: "Credentials lack permission; check the management role/VPN scope."},
	89:  {hint: "Not allowed in the object's current state."},
	135: {hint: "A configured maximum was reached."},
	228: {hint: "Supply all required fields for this object."},
	229: {hint: "Retry shortly.", retryable: true},
	256: {hint: "Drain or remove contained objects before deleting."},
}

// fsPrefixPattern matches filesystem paths by their leading prefix only, so
// SEMP object paths (e.g. /msgVpns/foo/queues/bar) — which have none of these
// prefixes — are preserved. This is the load-bearing constraint of the
// sanitizer: every prefix here must be a real OS root that can never collide
// with a SEMP object collection name. The set covers the common Unix roots that
// could leak host layout (including /home, /tmp, /root) plus a Windows drive.
var fsPrefixPattern = regexp.MustCompile(`(?i)(/opt/|/var/|/usr/|/etc/|/home/|/root/|/tmp/|/mnt/|/srv/|[A-Za-z]:\\)\S*`)

// ipv4Pattern matches dotted-quad IPv4 addresses. Known trade-off: a broker
// version with four dot-separated numbers (e.g. "10.2.1.5") has the same shape
// as a dotted-quad IPv4 address, so it is redacted to "[ip]" in the agent-facing
// message. Over-redaction is safe (the raw version still survives in the server
// log), so we accept the lost debugging signal rather than complicate the regex.
var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)

// ipv6Pattern matches IPv6 addresses, both the broker's bracket-wrapped form
// ("[2001:db8::1]", brackets consumed) and bare forms matched defensively.
// Requiring a hextet on both sides of "::" avoids redacting non-address tokens
// like "foo::bar"; the cost is that bare "::1"/"::" (non-sensitive) are skipped.
// Over-redaction is otherwise acceptable — the raw text survives in the server log.
// The IPv4-tailed branch is first because Go's regexp prefers the first matching
// branch, not the longest, so v4-mapped addresses match whole.
var ipv6Pattern = regexp.MustCompile(`(?i)\[?(?:` +
	// "::" with an embedded IPv4 tail (v4-mapped), e.g. ::ffff:1.2.3.4
	`(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?::(?:[0-9a-f]{1,4}:)*(?:\d{1,3}\.){3}\d{1,3}` +
	`|` +
	// Compressed "::", hextet required on both sides, e.g. 2001:db8::1
	`(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)` +
	`|` +
	// Full eight-group form, e.g. 2001:db8:0:0:0:0:2:1
	`(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}` +
	`)(?:%[0-9a-zA-Z._-]+)?\]?`)

// buildErrorResult converts a tool execution or broker-construction error into
// an MCP-compliant CallToolResult with IsError: true. Per the MCP spec, tool
// execution errors should be returned as results (not protocol-level errors)
// so the LLM can see them and self-correct. StructuredContent carries
// machine-readable fields (retryable, status, protocol-specific data, plus a
// suggestions array); Content carries a human-readable text message.
//
// err here is not vouched-for: it may be a handler's arbitrary error, or an
// arbitrary broker-construction failure from classifyBrokerError's
// broker_init_error branch. The unsuppressed/unsanitized text is never
// returned to the agent through this path — it is logged server-side by
// logToolResult as the "detail" field on the single per-invocation error line,
// so the whole event stays in one record. Contrast buildLocalErrorResult,
// which echoes its error verbatim because that error is always one this
// package constructed itself (SOL-152980).
func (m *ToolManager) buildErrorResult(err error, brokerAlias string) *mcp.CallToolResult {
	msg, suggestions := buildErrorMessage(err, brokerAlias)
	retryable := isRetryable(err)

	structured := map[string]any{
		"error":     msg,
		"retryable": retryable,
	}

	var sempv2Err *sempv2.SEMPError
	var sempv1Err *sempv1.Error
	var retriesErr *resilience.RetriesExhaustedError
	var busyErr *resilience.BrokerBusyError
	var exchErr *tokenexchange.ExchangeError

	switch {
	// Checked before the protocol errors: a shed request never reached the
	// broker, so there is no status or SEMP code to report, and the useful
	// machine-readable fact is how long to wait.
	case errors.As(err, &busyErr):
		// retryAfterMs is the configured bound, not the pacing interval. The
		// trigger is sustained saturation, so telling every shed caller to come
		// back in one interval just re-forms the queue that caused the shed.
		// max_queue_wait accepts any Go duration, so a sub-millisecond bound
		// (e.g. 500µs) would otherwise truncate to 0 here and could read as
		// "retry immediately" despite the human-facing message rounding up.
		structured["retryAfterMs"] = max(1, busyErr.MaxWait.Milliseconds())
		structured["error_source"] = "load_shed"
	case errors.As(err, &sempv2Err):
		structured["status"] = sempv2Err.StatusCode
		structured["operation"] = sempv2Err.Operation
		if sempv2Err.SEMPStatus != "" {
			structured["sempStatus"] = sempv2Err.SEMPStatus
		}
		if sempv2Err.SEMPCode != 0 {
			structured["sempCode"] = sempv2Err.SEMPCode
		}
	case errors.As(err, &sempv1Err):
		structured["status"] = sempv1Err.StatusCode
		structured["kind"] = sempv1Err.Kind.String()
		if sempv1Err.ReasonCode != 0 {
			structured["reasonCode"] = sempv1Err.ReasonCode
		}
	case errors.As(err, &retriesErr):
		if retriesErr.StatusCode != 0 {
			structured["status"] = retriesErr.StatusCode
		}
		structured["attempts"] = retriesErr.Attempts
	case errors.As(err, &exchErr):
		structured["error_source"] = "token_exchange"
	}

	// Content text mirrors the agent-facing message, with any suggestions
	// appended so non-structured consumers still see the guidance.
	contentText := msg
	if len(suggestions) > 0 {
		structured["suggestions"] = suggestions
		for _, s := range suggestions {
			contentText += "\n" + s
		}
	}

	return &mcp.CallToolResult{
		StructuredContent: structured,
		Content:           []mcp.Content{&mcp.TextContent{Text: contentText}},
		IsError:           true,
	}
}

// buildLocalErrorResult converts a local CallTool failure — one this package
// constructed itself, never a broker response or a handler's arbitrary error
// — into an MCP-spec CallToolResult the agent can see, instead of a
// protocol-level error (SOL-152980). Covers: missing broker parameter,
// unknown broker alias, parameter validation failure, a handler returning a
// nil/invalid result, output-schema validation failure, and a result that
// fails to marshal.
//
// Unlike buildErrorResult, err.Error() is shown verbatim: every caller of
// this function passes a message this package wrote directly (e.g. "broker
// parameter is required; available brokers: ...", the JSON-schema validation
// text, "tool %q returned nil result") — never broker- or handler-originated
// text, which is exactly what buildErrorResult exists to keep away from the
// agent. Do not route a wrapped broker or handler error through this
// function; use buildErrorResult instead.
//
// None of these are retryable with the same arguments: each is a
// deterministic caller mistake (bad tool name upstream of this function,
// missing/invalid parameters) or a server-side contract violation (nil
// output, schema mismatch, marshal failure) that retrying cannot fix.
func buildLocalErrorResult(err error) *mcp.CallToolResult {
	msg := err.Error()
	return &mcp.CallToolResult{
		StructuredContent: map[string]any{
			"error":     msg,
			"retryable": false,
		},
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}
}

// buildBrokerResolutionErrorResult converts a classifyBrokerError outcome into
// an MCP-spec CallToolResult (SOL-152980). The two outcomes need different
// treatment: unknown_broker's message is pre-crafted by classifyBrokerError
// itself (it echoes the caller's own alias verbatim, by design — see
// TestCallTool_UnknownBroker_PreservesCallerCasing) and goes through
// buildLocalErrorResult unchanged. broker_init_error wraps an arbitrary
// underlying construction failure — today from cookie-jar or authenticator
// setup, and in the future potentially a *tokenexchange.ExchangeError from an
// OAuth broker's init path — so it is not vouched-for text and goes through
// buildErrorResult instead, which suppresses unrecognized error text behind
// the generic message, classifies an ExchangeError via its own AgentMessage,
// and computes retryable correctly (a transient exchange failure is
// retryable; an unrecognized construction error is not).
func (m *ToolManager) buildBrokerResolutionErrorResult(errorType string, err error, brokerAlias string) *mcp.CallToolResult {
	// errorType is classifyBrokerError's own return value — the two string
	// literals it can produce ("unknown_broker", "broker_init_error") are
	// asserted against directly here rather than via a shared const, matching
	// every other errorType value in manager.go.
	if errorType == "unknown_broker" {
		return buildLocalErrorResult(err)
	}
	return m.buildErrorResult(err, brokerAlias)
}

// buildErrorMessage produces the human-readable, agent-facing error string
// along with any actionable suggestions. It prefers the broker's own
// description for client/config errors (lightly sanitized as defense-in-depth),
// but suppresses 500-class internal detail behind a generic message so it never
// reaches the agent.
//
// Suggestions are the curated generic hint for the broker's comRc_t code.
// Server-suppressed (5xx, except 503) errors get no object-level guidance.
//
// brokerAlias is the caller's own config label for the invocation (never a
// host, URL, or IP). When non-empty it is echoed back on the code-72
// (permission-denied) path so a multi-broker operator can tell which broker
// denied the request from the agent's output alone.
func buildErrorMessage(err error, brokerAlias string) (string, []string) {
	var retriesErr *resilience.RetriesExhaustedError
	var busyErr *resilience.BrokerBusyError
	var sempv2Err *sempv2.SEMPError
	var sempv1Err *sempv1.Error
	var exchErr *tokenexchange.ExchangeError

	var message string
	var status, code int // broker HTTP status and comRc_t code, for suggestions

	switch {
	// The server shed this request before sending it: the broker was too busy
	// to admit it within semp.max_queue_wait (SOL-153442). Every word here is
	// server-generated, so nothing needs sanitizing — no broker text exists,
	// because no broker was contacted.
	//
	// Saying the request was not sent is the load-bearing part. It tells the
	// agent this is safe to repeat even for a write, which is the opposite of
	// the non-idempotent retry-exhaustion case below.
	case errors.As(err, &busyErr):
		waitSeconds := max(1, int(busyErr.MaxWait.Round(time.Second)/time.Second))
		unit := "seconds"
		if waitSeconds == 1 {
			unit = "second"
		}
		return fmt.Sprintf(
				"The broker is too busy to accept this request right now, so it was not sent. "+
					"Nothing was changed on the broker. Wait about %d %s and try again.",
				waitSeconds, unit),
			[]string{"If this keeps happening, the broker is saturated: reduce how many requests you issue at once."}

	case errors.As(err, &retriesErr):
		// The underlying network error can contain a host or IP address, so
		// don't show it. Just report how many attempts were made and the
		// status. Retry-exhaustion carries no object-level suggestions.
		if retriesErr.NonIdempotent {
			// The request was deliberately not replayed because the broker may
			// already have carried it out. Telling the agent to try again would
			// re-run exactly the side effect the retry policy just refused to
			// duplicate — for a queue purge, destroying everything spooled
			// since the original call.
			msg := "Request failed and was deliberately not retried, because repeating " +
				"this operation is not safe: the broker may have already applied it. " +
				"Check the current state before deciding whether to issue it again."
			// The broker's own reason, when it gave one, is what narrows "may have
			// applied it" down to an answer. A 503 saying "Replication Is Standby"
			// or "VPN busy reconciling" is a pre-execution rejection: nothing ran.
			// Withholding it leaves the agent guessing on the one operation where
			// guessing is most expensive.
			//
			// Subject to the same 5xx suppression as every other broker-text path:
			// the gate fires on 429, 503, and other 5xx, and that last group is
			// exactly the class whose description may carry internal detail.
			if retriesErr.Detail != "" && brokerTextMayBeShown(retriesErr.StatusCode) {
				msg += " The broker reported: " + sanitizeBrokerText(retriesErr.Detail)
			}
			return msg, nil
		}
		return fmt.Sprintf(
			"Request failed after %d attempts (HTTP %d). Internal retries exhausted; try again later.",
			retriesErr.Attempts, retriesErr.StatusCode), nil

	case errors.As(err, &exchErr):
		return exchErr.AgentMessage(brokerAlias), nil

	case errors.As(err, &sempv2Err):
		status, code = sempv2Err.StatusCode, sempv2Err.SEMPCode
		message = buildSEMPv2Message(sempv2Err)

	case errors.As(err, &sempv1Err):
		status, code = sempv1Err.StatusCode, sempv1Err.ReasonCode
		message = buildSEMPv1Message(sempv1Err)

	default:
		// Unknown/internal error: never echo raw detail to the agent.
		return genericInternalMessage, nil
	}

	// Suggestions, shared by the SEMPv1/SEMPv2 paths. Server-suppressed 5xx
	// (except 503) get no object-level guidance.
	if !brokerTextMayBeShown(status) {
		return message, nil
	}
	// Code 72 (permission denied): when the caller's broker alias is known,
	// replace the broker's own text with an alias-tagged authorization message
	// and suppress the generic code-72 hint. That hint points at management role
	// and VPN scope — the Basic-auth knobs — which actively misleads an OAuth
	// operator, where authorization is governed by the oauthProfile
	// accessLevelGroups mapping instead. When the alias is empty the behavior is
	// unchanged: the broker's own message plus the generic hint below.
	if code == 72 && brokerAlias != "" {
		return fmt.Sprintf("Authorization failed on broker %q.", brokerAlias), nil
	}
	if info, ok := translatedErrorCodes[code]; ok && info.hint != "" {
		return message, []string{info.hint}
	}
	return message, nil
}

// buildSEMPv1Message builds the message the agent sees for a SEMPv1 error.
//
// SEMPv1 can fail in two different ways:
//  1. The HTTP request itself fails (4xx/5xx). The status code is real, but
//     there is no readable message — only a raw body we don't show.
//  2. The HTTP request succeeds (status 200) but the broker reports that the
//     command failed, with the reason in the response body. Here the status is
//     always 200, so the status code tells us nothing.
//
// Hence, branch on ErrorKind instead of StatusCode.
func buildSEMPv1Message(err *sempv1.Error) string {
	switch err.Kind {
	case sempv1.ErrorKindExecuteFail, sempv1.ErrorKindParse,
		sempv1.ErrorKindPermission, sempv1.ErrorKindLimit:
		// Case 2: a normal "command failed" error (e.g. wrong name, too many
		// clients). The broker's own reason text is useful to the agent, so
		// show it — lightly cleaned up to strip anything sensitive.
		if err.Message != "" {
			msg := fmt.Sprintf("%s error: %s", err.Kind, sanitizeBrokerText(err.Message))
			if err.Kind == sempv1.ErrorKindLimit {
				msg += ". Reduce the scope of the request."
			}
			return msg
		}
		return fmt.Sprintf("%s error (status=%d)", err.Kind, err.StatusCode)

	case sempv1.ErrorKindHTTP:
		// Case 1: the HTTP request failed. There's no useful message here, just
		// a raw body we don't show. This differs from SEMPv2 as we don't parse
		// the SEMPv1 raw body in client.go.
		// Note that a 503 means "try again later", not "something broke", so show
		// a transient message. For 500 and other server errors, display a generic
		// message so we don't leak internal detail.
		if err.StatusCode == 503 {
			return serviceUnavailableMessage
		}
		if err.StatusCode >= 500 {
			return genericInternalMessage
		}
		return fmt.Sprintf("HTTP %d error", err.StatusCode)

	default:
		// We couldn't make sense of the response so return a generic message.
		return genericInternalMessage
	}
}

// brokerTextMayBeShown reports whether the broker's own description is safe to
// pass to the agent for this status.
//
// 500-class responses can carry internal detail — stack context, hostnames,
// component names — so their text is replaced with a generic message. 503 is
// the deliberate exception: it carries a safe and operationally useful reason
// (e.g. "VPN 'X' is busy reconciling", "Replication Is Standby").
//
// This is the single definition of that rule. Every path that echoes broker
// text to the agent must go through it — the rule was previously restated at
// each site, and a new path that forgot it leaked 5xx detail (caught in review
// on #219).
func brokerTextMayBeShown(status int) bool {
	return status < 500 || status == 503
}

func buildSEMPv2Message(err *sempv2.SEMPError) string {
	var msg string
	switch {
	case !brokerTextMayBeShown(err.StatusCode):
		msg = genericInternalMessage
	case err.Description != "":
		msg = sanitizeBrokerText(err.Description)
	default:
		msg = fmt.Sprintf("%s returned HTTP %d", err.Operation, err.StatusCode)
	}
	return msg
}

// isRetryable returns true for errors that represent transient conditions where
// the same request might succeed later: a request shed at admission because the
// broker was too busy (never sent, so always safe to repeat), exhausted
// internal retries (the resilience layer only exhausts on genuinely transient
// HTTP statuses, e.g. 429/503), a live HTTP 429 or 503, or a transient comRc_t
// code (229 TIME_OUT). All other SEMP/envelope errors are deterministic and
// non-retryable.
func isRetryable(err error) bool {
	if errors.Is(err, tokenexchange.ErrExchangeTransport) {
		return true
	}
	// A shed request never reached the broker, so retrying it is safe
	// unconditionally — including for a write. This is the one retryable case
	// with no idempotency caveat at all.
	var busyErr *resilience.BrokerBusyError
	if errors.As(err, &busyErr) {
		return true
	}
	var retriesErr *resilience.RetriesExhaustedError
	if errors.As(err, &retriesErr) {
		// A request the caller declared non-idempotent was not replayed at all,
		// because the broker may already have carried it out. Reporting it as
		// retryable would invite the agent to repeat the very side effect the
		// retry policy refused to duplicate.
		return !retriesErr.NonIdempotent
	}
	var sempv2Err *sempv2.SEMPError
	if errors.As(err, &sempv2Err) {
		return sempv2Err.StatusCode == 429 || sempv2Err.StatusCode == 503 || translatedErrorCodes[sempv2Err.SEMPCode].retryable
	}
	var sempv1Err *sempv1.Error
	if errors.As(err, &sempv1Err) {
		return sempv1Err.StatusCode == 429 || sempv1Err.StatusCode == 503 || translatedErrorCodes[sempv1Err.ReasonCode].retryable
	}
	return false
}

// sanitizeBrokerText is a defense-in-depth pass on the client pass-through
// message only. SEMP descriptions are not expected to carry IPs or filesystem
// paths (verified against the broker source), so this strips only the two
// patterns that could leak internal detail while leaving SEMP object paths intact
func sanitizeBrokerText(s string) string {
	s = fsPrefixPattern.ReplaceAllString(s, "[path]")
	// IPv6 before IPv4 so the bracketed form is replaced as a whole.
	s = ipv6Pattern.ReplaceAllString(s, "[ip]")
	s = ipv4Pattern.ReplaceAllString(s, "[ip]")
	return s
}
