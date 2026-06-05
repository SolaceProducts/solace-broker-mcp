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
	"log/slog"
	"regexp"

	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv1"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/sempv2"
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
	summary string // short label used when the broker gives no description
	hint    string // generic actionable hint surfaced to the agent
	// retryable marks transient codes where the same request might succeed on
	// retry (HTTP 503 and exhausted internal retries are handled separately).
	retryable bool
}

// translatedErrorCodes maps the comRc_t integer to a short summary and a generic hint.
// Note that the comRc_t code corresponds to the SEMPv2 error.code / SEMPv1 reasonCode.
var translatedErrorCodes = map[int]codeInfo{
	6:   {summary: "Object not found", hint: "Verify the name is correct."},
	10:  {summary: "Object already exists", hint: "Update the existing object or choose a new name."},
	11:  {summary: "Invalid parameter", hint: "Check attribute values against the SEMP schema."},
	14:  {summary: "Operation not supported", hint: "Not supported for this object or broker mode."},
	27:  {summary: "Request parse error", hint: "Check the request/query syntax."},
	72:  {summary: "Unauthorized", hint: "Credentials lack permission; check the management role/VPN scope."},
	89:  {summary: "Operation not allowed", hint: "Not allowed in the object's current state."},
	135: {summary: "Maximum exceeded", hint: "A configured maximum was reached."},
	228: {summary: "Missing required parameter", hint: "Supply all required fields for this object."},
	229: {summary: "Operation timed out", hint: "Retry shortly.", retryable: true},
	256: {summary: "Object not empty", hint: "Drain or remove contained objects before deleting."},
}

// fsPrefixPattern matches filesystem paths by their leading prefix only, so
// SEMP object paths (e.g. /msgVpns/foo/queues/bar) — which have none of these
// prefixes — are preserved. This is the load-bearing constraint of the
// sanitizer.
var fsPrefixPattern = regexp.MustCompile(`(?i)(/opt/|/var/|/usr/|/etc/|[A-Za-z]:\\)\S*`)

// ipv4Pattern matches dotted-quad IPv4 addresses.
var ipv4Pattern = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)

// ipv6Pattern matches the broker's bracket-wrapped IPv6 form (e.g. "[2001:db8::1]").
// The broker brackets IPv6 and leaves IPv4 bare, so only the bracketed shape is
// needed here. The required ':' avoids matching plain bracketed hex tokens.
var ipv6Pattern = regexp.MustCompile(`\[[0-9a-fA-F:]*:[0-9a-fA-F:]*\]`)

