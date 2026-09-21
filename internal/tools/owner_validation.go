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
type ownerCheckFailedError struct {
	cause      error
	objectKind string
}

func (e *ownerCheckFailedError) Error() string {
	return fmt.Sprintf("checking whether the owner client username exists: %v", e.cause)
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
		return nil, &ownerCheckFailedError{cause: err, objectKind: h.spec.objectKind}
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
		return nil, &ownerCheckFailedError{cause: vpnErr, objectKind: h.spec.objectKind}
	}

	return nil, &ownerNotFoundError{owner: owner, msgVpn: msgVpn, objectKind: h.spec.objectKind, isUpdate: h.spec.isUpdate}
}

// extractOwner reads the "owner" attribute the caller supplied for this
// write, and the sibling params["msgVpnName"]. "owner" can arrive two ways:
// nested inside the tool's documented config object
// (params[configParam]["owner"]), or as a bare top-level params["owner"] —
// undeclared by any of these tools' input schemas, but the schema has no
// additionalProperties:false (a deliberate choice, SOL-154164), and
// constructRequestBody (internal/composite/executor.go) spreads ANY
// top-level scalar param whose name is a known SEMP body field straight
// into the request body, "owner" among them. A caller (or a confused LLM)
// who puts "owner" there instead of inside the config object would
// otherwise reach the broker with zero validation — reproduced live during
// review. Checking the nested location first is not a security-relevant
// ordering choice: if both are set, constructRequestBody's own
// ambiguous-request-body check rejects the call before it reaches the
// broker regardless of which value this function picked, so the ordering
// only affects which safe error the caller sees, never whether the write
// goes through unchecked.
//
// ok is false when neither location has a non-empty string "owner" — that
// includes a non-string value (e.g. {"owner": 123}), which is treated as "no
// owner supplied" here rather than rejected: constructRequestBody spreads it
// into the body regardless, and the broker's own type check on a
// string-typed attribute already rejects it with an ordinary
// INVALID_PARAMETER error, so duplicating that check here would add nothing.
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
	return "", "", false
}
