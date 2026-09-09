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

package schema

import "slices"

// AuthFailureReason is the closed vocabulary shared by the auth_failure audit
// record's reason field (SOL-152097) and the mcp_auth_failure_total{reason}
// counter label (SOL-152099). One value, two signals, so a reviewer's audit
// query and an operator's dashboard cannot disagree about why a token was
// rejected — the point ADR-009 makes about metric/log/audit consistency.
//
// It lives here rather than in internal/auth (SOL-154163) because these five
// strings are part of this server's *output* schema, not its auth logic: a
// consumer's saved query breaks when they change, which is the same reason
// every other closed observability vocabulary is owned on this side.
// internal/auth produces the values (ClassifyAuthFailure) and
// internal/observability/{audit,metrics} render them; none of the three owns
// the set, and all three can import this leaf package without a cycle.
type AuthFailureReason string

// The five auth_failure reasons. Deliberately coarse, so no token content is
// ever exposed as a metric label or an audit field. docs/observability.md
// carries the operator-facing gloss of what each one covers — add a value
// here and to that gloss if a new failure shape needs one, never a second
// vocabulary.
const (
	// AuthFailureReasonInvalidToken is the catch-all: a malformed or
	// unparseable JWT, an issuer mismatch, a static dev-token mismatch, or an
	// Authorization header that is present but not bearer-shaped.
	AuthFailureReasonInvalidToken AuthFailureReason = "invalid_token"
	// AuthFailureReasonExpired: the token's exp claim has passed.
	AuthFailureReasonExpired AuthFailureReason = "expired"
	// AuthFailureReasonAudienceMismatch: the token's audience is not the one
	// this server expects.
	AuthFailureReasonAudienceMismatch AuthFailureReason = "audience_mismatch"
	// AuthFailureReasonSignatureInvalid: a signing or JWKS-rotation failure,
	// held distinct from a merely malformed token so a key-rotation incident
	// is visible on its own.
	AuthFailureReasonSignatureInvalid AuthFailureReason = "signature_invalid"
	// AuthFailureReasonMissing: no Authorization header was presented at all,
	// or a token that verified fully but omitted the required sub claim.
	AuthFailureReasonMissing AuthFailureReason = "missing"
)

// authFailureReasons is the closed set in the order docs/observability.md
// glosses it, catch-all first. AuthFailureReasons sorts a copy, so this
// declaration order stays free to read well rather than having to be sorted.
var authFailureReasons = []AuthFailureReason{
	AuthFailureReasonInvalidToken,
	AuthFailureReasonExpired,
	AuthFailureReasonAudienceMismatch,
	AuthFailureReasonSignatureInvalid,
	AuthFailureReasonMissing,
}

// AuthFailureReasons returns the closed vocabulary, sorted, as a fresh slice
// the caller may mutate. Consumers build their own lookup shape from it —
// internal/observability/audit's constructor validation set, SOL-152099's
// counter label pre-registration — so no call site restates the five values.
func AuthFailureReasons() []AuthFailureReason {
	out := make([]AuthFailureReason, len(authFailureReasons))
	copy(out, authFailureReasons)
	slices.Sort(out)
	return out
}
