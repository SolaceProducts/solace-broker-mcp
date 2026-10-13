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
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// ownerValidationSpec names, for one create/update composite tool, the
// parameter holding its config object, a human name for the object it
// creates/updates, and whether it's an update — the last of these matters
// because omitting "owner" means something different for each: a create
// gets the broker's default (owner=""), an update leaves the object's
// current, already-stored owner untouched. Both used only in the rejection
// message.
type ownerValidationSpec struct {
	configParam string
	objectKind  string
	isUpdate    bool
}

// ownerValidatedTools names every write tool whose config object accepts an
// "owner" attribute — a bare client-username string that SEMP itself never
// checks for existence (SOL-153080). Confirmed against the embedded SEMP
// config spec: MsgVpnQueue and MsgVpnTopicEndpoint both carry "owner" +
// "permission"; MsgVpnRestDeliveryPoint and every other write tool's object
// do not, so create-rdp and friends are correctly absent here. Update tools
// carry the exact same risk as their create counterparts — a caller can PATCH
// an existing object's owner to a nonexistent username just as easily as
// setting it at creation.
//
// This is a manually-curated list, not derived structurally, because
// "accepts an owner attribute" isn't something the composite tool definition
// exposes — it's a fact about the target SEMP object, not about the tool's
// shape. Mirrors the writeToolIdentifierFields map's same manual-list
// tradeoff in composite_handler.go. TestOwnerValidatedTools_MatchesBodyFields
// cross-checks this list against the embedded catalog's own BodyFields for
// every write tool's step operation, so a future tool or SEMP spec bump that
// adds "owner" to a new object doesn't silently ship unprotected.
var ownerValidatedTools = map[string]ownerValidationSpec{
	"create-queue":          {configParam: "queueConfig", objectKind: "queue", isUpdate: false},
	"update-queue":          {configParam: "queueConfig", objectKind: "queue", isUpdate: true},
	"create-topic-endpoint": {configParam: "topicEndpointConfig", objectKind: "topic endpoint", isUpdate: false},
	"update-topic-endpoint": {configParam: "topicEndpointConfig", objectKind: "topic endpoint", isUpdate: true},
}

// getClientUsernameOperationID is the same read-only operation the
// get-client-username tool exposes, reused here as a pre-flight check rather
// than declared as a second composite step: the composite engine has no
// conditional steps, and every write tool today is deliberately single-step
// (see docs/internal/architecture.md's fail-fast/no-compensation rationale) —
// adding a real conditional-step feature just for this one check would be a
// bigger, riskier change than a small wrapping handler needs to be.
const getClientUsernameOperationID = "monitor/getMsgVpnClientUsername"

// getMsgVpnOperationID is the same read-only operation get-vpn-status uses
// to resolve a single VPN. Reused here strictly to disambiguate a NOT_FOUND
// from getClientUsernameOperationID (see the comment in Handle): SEMP
// returns the byte-identical NOT_FOUND/SEMPCode 6 for "clientUsername
// doesn't exist" and for "msgVpnName itself doesn't exist" — confirmed live
// against a broker — so a missing VPN cannot be told apart from a missing
// owner using the first response alone.
const getMsgVpnOperationID = "monitor/getMsgVpn"

// ownerNotFoundError reports that a create/update tool's config object named
// an "owner" client username SEMP has no record of in the target VPN.
// Constructed only by ownerValidatingHandler's own pre-flight check, so, like
// resilience.BrokerBusyError and tokenexchange.ExchangeError, its message is
// vouched-for by this package and shown to the agent verbatim by
// buildErrorMessage instead of being suppressed behind the generic
// 500-class message.
type ownerNotFoundError struct {
	owner      string
	msgVpn     string
	objectKind string
	isUpdate   bool
}

func (e *ownerNotFoundError) Error() string {
	return fmt.Sprintf("client username %q does not exist in Message VPN %q", e.owner, e.msgVpn)
}

