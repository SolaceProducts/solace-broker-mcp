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
	"strings"
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
//
// 135 (MAX_NUM_EXCEEDED) and 403 (MAX_NUM_SUBSCRIPTIONS_EXCEEDED) deliberately
// stop short of naming which limit was hit or its current count (SOL-153341
// AC4, descoped after review with the ticket owner) — each hint instead says
// plainly that the tool can't tell, so a caller doesn't mistake silence for
// certainty and act on the wrong scope. The two hints differ because the
// broker-source finding differs per code: subscription creation (403) checks
// a broker-wide total and a per-queue total with `||`
// (adQueueCommand.cpp / moTypeHdlrQendpt.cpp), and the broker itself doesn't
// retain which side tripped, so a confident claim there would be a guess
// between broker-wide and per-queue; the other limit families 135 covers
// (queues, endpoints, REST delivery points) are broker-wide totals allocated
// per-VPN, and an accurate broker-wide count has no single SEMP call — it
// would mean enumerating every VPN — so the guess there is between
// broker-wide and per-VPN. Scoping and live counts are tracked as a
// follow-up spike rather than guessed at or built at real per-error cost here.
var translatedErrorCodes = map[int]codeInfo{
	6:   {hint: "Verify the name is correct."},
	10:  {hint: "Update the existing object or choose a new name."},
	11:  {hint: "Check attribute values against the SEMP schema."},
	14:  {hint: "Not supported for this object or broker mode."},
	27:  {hint: "Check the request/query syntax."},
	72:  {hint: "Credentials lack permission; check the management role/VPN scope."},
	89:  {hint: "Not allowed in the object's current state."},
	135: {hint: "A configured maximum was reached; this tool can't tell whether the limit is broker-wide or per-VPN."},
	228: {hint: "Supply all required fields for this object."},
	229: {hint: "Retry shortly.", retryable: true},
	256: {hint: "Drain or remove contained objects before deleting."},
	403: {hint: "A configured maximum was reached; this tool can't tell whether the limit is broker-wide or per-queue."}, // MAX_NUM_SUBSCRIPTIONS_EXCEEDED
}

// errorTypeNoopExists and errorTypeNoopAbsent are the errorType values
// CallTool records for the two classifyDesiredStateOutcome cases (SOL-153341),
// so logToolResult can log them at INFO instead of falling into the generic
// ERROR branch every other toolErr takes.
const (
	errorTypeNoopExists = "noop_already_exists"
	errorTypeNoopAbsent = "noop_already_absent"
)

// parentModePattern matches the broker's two "Cannot enter X mode: not
// found" message shapes, both confirmed live against a real broker
// (SOL-153341): "Cannot enter mode for message-vpn X: not found." (names the
// instance — capture "modeNamed" + "instance") and "Cannot enter queue mode:
// not found." (names only the mode word — capture "modeBare", no instance).
// The two alternatives are structurally distinct ("mode for X Y" vs. "X
// mode") specifically so a literal "mode" can never be misread as an
// instance name by the wrong branch.
//
// Which shape a given broker command uses is NOT determined by the parent's
// object type: create-queue and create-topic-endpoint against a missing VPN
// both use the first shape, but create-rdp against the very same missing
// VPN uses the second — so both alternatives must always be tried for every
// parent type, never dispatched on the mode word first.
var parentModePattern = regexp.MustCompile(`^Problem with \w+: Cannot enter (?:mode for (?P<modeNamed>[a-z-]+) (?P<instance>.+)|(?P<modeBare>[a-z-]+) mode): not found\.$`)

// parentModeFriendlyNames translates the CLI "mode" word captured by
// parentModePattern to the human-readable object type AC3 asks for. Every
// entry here has been seen live in this shape; a mode word not in this map
// falls through to showing the broker's raw text unchanged (see
// buildSEMPv2Message) rather than guessing a translation.
//
// "topic-endpoint" is deliberately absent: no tool in this catalog has
// topicEndpointName as a non-final path segment for anything, so this mode
// word may be unreachable with today's tool set. Add it once it's actually
// seen, not preemptively.
var parentModeFriendlyNames = map[string]string{
	"queue":               "the queue",
	"message-vpn":         "the Message VPN",
	"rest-delivery-point": "the REST Delivery Point",
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

// desiredStateOutcome describes a write-tool error that SEMP reported as a
// hard failure, but that already reflects the caller's desired end state —
// an idempotent create/delete replay (SOL-153341). ALREADY_EXISTS on a
// create and NOT_FOUND on a delete both mean the desired state already
// holds: for an idempotent workflow that is success, not failure, and an
// agent that treats it as a hard error has no way to tell "already done"
// from "actually broken" without a separate read to reconcile.
type desiredStateOutcome struct {
	Outcome string // "exists_unchanged" | "already_absent"
	Message string

	// SEMPCode, SEMPStatus, Operation, and Detail mirror the underlying SEMP
	// error's audit fields. manager.go's CallTool clears toolErr back to nil
	// for this outcome — a non-nil toolErr means "the call failed" to every
	// other consumer in this codebase, including SOL-152086's per-tool
	// metrics, which infer outcome=error from exactly that signal — so
	// logToolResult reads these fields from here instead of re-deriving them
	// from toolErr.
	SEMPCode   int
	SEMPStatus string
	Operation  string
	Detail     string
}

// classifyDesiredStateOutcome reports whether err is a SEMP error that,
// despite being a hard failure at the protocol level, already reflects the
// caller's desired state: ALREADY_EXISTS on a create-prefixed operation, or
// NOT_FOUND on a delete-prefixed one. Returns nil for anything else,
// including NOT_FOUND on a read (get/list) operation — a missing parent on
// a read is a real error the caller needs to see, not a desired-state noop.
//
// Every SEMP write operationId in this catalog follows a fixed
// create*/update*/delete*/do* naming convention (verified against every
// operation referenced in tools.yaml), so this needs no per-tool metadata —
// it classifies purely from the operation ID prefix and the broker's own
// SEMPStatus/SEMPCode, and a new create-*/delete-* tool inherits the
// classification automatically with no tools.yaml change.
//
// Matches SEMPStatus first (already-parsed, self-documenting); falls back
// to the pinned SEMPCode (6=NOT_FOUND, 10=ALREADY_EXISTS — see
// TestCallTool_SEMPErrorWrapped and executor_vpn_test.go's
// TestExecute_CreateMessageVPN_AlreadyExists) in case a broker version ever
// omits the status string.
//
// Deliberately does not verify the existing/absent object's attributes
// match the request before classifying — the ticket's own Proposal section
// never mentions an attribute diff, only the two SEMP status codes, and
// adding one would cost a round-trip this ticket's "close to zero per-tool
// work" framing rules out. Revisit if that reading turns out wrong.
func classifyDesiredStateOutcome(err error) *desiredStateOutcome {
	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) {
		return nil
	}
	switch {
	case isSEMPStatus(sempErr, "ALREADY_EXISTS", 10) && strings.HasPrefix(sempErr.Operation, "create"):
		return &desiredStateOutcome{
			Outcome:    "exists_unchanged",
			Message:    buildSEMPv2Message(sempErr),
			SEMPCode:   sempErr.SEMPCode,
			SEMPStatus: sempErr.SEMPStatus,
			Operation:  sempErr.Operation,
			Detail:     sempErr.Error(),
		}
	case isSEMPStatus(sempErr, "NOT_FOUND", 6) && strings.HasPrefix(sempErr.Operation, "delete"):
		return &desiredStateOutcome{
			Outcome:    "already_absent",
			Message:    buildSEMPv2Message(sempErr),
			SEMPCode:   sempErr.SEMPCode,
			SEMPStatus: sempErr.SEMPStatus,
			Operation:  sempErr.Operation,
			Detail:     sempErr.Error(),
		}
	}
	return nil
}

