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
// parameter holding its config object and a human name for the object it
// creates/updates (used only in the rejection message).
type ownerValidationSpec struct {
	configParam string
	objectKind  string
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
// tradeoff in composite_handler.go.
var ownerValidatedTools = map[string]ownerValidationSpec{
	"create-queue":          {configParam: "queueConfig", objectKind: "queue"},
	"update-queue":          {configParam: "queueConfig", objectKind: "queue"},
	"create-topic-endpoint": {configParam: "topicEndpointConfig", objectKind: "topic endpoint"},
	"update-topic-endpoint": {configParam: "topicEndpointConfig", objectKind: "topic endpoint"},
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
}

func (e *ownerNotFoundError) Error() string {
	return fmt.Sprintf("client username %q does not exist in Message VPN %q", e.owner, e.msgVpn)
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
		// binding this check exists to prevent; the existing SEMPv2 error
		// translation (buildErrorMessage) already gives this a sensible,
		// non-generic agent-facing message.
		return nil, fmt.Errorf("checking owner client username %q in VPN %q: %w", owner, msgVpn, err)
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
		// Inconclusive for the same reason as above: deny on doubt.
		return nil, fmt.Errorf("checking owner client username %q in VPN %q: %w", owner, msgVpn, err)
	}

	return nil, &ownerNotFoundError{owner: owner, msgVpn: msgVpn, objectKind: h.spec.objectKind}
}

// extractOwner reads params[configParam]["owner"] and the sibling
// params["msgVpnName"]. ok is false when owner is absent or empty — the
// caller should skip validation and pass the call through unchanged.
func extractOwner(params map[string]any, configParam string) (owner, msgVpn string, ok bool) {
	msgVpn, _ = params["msgVpnName"].(string)
	cfg, _ := params[configParam].(map[string]any)
	if cfg == nil {
		return "", "", false
	}
	owner, _ = cfg["owner"].(string)
	if owner == "" {
		return "", "", false
	}
	return owner, msgVpn, true
}