// ownerLogMaxRunes is the most runes truncateOwnerLogValue returns. It bounds
// the owner and VPN values LogAttrs writes, both caller-supplied and neither
// length-limited upstream. 200 is just above the broker's own 189-character
// client-username limit, so no legitimate value is ever cut.
//
// It does NOT bound ownerCheckFailedError.Error(), which %q-formats the
// untruncated owner and VPN into "detail" when the wrapped cause is a vouched
// type. That predates these attributes and is left as it was, so on that path
// the line can carry the value twice: once bounded here, once as before.
const ownerLogMaxRunes = 200

// truncateOwnerLogValue returns s unchanged if it is at most ownerLogMaxRunes
// runes, and otherwise its first ownerLogMaxRunes-1 runes followed by "…", so
// the result never exceeds the cap and says it was cut. It cuts by rune, never
// by byte, so a multi-byte character is never split. Same rule as
// tokenexchange.truncateRunes, which is unexported and so cannot be shared.
func truncateOwnerLogValue(s string) string {
	r := []rune(s)
	if len(r) <= ownerLogMaxRunes {
		return s
	}
	return string(r[:ownerLogMaxRunes-1]) + "…"
}

// LogAttrs returns structured slog attributes for this error, called by
// logToolResult — field ownership stays with the type, like
// tokenexchange.ExchangeError.LogAttrs. This type is on no vouched-type gate,
// so without these the "tool invoked" line reports only the Go type name and an
// operator cannot see which owner or VPN a rejection concerned, nor tell this
// control apart from any other execution_error. Keys mirror the structured
// fields buildErrorResult returns to the agent (error_source, owner, objectKind,
// msgVpnName), in the log's snake_case. That list is written out here and in
// errors.go; TestOwnerErrorLogKeys_MirrorAgentFacingFields fails if the two drift.
func (e *ownerNotFoundError) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("error_source", "owner_validation"),
		slog.String("owner", truncateOwnerLogValue(e.owner)),
		slog.String("msg_vpn", truncateOwnerLogValue(e.msgVpn)),
		slog.String("object_kind", e.objectKind),
	}
}

// ownerCheckStage names which of ownerValidatingHandler's two pre-flight
// reads produced an ownerCheckFailedError — the two have different causes
// and different fixes, so collapsing them into one message loses exactly
// the distinction an operator triaging a spike of failures needs.
type ownerCheckStage string

const (
	ownerCheckStageClientUsername    ownerCheckStage = "client-username-read"
	ownerCheckStageVpnDisambiguation ownerCheckStage = "vpn-disambiguation-read"
)

// ownerCheckFailedError reports that ownerValidatingHandler's pre-flight
// existence check itself could not be completed — a transient network
// error, timeout, rate limit, or 5xx from either the client-username or the
// VPN-disambiguation read — as opposed to ownerNotFoundError, which means
// the check completed and the owner is confirmed absent.
//
// It wraps the underlying cause via Unwrap, so errors.As still finds a
// *sempv2.SEMPError, *resilience.BrokerBusyError, etc. inside it exactly as
// if this wrapper weren't there — buildErrorMessage's and isRetryable's
// existing cases for those types keep classifying status/retryability from
// the real failure. What this type adds, in buildErrorMessage's own case for
// it, is the one thing none of those underlying messages say on their own:
// that this was a pre-flight read for a write tool and the write never ran,
// which matters here exactly as much as it does for
// resilience.BrokerBusyError's "Nothing was changed on the broker" and
// ownerNotFoundError's "so this queue was not created or updated" — without
// it, a caller facing e.g. a bare 503 on the pre-flight read sees only
// "getMsgVpnClientUsername returned HTTP 503" with no indication the write
// itself was never attempted.
//
// owner, msgVpn, and stage are carried separately from cause (rather than
// leaving Error() to render cause's own text) because Error()'s output is
// what logToolResult writes to the server-side "detail" field
// (manager.go's own comment on that field) — without them, that field
// collapses both call sites in Handle to byte-identical text, and an
// operator triaging a spike of pre-flight failures cannot tell which owner
// or VPN was involved, nor which of the two reads is the one failing. Both
// values are caller-supplied input already present in the hashed audit
// args, so carrying them here discloses nothing new.
type ownerCheckFailedError struct {
	cause      error
	objectKind string
	owner      string
	msgVpn     string
	stage      ownerCheckStage
}

