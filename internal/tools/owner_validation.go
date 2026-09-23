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