// isSEMPStatus reports whether err's SEMPStatus (or, when the broker omitted
// that string, its SEMPCode) matches the given category.
func isSEMPStatus(err *sempv2.SEMPError, status string, code int) bool {
	if err.SEMPStatus != "" {
		return err.SEMPStatus == status
	}
	return err.SEMPCode == code
}

// buildDesiredStateResult converts a desiredStateOutcome into a non-error
// MCP result (SOL-153341). IsError is false — this is the actual mechanism
// behind "reported as a non-failure": an agent branching on IsError sees a
// completed step, while the outcome/changed fields still let it report
// accurately that nothing new happened rather than claiming a fresh success.
func buildDesiredStateResult(outcome *desiredStateOutcome) *mcp.CallToolResult {
	structured := map[string]any{
		"outcome": outcome.Outcome,
		"changed": false,
		"message": outcome.Message,
	}
	return &mcp.CallToolResult{
		StructuredContent: structured,
		Content:           []mcp.Content{&mcp.TextContent{Text: outcome.Message}},
		IsError:           false,
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
	if !brokerTextMayBeShown(err.StatusCode) {
		return genericInternalMessage
	}
	// Parent-naming (SOL-153341, AC3): on a create, NOT_FOUND means a path
	// parent is missing — the object being created never exists yet, so it
	// cannot be what NOT_FOUND refers to. Translate the broker's CLI-mode
	// jargon into plain language before falling back to its raw text. Not
	// gated to any particular tool: any create-prefixed operation's NOT_FOUND
	// gets the same treatment, tool-agnostic like the rest of this
	// classification.
	if isSEMPStatus(err, "NOT_FOUND", 6) && strings.HasPrefix(err.Operation, "create") {
		if translated := translateParentNotFound(err.Description); translated != "" {
			return translated
		}
	}
	if err.Description != "" {
		return sanitizeBrokerText(err.Description)
	}
	return fmt.Sprintf("%s returned HTTP %d", err.Operation, err.StatusCode)
}

// translateParentNotFound rewrites the broker's "Cannot enter X mode: not
// found" wording (see parentModePattern) into a message that states the
// missing parent's type in plain language, instead of relaying CLI-mode
// jargon an agent would otherwise have to pass on to a user verbatim.
// Returns "" when description doesn't match either known shape, or names a
// mode word this codebase hasn't seen live yet (parentModeFriendlyNames) —
// either way the caller falls back to the broker's raw (sanitized) text
// unchanged rather than guessing a translation.
func translateParentNotFound(description string) string {
	match := parentModePattern.FindStringSubmatch(description)
	if match == nil {
		return ""
	}
	var modeWord, instance string
	for i, name := range parentModePattern.SubexpNames() {
		switch name {
		case "modeNamed", "modeBare":
			if match[i] != "" {
				modeWord = match[i]
			}
		case "instance":
			instance = match[i]
		}
	}
	friendly, ok := parentModeFriendlyNames[modeWord]
	if !ok {
		return ""
	}
	capitalized := strings.ToUpper(friendly[:1]) + friendly[1:]
	if instance != "" {
		// Routed through sanitizeBrokerText like every other broker-derived
		// agent-facing string in this file, even though instance is, today,
		// just an echo of the caller-supplied name — not broker-internal
		// data. Keeps this function from being a silent exception to that
		// otherwise-universal invariant if that ever stops being true.
		return fmt.Sprintf("%s %q does not exist.", capitalized, sanitizeBrokerText(instance))
	}
	return fmt.Sprintf("%s does not exist.", capitalized)
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