func (e *ownerCheckFailedError) Error() string {
	return fmt.Sprintf("checking owner client username %q in Message VPN %q (%s): %v", e.owner, e.msgVpn, e.stage, e.cause)
}

func (e *ownerCheckFailedError) Unwrap() error { return e.cause }

// LogAttrs returns structured slog attributes for this error, called by
// logToolResult. Only fields this package wrote are emitted — never cause's
// text: cause is whatever sempv2.Client.Execute returned, which can be an
// unvouched transport or auth wrap error, and the vouched-type gate on "detail"
// exists to keep exactly that out of the log.
//
// error_stage, not error_source, mirrors buildErrorResult (errors.go), where
// the same wrapper is layered on top of a switch that may already have set
// error_source from the wrapped cause (load_shed, token_exchange); overwriting
// it there would discard a correct classification. Nothing on the log line sets
// error_source from a cause, so this is for parity, not collision avoidance: an
// operator correlating a log line with the agent-facing result sees the same
// names. A vouched cause's own attributes (http_status, semp_code, ...) are
// emitted by logToolResult alongside these.
func (e *ownerCheckFailedError) LogAttrs() []slog.Attr {
	return []slog.Attr{
		slog.String("error_stage", "owner_validation_check"),
		slog.String("check_stage", string(e.stage)),
		slog.String("owner", truncateOwnerLogValue(e.owner)),
		slog.String("msg_vpn", truncateOwnerLogValue(e.msgVpn)),
		slog.String("object_kind", e.objectKind),
	}
}

// ownerValidatingHandler wraps a create/update composite tool handler to
// reject an "owner" that doesn't name a real, existing client username
// before the wrapped tool's single write call ever runs.
//
// Omitting "owner" entirely passes straight through with no extra broker
// round-trip: that path already produces SEMP's own secure default
// (owner="", permission="no-access" — verified live against a broker,
// SOL-153080), so there is nothing to validate. Only a caller-supplied,
// non-empty "owner" triggers the check.
//
// The check is a read (monitor/getMsgVpnClientUsername), never a write, run
// strictly before the wrapped handler. A rejection here means the wrapped
// handler's Handle is never called at all, so no partial state is possible —
// safe under the composite engine's fail-fast, no-compensation design (that
// design's own risk is specifically about a write step succeeding before a
// later step fails; a read that fails first leaves nothing behind).
//
// Known residual gap: the check and the write are two separate broker
// round-trips with no atomicity, so a client username deleted in the window
// between them would still let the write proceed against an owner that no
// longer exists by the time it lands. This is a limitation of doing the
// check from this MCP server specifically — it is NOT a limitation of SEMP
// itself: the broker already enforces this exact kind of reference
// atomically, in the same request, for other attributes (verified against
// broker source and live — creating a MsgVpnClientUsername with a
// nonexistent aclProfileName or clientProfileName is rejected in that same
// POST with a "does not exist" error; both are modeled with
// SuggestedValuesSempCollectionReference(..., strict=True), whereas the
// queue/topic-endpoint "owner" attribute is modeled with strict=False and
// its only server-side check, Parameter::validateOwner, is syntax-only).
// The broker could close this completely and atomically by validating
// "owner" the same way — that would be the real fix; this handler only
// narrows SOL-153080's window from "always" to "only during a race with a
// concurrent client-username deletion" because a broker-side change is out
// of scope for this repository.
type ownerValidatingHandler struct {
	inner       ToolHandler
	spec        ownerValidationSpec
	getUsername *sempv2.Operation
	getVpn      *sempv2.Operation
}

// newOwnerValidatingHandler wraps inner with the owner-existence check
// described by spec. getUsername and getVpn are the parsed
// monitor/getMsgVpnClientUsername and monitor/getMsgVpn operations from the
// same catalog the wrapped tool's own executor uses.
func newOwnerValidatingHandler(inner ToolHandler, spec ownerValidationSpec, getUsername, getVpn *sempv2.Operation) ToolHandler {
	return &ownerValidatingHandler{inner: inner, spec: spec, getUsername: getUsername, getVpn: getVpn}
}

