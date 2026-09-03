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
	"log/slog"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/authz"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/logging/sanitize"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// methodToolsList is the JSON-RPC method this filter narrows. The SDK's own
// constant (methodListTools in mcp/protocol.go) is unexported, so it is
// duplicated here rather than imported.
const methodToolsList = "tools/list"

// eventToolListFilter tags every record this file emits.
//
// The call path's "tool authorization" records share this file's
// decision_reason vocabulary, so without an explicit tag an operator filtering
// on a reason would get both paths interleaved with no field saying which is
// which. Present tense, not "filtered": the event is emitted whether or not
// anything was removed, and decision_reason carries that.
//
// Deliberately not named for authorization. Listing is visibility; the
// authorization boundary is tools/call.
const eventToolListFilter = "tool_list_filter"

// decision_reason values. The vocabulary matches withAuthorization's so an
// operator correlating a tools/list against a later tools/call reads one set
// of terms.
const (
	listReasonFiltered     = "filtered"
	listReasonNotPermitted = "not_permitted"
	listReasonMissingClaim = "missing_claim"
	listReasonUnfiltered   = "unfiltered"
)

// WithListFiltering narrows tools/list to the tools the caller may invoke,
// using the same policy.Authorize predicate that gates tools/call.
//
// Discovery hygiene, not an access control: an unlisted tool is still callable
// by name, because the SDK resolves callTool against the global tool set. This
// only stops a caller's agent from spending context on tools it will be denied.
//
// Because listed-implies-callable is the point, the verdict here must stay
// identical to withAuthorization's — hence the shared requestGroups and
// policy.Authorize rather than a second implementation.
//
// Stateless by requirement: the 2026-07-28 spec says the tool set "MUST NOT
// vary per-connection" but MAY vary by "the authorization presented on the
// request", so this reads only the current token.
//
// Exempt tools (IsExemptFromToolAuthorization) are registered without a policy,
// so Policy.Authorize returns a zero-value deny for them. Filtering blindly
// would hide tools every caller can still invoke.
//
// The caller is told nothing about what was removed — no protocol channel
// reaches the model, and a zero-result list is a normal 200. The audit event
// below is the only diagnostic this feature has, which is why it fires on
// every filtered list rather than only when something changed.
//
// configuredGroupsClaimName is reported as expected_claim on the missing-claim
// record, so an operator with a non-default groups_claim_name need not
// cross-reference config. Same field withAuthorization emits.
//
// Correlation ID is stamped on by the correlation slog handler reading it from
// ctx — do NOT add correlation_id here, or the record will carry two.
func WithListFiltering(policy *authz.Policy, configuredGroupsClaimName string) mcp.Middleware {
	// Fail at wiring time rather than on the first request: a nil policy means
	// the composition-site guard was dropped, and every list would silently go
	// out unfiltered.
	if policy == nil {
		panic("WithListFiltering: nil policy (composition-site invariant violated)")
	}

	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != methodToolsList {
				return res, err
			}

			ltr, ok := res.(*mcp.ListToolsResult)
			if !ok || ltr == nil {
				return res, err
			}

			// Groups come off the SDK's TokenInfo (authorization input); the
			// record's identity comes from the Principal on ctx (SOL-152087),
			// as at every other emit site.
			var info *sdkauth.TokenInfo
			if extra := req.GetExtra(); extra != nil {
				info = extra.TokenInfo
			}
			id := NewIdentityFromPrincipal(auth.PrincipalFrom(ctx))
			groups, present := requestGroups(info)

			toolsBefore := len(ltr.Tools)
			kept := make([]*mcp.Tool, 0, toolsBefore)
			nilTools := 0
			// Counts only what the policy allowed, never the exemption, so
			// "the caller was granted nothing" needs no inspection of kept.
			granted := 0

			for _, t := range ltr.Tools {
				// The name lives inside the object we cannot read, so a nil
				// entry can only be counted, not identified.
				if t == nil {
					nilTools++
					continue
				}
				// Exempt before the policy check, which would deny it.
				if IsExemptFromToolAuthorization(t.Name) {
					kept = append(kept, t)
					continue
				}
				// Fail closed. Checked per tool rather than short-circuiting
				// above so the exemption still survives this path.
				if !present {
					continue
				}
				if policy.Authorize(groups, t.Name).Allowed {
					kept = append(kept, t)
					granted++
				}
			}

			if nilTools > 0 {
				slog.LogAttrs(ctx, slog.LevelError,
					"internal: nil tools in tools/list result",
					slog.String("event", eventToolListFilter),
					slog.Int("nil_tools", nilTools),
					slog.Int("tools_before", toolsBefore))
			}

			reason, level := listFilterOutcome(present, granted, len(kept), toolsBefore)

			// The caller's groups are deliberately not logged: counts and the
			// reason diagnose the request without disclosing one caller's
			// group membership to everyone who can read the log.
			attrs := []slog.Attr{
				slog.String("event", eventToolListFilter),
				slog.String("decision_reason", reason),
				slog.Bool("groups_present", present),
				slog.Int("tools_before", toolsBefore),
				slog.Int("tools_after", len(kept)),
			}
			// Only on missing_claim: elsewhere the claim was read fine, so naming
			// it would suggest a fault that is not there.
			if !present {
				attrs = append(attrs,
					slog.String("expected_claim", sanitize.Claim(configuredGroupsClaimName)))
			}
			attrs = append(attrs, slog.Any("", id))

			slog.LogAttrs(ctx, level, "tool list filter", attrs...)

			// Copy rather than assigning into ltr: the result belongs to the
			// SDK. Sharing the *mcp.Tool pointers is fine — tools are immutable
			// after registration.
			out := *ltr
			out.Tools = kept
			return &out, nil
		}
	}
}

// listFilterOutcome classifies a completed filter pass for the audit event.
//
// missing_claim is WARN because the token could not answer the authorization
// question at all — a server- or IdP-side fault, invisible to the caller, and
// affecting every user of the deployment. Every other outcome is the configured
// policy working correctly, so INFO; which of those an operator cares about is
// theirs to decide from decision_reason.
//
// not_permitted and missing_claim are indistinguishable to the caller by
// design. They must not be indistinguishable here.
//
// granted counts policy allows only, so not_permitted stays correct however
// many tools are exempt.
func listFilterOutcome(present bool, granted, after, before int) (reason string, level slog.Level) {
	if !present {
		return listReasonMissingClaim, slog.LevelWarn
	}
	if granted == 0 {
		return listReasonNotPermitted, slog.LevelInfo
	}
	if after == before {
		return listReasonUnfiltered, slog.LevelInfo
	}
	return listReasonFiltered, slog.LevelInfo
}