// buildErrorResult converts a tool execution error into an MCP-compliant
// CallToolResult with IsError: true. Per the MCP spec, tool execution errors
// should be returned as results (not protocol-level errors) so the LLM can see
// them and self-correct. StructuredContent carries machine-readable fields
// (retryable, status, protocol-specific data, plus a suggestions array);
// Content carries a human-readable text message.
//
// The full unsuppressed/unsanitized error is logged server-side here
// and never returned to the agent.
func (m *ToolManager) buildErrorResult(toolName string, err error) *mcp.CallToolResult {
	msg, suggestions := buildErrorMessage(err)
	retryable := isRetryable(err)

	structured := map[string]any{
		"error":     msg,
		"retryable": retryable,
	}

	// status and code are also captured for the server-side log line below.
	var status, code int

	var sempv2Err *sempv2.SEMPError
	var sempv1Err *sempv1.Error
	var retriesErr *resilience.RetriesExhaustedError

	switch {
	case errors.As(err, &sempv2Err):
		status, code = sempv2Err.StatusCode, sempv2Err.SEMPCode
		structured["status"] = sempv2Err.StatusCode
		structured["operation"] = sempv2Err.Operation
		if sempv2Err.SEMPStatus != "" {
			structured["sempStatus"] = sempv2Err.SEMPStatus
		}
		if sempv2Err.SEMPCode != 0 {
			structured["sempCode"] = sempv2Err.SEMPCode
		}
	case errors.As(err, &sempv1Err):
		status, code = sempv1Err.StatusCode, sempv1Err.ReasonCode
		structured["status"] = sempv1Err.StatusCode
		structured["kind"] = sempv1Err.Kind.String()
		if sempv1Err.ReasonCode != 0 {
			structured["reasonCode"] = sempv1Err.ReasonCode
		}
	case errors.As(err, &retriesErr):
		status = retriesErr.StatusCode
		if retriesErr.StatusCode != 0 {
			structured["status"] = retriesErr.StatusCode
		}
		structured["attempts"] = retriesErr.Attempts
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

	// Log the full, unsanitized error server-side for debugging. The agent only
	// ever sees the sanitized message above; this log line is the one place the
	// raw detail is kept so operators can diagnose failures.
	//
	// Note: "detail" is a raw external error string, so ReplaceAttr (which keys
	// off field names) cannot scrub it. This is acceptable because our SEMP/HTTP
	// errors carry no credentials — auth is applied via headers, not URLs, and
	// the error text is broker-generated. Keep credentials out of these error
	// types so this stays safe.
	slog.Info("tool error detail",
		slog.String("tool", toolName),
		slog.Int("http_status", status),
		slog.Int("semp_code", code),
		slog.String("detail", err.Error()))

	return &mcp.CallToolResult{
		StructuredContent: structured,
		Content:           []mcp.Content{&mcp.TextContent{Text: contentText}},
		IsError:           true,
	}
}

// buildErrorMessage produces the human-readable, agent-facing error string
// along with any actionable suggestions. It prefers the broker's own
// description for client/config errors (lightly sanitized as defense-in-depth),
// but suppresses 500-class internal detail behind a generic message so it never
// reaches the agent.
//
// Suggestions are the curated generic hint for the broker's comRc_t code.
// Server-suppressed (5xx, except 503) errors get no object-level guidance.
func buildErrorMessage(err error) (string, []string) {
	var retriesErr *resilience.RetriesExhaustedError
	var sempv2Err *sempv2.SEMPError
	var sempv1Err *sempv1.Error

	var message string
	var status, code int // broker HTTP status and comRc_t code, for suggestions

	switch {
	case errors.As(err, &retriesErr):
		// The underlying network error can contain a host or IP address, so
		// don't show it. Just report how many attempts were made and the
		// status. Retry-exhaustion carries no object-level suggestions.
		return fmt.Sprintf(
			"Request failed after %d attempts (HTTP %d). Internal retries exhausted; try again later.",
			retriesErr.Attempts, retriesErr.StatusCode), nil

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
	if status >= 500 && status != 503 {
		return message, nil
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

func buildSEMPv2Message(err *sempv2.SEMPError) string {
	// For 500 and other server errors, display a generic message so we don't
	// leak internal detail. 503 is the exception: it carries a safe, useful
	// reason (e.g. "VPN 'X' is busy reconciling", "Replication Is Standby"),
	// so we let it through.
	var msg string
	switch {
	case err.StatusCode >= 500 && err.StatusCode != 503:
		msg = genericInternalMessage
	case err.Description != "":
		msg = sanitizeBrokerText(err.Description)
	default:
		msg = fmt.Sprintf("%s returned HTTP %d", err.Operation, err.StatusCode)
	}
	return msg
}

// isRetryable returns true for errors that represent transient conditions where
// the same request might succeed later: exhausted internal retries (the
// resilience layer only exhausts on genuinely transient statuses), an HTTP 503
// or a transient comRc_t code (229 TIME_OUT) from the broker. All other SEMP/envelope
// errors are deterministic and non-retryable.
func isRetryable(err error) bool {
	var retriesErr *resilience.RetriesExhaustedError
	if errors.As(err, &retriesErr) {
		return true
	}
	var sempv2Err *sempv2.SEMPError
	if errors.As(err, &sempv2Err) {
		return sempv2Err.StatusCode == 503 || translatedErrorCodes[sempv2Err.SEMPCode].retryable
	}
	var sempv1Err *sempv1.Error
	if errors.As(err, &sempv1Err) {
		return sempv1Err.StatusCode == 503 || translatedErrorCodes[sempv1Err.ReasonCode].retryable
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