func (h *ownerValidatingHandler) Metadata() Metadata {
	return h.inner.Metadata()
}

func (h *ownerValidatingHandler) Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
	owner, msgVpn, ok := extractOwner(params, h.spec.configParam)
	if !ok {
		return h.inner.Handle(ctx, tc, params)
	}

	_, err := tc.SEMPv2Client.Execute(ctx, h.getUsername, map[string]any{
		"msgVpnName":     msgVpn,
		"clientUsername": owner,
	})
	if err == nil {
		return h.inner.Handle(ctx, tc, params)
	}

	var sempErr *sempv2.SEMPError
	if !errors.As(err, &sempErr) || !isSEMPStatus(sempErr, "NOT_FOUND", 6) {
		// Any other failure (network, auth, 5xx, rate limiting) means the
		// check was inconclusive, not that the username is confirmed absent.
		// Deny on doubt rather than risk creating exactly the unscoped
		// binding this check exists to prevent; ownerCheckFailedError's own
		// buildErrorMessage case makes sure the caller is told the write
		// never ran, not just shown the bare read failure.
		return nil, &ownerCheckFailedError{
			cause:      err,
			objectKind: h.spec.objectKind,
			owner:      owner,
			msgVpn:     msgVpn,
			stage:      ownerCheckStageClientUsername,
		}
	}

	// SEMP's NOT_FOUND here is ambiguous: a nonexistent msgVpnName produces
	// the exact same code/description as a nonexistent clientUsername
	// (verified live — see getMsgVpnOperationID's doc comment), so this
	// NOT_FOUND alone cannot tell "no such owner" apart from "no such VPN".
	// Resolve it with one more read, only on this already-slow path: if the
	// VPN itself doesn't exist, this was never an owner problem, so fall
	// through and let the wrapped tool's own call report the real, correct
	// "Message VPN does not exist" error instead of a misleading
	// owner-not-found one.
	if _, vpnErr := tc.SEMPv2Client.Execute(ctx, h.getVpn, map[string]any{"msgVpnName": msgVpn}); vpnErr != nil {
		var vpnSempErr *sempv2.SEMPError
		if errors.As(vpnErr, &vpnSempErr) && isSEMPStatus(vpnSempErr, "NOT_FOUND", 6) {
			return h.inner.Handle(ctx, tc, params)
		}
		// Inconclusive for the same reason as above: deny on doubt. Wrap
		// vpnErr, not the original owner-check err — vpnErr is the actual
		// reason disambiguation failed, and the original err is just the
		// ambiguous NOT_FOUND this whole branch exists to not take at face
		// value; surfacing it here would silently reintroduce the exact
		// misleading owner-blame message this fix removes.
		return nil, &ownerCheckFailedError{
			cause:      vpnErr,
			objectKind: h.spec.objectKind,
			owner:      owner,
			msgVpn:     msgVpn,
			stage:      ownerCheckStageVpnDisambiguation,
		}
	}

	return nil, &ownerNotFoundError{owner: owner, msgVpn: msgVpn, objectKind: h.spec.objectKind, isUpdate: h.spec.isUpdate}
}

