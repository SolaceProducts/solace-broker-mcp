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

package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gowebpki/jcs"
)

// sensitiveKeySubstrings mirrors cmd/server's redactedKeys — the same
// case-insensitive substring match docs/internal/secure-logging-rules.md
// Rule 3 applies as a ReplaceAttr safety net on log output — applied here to
// a hash's pre-image instead. Nothing about arguments_hash's own emitted
// attribute matches Rule 3's key patterns (the key is "arguments_hash"), so
// that net cannot reach a secret-shaped value baked into the digest; this is
// the control for that gap. Kept as its own copy rather than importing
// cmd/server's var: this package must not depend on cmd/server, and the two
// lists mean the same policy, not the same Go value.
var sensitiveKeySubstrings = []string{"password", "token", "secret", "authorization", "credential", "api_key", "private_key"}

// redactedPlaceholder replaces a sensitive value before hashing. Fixed rather
// than derived from the original value, so that two calls differing only in a
// redacted field's value — for example a broker's replication bridge password
// changing — hash identically instead of leaking a comparison oracle over the
// secret.
const redactedPlaceholder = "[REDACTED]"

// RedactSensitive returns a copy of args with the value of any key matching
// sensitiveKeySubstrings replaced by redactedPlaceholder, recursively through
// nested maps and slices — the shape a free-form SEMP config object argument
// (for example update-message-vpn's msgVpnConfig, which the composite schema
// leaves as an unconstrained object) can take. Call this before HashArgs on
// any argument map that reached the wire from outside this process; nothing
// else stands between such a value and the audit stream's arguments_hash
// digest.
func RedactSensitive(args map[string]any) map[string]any {
	redacted, ok := redactValue(args).(map[string]any)
	if !ok {
		// args is declared map[string]any, so redactValue's map branch always
		// returns one; this exists only so a future refactor that breaks that
		// invariant fails loudly instead of returning a nil map that would
		// hash as {} regardless of the real arguments.
		return map[string]any{}
	}
	return redacted
}

// redactValue walks v, replacing the value of any map key matching
// sensitiveKeySubstrings and recursing into maps and slices.
func redactValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			if isSensitiveKey(k) {
				out[k] = redactedPlaceholder
				continue
			}
			out[k] = redactValue(item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = redactValue(item)
		}
		return out
	default:
		return v
	}
}

// isSensitiveKey reports whether key matches sensitiveKeySubstrings,
// case-insensitively.
func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, s := range sensitiveKeySubstrings {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}

// HashArgs returns the lowercase hex SHA-256 (FIPS 180-4) of the RFC 8785
// (JSON Canonicalization Scheme) form of args.
//
// This is the ONLY representation of tool arguments an audit record may carry.
// Raw argument values never enter the audit stream: a delete-queue call's
// queue name, a message selector, any operator-supplied string — none of it is
// written. An auditor holding the original arguments recomputes the hash and
// proves the recorded event corresponds to that specific call.
//
// Why RFC 8785 rather than sorting keys ourselves: the reproducibility promise
// is only worth something if a customer's Python, Java, or Rust tooling can
// regenerate the same digest. That requires agreeing on number serialisation
// (ES6 JSON.stringify semantics), Unicode escaping, and recursive key ordering
// by UTF-16 code unit — the edge cases a home-grown canonicaliser gets subtly
// wrong, producing a hash nobody outside this process can reproduce and no test
// here would catch. github.com/gowebpki/jcs (Apache-2.0) implements the RFC and
// is verified against its published test vectors.
//
// A nil or empty map hashes as the empty JSON object. The two are the same
// call as far as MCP is concerned — tools/call carries arguments with
// omitempty, so "no arguments field" and "{}" arrive indistinguishably — and
// hashing nil as JSON null would split one call into two digests.
//
// Returns an error rather than a sentinel digest when args cannot be
// represented as JSON (a NaN or +Inf float, a channel or func value). That is
// unreachable from the production path, where args is always the output of
// json.Unmarshal into map[string]any, but a caller must not be able to record
// a digest that stands for nothing. A caller that cannot hash cannot build a
// valid operation record and should emit a drop notice instead.
//
// Logging the returned error: log its Go TYPE, not its text. The wrapped
// encoding/json error is stdlib-authored and renders the offending value for
// some cases, and the values here are tool arguments — the one thing this
// function exists to keep out of the log. This is the same rule
// internal/tools/manager.go's logToolResult applies to errors it cannot vouch
// for (docs/internal/secure-logging-rules.md).
func HashArgs(args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	// json.Marshal is only the transport into jcs.Transform, which reparses and
	// re-serialises per RFC 8785. Go's HTML escaping of <, > and & is therefore
	// normalised away here rather than baked into the digest, and Go's own
	// number formatting is replaced by the ES6 form the RFC mandates.
	raw, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("audit: encoding arguments for hashing: %w", err)
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return "", fmt.Errorf("audit: RFC 8785 canonicalization: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