// extractOwner reads the "owner" attribute the caller supplied for this
// write, and the sibling params["msgVpnName"]. constructRequestBody
// (internal/composite/executor.go) assembles the SEMP request body from the
// raw param map two ways, and "owner" can reach the broker through either:
// a scalar param is set into the body under its OWN name (so a bare
// top-level params["owner"] lands as body["owner"]), while an object-valued
// param has its KEYS spread into the body regardless of that param's own
// name (so params[configParam]["owner"] lands as body["owner"], but so does
// params["literally-anything-else"]["owner"] — constructRequestBody does
// not check the param's name before spreading an object's keys, only that
// each resulting body field is declared on the operation, and "owner" is).
// None of these three locations are declared in any of these tools' input
// schemas beyond configParam itself, but the schema has no
// additionalProperties:false (a deliberate choice, SOL-154164), so nothing
// upstream strips an undeclared key before it reaches here.
//
// This function checks, in this order: nested inside the documented
// configParam object; a bare top-level params["owner"] that is itself a
// string; and finally every other object-valued param's "owner" key — this
// last scan is NOT restricted to params whose name differs from "owner", so
// a param literally named "owner" whose VALUE is itself a map (e.g.
// {"owner": {"owner": "ghost", "permission": "consume"}}) is still caught
// here, by the same scan that catches any other wrong config-object name.
// Only configParam is excluded from this final scan, because its "owner"
// key was already checked above; excluding "owner" too was an earlier,
// incorrect version of this function — the scalar check two lines up only
// matches params["owner"] when it is a string, so a map-valued
// params["owner"] fell through that check unhandled and was then skipped by
// the loop as if it had been "already checked", reopening the exact class
// of bypass this function exists to close. Found and fixed in review: a
// caller (or a confused LLM inventing a plausible-but-wrong config-object
// name — attempts caught in review used "config", "attributes",
// "queueAttributes", and finally "owner" itself) that puts "owner" in any
// of these shapes would otherwise reach the broker with zero validation —
// reproduced live during review, three times now, for three different
// shapes found across three review passes. Check order is not a
// security-relevant choice: if more than one location is set,
// constructRequestBody's own ambiguous-request-body check rejects the call
// before it reaches the broker regardless of which value this function
// picked, so the order only affects which safe error the caller sees, never
// whether the write goes through unchecked.
//
// ok is false when no location has a non-empty string "owner" — that
// includes a non-string value (e.g. {"owner": 123}), which is treated as "no
// owner supplied" here rather than rejected: constructRequestBody spreads it
// into the body regardless, and the broker's own type check on a
// string-typed attribute already rejects it with an ordinary
// INVALID_PARAMETER error, so duplicating that check here would add nothing.
//
// Deliberately scoped to "owner" specifically, not a general fix for the
// underlying looseness in constructRequestBody (any object-valued param's
// keys get spread regardless of that param's declared name — see its own
// doc comment). Review raised fixing that at the executor level instead, so
// the whole class closes at once rather than per sensitive field; this
// function is the narrower, tool-specific mirror it also proposed as a
// fallback if the executor change is judged too broad for this PR. The
// tradeoff: a hypothetical future body field with the same referential
// looseness as "owner" (SEMP has none today outside this one) would need
// its own scan added here, and TestOwnerValidatedTools_MatchesBodyFields
// would give no signal that one was needed, because it only checks for
// "owner" specifically. Accepted for now; revisit at the executor level if
// a second such field ever appears.
func extractOwner(params map[string]any, configParam string) (owner, msgVpn string, ok bool) {
	msgVpn, _ = params["msgVpnName"].(string)
	if cfg, isMap := params[configParam].(map[string]any); isMap {
		if o, isStr := cfg["owner"].(string); isStr && o != "" {
			return o, msgVpn, true
		}
	}
	if o, isStr := params["owner"].(string); isStr && o != "" {
		return o, msgVpn, true
	}
	for name, val := range params {
		if name == configParam {
			continue // its "owner" key is checked above
		}
		obj, isMap := val.(map[string]any)
		if !isMap {
			continue
		}
		if o, isStr := obj["owner"].(string); isStr && o != "" {
			return o, msgVpn, true
		}
	}
	return "", "", false
}

// recordOwnerValidation increments mcp_owner_validation_total (SOL-154679) when
// err says the owner-existence check blocked the write: ownerNotFoundError is a
// confirmed-absent owner, ownerCheckFailedError an inconclusive check denied on
// doubt. Any other error, including a nil one, records nothing.
//
// A check that failed because the caller cancelled (context.Canceled on ctx) is
// not counted: the caller abandoned the call, the check did not block it, and
// counting it would make the series move with how often clients hit stop. A
// timeout (DeadlineExceeded) is a check that could not complete and does count,
// and a confirmed-absent owner counts regardless, since that check completed.
// ctx, not errors.Is on err: the caller's cancellation is what ctx reports,
// whatever the failed read's error chain happens to expose.
//
// This lives in CallTool's error path rather than in ownerValidatingHandler for
// one concrete reason: the handler's params have "broker" stripped
// (stripBrokerParam), so it cannot label the series by broker the way
// mcp_broker_authz_denied_total is, and recording there would also mean
// threading *metrics.ToolMetrics and the tool name into it.
//
// errors.As, not a type switch, so a wrapped error still matches — handleErr
// reaches here raw, but ownerCheckFailedError itself wraps its cause. Metrics
// disabled needs no guard here: RecordOwnerValidation is nil-safe on its
// receiver, the same single guard RecordBrokerAuthzDenied relies on.
func (m *ToolManager) recordOwnerValidation(ctx context.Context, tool, brokerAlias string, err error) {
	var notFound *ownerNotFoundError
	var checkFailed *ownerCheckFailedError
	var outcome metrics.OwnerValidationOutcome
	switch {
	case errors.As(err, &notFound):
		outcome = metrics.OwnerValidationRejectedNotFound
	case errors.As(err, &checkFailed):
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		outcome = metrics.OwnerValidationRejectedCheckFailed
	default:
		return
	}
	m.metrics.RecordOwnerValidation(ctx, tool, canonicalBrokerLabel(m.pool, brokerAlias), outcome)
}

// ownerValidationRejectedReasonNotFound is the only reason an
// owner_validation_rejected record can carry today (SOL-154703): the check
// completed and the supplied owner is absent from the target VPN. audit's own
// ownerValidationRejectedReasons is the real vocabulary and is package-private,
// so this is a local copy; TestEmitOwnerValidationRejectedAudit_ReasonMatchesAuditVocabulary
// pins the two together.
const ownerValidationRejectedReasonNotFound = "owner_not_found"

// isOwnerValidationRejection reports whether err is a confirmed owner rejection:
// the pre-flight check completed and the owner is absent. Deliberately
// ownerNotFoundError only, not ownerCheckFailedError. A check that could not
// complete (a transient 5xx, a timeout, a refused read) is an ordinary failure,
// not a refusal, and recording it as one would be a false signal for the operator
// alerting on this record. (A check whose read was refused by the broker with
// SEMP code 72 is already a real hop-2 denial and fires broker_authz_denied; that
// is a different event and is not this one.)
//
// errors.As, so a wrapped error still matches — handleErr reaches CallTool raw,
// but toolErr wraps it in "%w".
func isOwnerValidationRejection(err error) bool {
	var notFound *ownerNotFoundError
	return errors.As(err, &notFound)
}

// emitOwnerValidationRejectedAudit builds and emits the owner_validation_rejected
// audit record for a confirmed owner rejection (SOL-154703). Called
// unconditionally on a match, destructive or not, unlike emitOperationAudit,
// which fires only for a destructive call; the two coexist for a destructive
// update tool rather than one suppressing the other. Mirrors
// emitBrokerAuthzDeniedAudit's shape: tool and broker, a reason, and nothing
// else. The owner and VPN stay out of the record, as the sibling keeps its
// request detail out; when correlation is enabled the correlation ID the record
// carries joins it to the "tool invoked" line, which names both (SOL-155918).
//
// Identity is not passed in, for the same reason as the sibling: audit.NewEvent
// reads the principal from ctx.
func emitOwnerValidationRejectedAudit(ctx context.Context, tool, broker string) {
	event, err := audit.NewEvent(ctx, audit.Fields{
		Type:   audit.EventOwnerValidationRejected,
		Reason: ownerValidationRejectedReasonNotFound,
		Tool:   tool,
		Broker: broker,
	})
	if err != nil {
		// Same reasoning as the sibling's drop path: err.Error() names only
		// field names and closed-vocabulary values, never argument data.
		slog.ErrorContext(ctx, "audit: owner_validation_rejected record rejected by the schema constructor; recording a drop",
			slog.String("tool", tool),
			slog.String("broker", broker),
			slog.String("detail", err.Error()))
		audit.EmitDrop(ctx, audit.DropContext{DroppedEventType: audit.EventOwnerValidationRejected, Tool: tool, Broker: broker})
		return
	}
	audit.Emit(ctx, event)
}
