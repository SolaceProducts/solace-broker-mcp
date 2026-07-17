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

package auth

import (
	"encoding/json"
	"fmt"
	"log/slog"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Claims is the single decoded view of a verified token's payload — the
// canonical representation at the auth boundary. All claim access goes
// through typed accessor methods with exact, case-sensitive key lookup
// (RFC 7519 claim names are case-sensitive); the raw form never escapes
// this package. Consumers with new type requirements add an accessor
// method here rather than reshaping the map to their preference — the
// boundary owns the representation, consumers adapt to the boundary.
type Claims struct {
	raw map[string]json.RawMessage
}

// String extracts an optional string claim. An absent claim returns
// ("", nil). A present claim of any non-string JSON type is a hard
// failure — callers depend on these values for identity, auditing, and
// scope parsing, so a type mismatch means the token cannot be safely
// interpreted.
func (c Claims) String(key string) (string, error) {
	msg, ok := c.raw[key]
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(msg, &s); err != nil {
		return "", fmt.Errorf("%w: claim %q has unexpected type: %v", sdkauth.ErrInvalidToken, key, err)
	}
	return s, nil
}

// Value hydrates a single claim into its generic Go form (string, float64,
// bool, []any, map[string]any, or nil for JSON null), lazily and on demand.
// exists reports whether the key is present; a present JSON null yields
// (nil, true, nil). Hydration is deterministic — every caller sees the
// identical value for the same key — so lazy per-claim access cannot
// reintroduce a parser differential.
//
// The error branch is defensive: the top-level document parse already
// validated every stored RawMessage, so re-hydration cannot fail on syntax
// today. If a future change ever makes it fire, fail closed rather than
// compute an authorization decision over a partial claim set.
func (c Claims) Value(key string) (val any, exists bool, err error) {
	msg, ok := c.raw[key]
	if !ok {
		return nil, false, nil
	}
	if err := json.Unmarshal(msg, &val); err != nil {
		return nil, true, fmt.Errorf("%w: claim %q is malformed: %v", sdkauth.ErrInvalidToken, key, err)
	}
	return val, true, nil
}

// groupsSoftCap is the maximum number of group values ResolveGroups returns.
// Values beyond this are truncated (first N in JWT-array order) and a WARN
// is emitted. The cap sits above every legitimate IdP-emitted ceiling
// (Entra 200, Okta 100) and inside typical reverse-proxy header limits.
const groupsSoftCap = 250

// ResolveGroups extracts the group list at claimName from a decoded JWT
// claims map. claimName is looked up only as a top-level key — nested paths
// are not supported, matching the Solace broker's accessLevelGroupsClaimName
// stance.
//
// This is a thin lookup wrapper over resolveGroupsValue, which holds all
// value semantics. The auth boundary (buildTokenInfo) performs its own
// exact-key lookup via Claims.Value and calls resolveGroupsValue directly;
// this map-based entry point is retained for existing callers and tests.
//
// Return semantics:
//   - ok == true: groups contains the extracted values (possibly zero-length
//     for an empty JSON array — distinct from missing, represents
//     "authenticated user with zero groups").
//   - ok == false: could not extract usable groups. Every failure reason
//     (key absent, claims nil, value nil, unusable type, empty scalar string,
//     array of all non-strings) folds into this single outcome.
//   - Key present, value is a non-empty scalar string → ([]string{s}, true).
//   - Key present, value is []any → collect strings, silently drop
//     non-strings. Empty result from an originally non-empty array still
//     returns (nil, false). Empty JSON array [] → ([]string{}, true).
//   - Key present, value is []string (defensive; json.Unmarshal produces
//     []any) → returned as-is (copy made by caller if needed).
//   - Duplicates are preserved — dedup happens at Authorize, not here.
//   - Soft cap: if more than 250 elements survive filtering, truncate to
//     the first 250 and emit a WARN.
func ResolveGroups(claims map[string]any, claimName string) (groups []string, ok bool) {
	v, exists := claims[claimName]
	if !exists {
		return nil, false
	}
	return resolveGroupsValue(v)
}

// resolveGroupsValue extracts a group list from a single hydrated claim
// value. All type-switch semantics documented on ResolveGroups live here;
// the two entry points cannot drift because ResolveGroups delegates to
// this function unconditionally after its key lookup.
//
// NOTE (open policy, see buildTokenInfo): a present-but-unusable value —
// wrong type, empty scalar, array of all non-strings — currently folds into
// ok == false, and mixed arrays silently drop non-string elements. If the
// team decides malformed groups should hard-reject the token, this function
// grows a third "malformed" return state and buildTokenInfo maps it to
// sdkauth.ErrInvalidToken.
func resolveGroupsValue(v any) (groups []string, ok bool) {
	if v == nil {
		return nil, false
	}

	switch val := v.(type) {
	case string:
		if val == "" {
			return nil, false
		}
		return []string{val}, true

	case []any:
		if len(val) == 0 {
			return []string{}, true
		}
		var count int
		result := make([]string, 0, min(len(val), groupsSoftCap))
		for _, elem := range val {
			s, isStr := elem.(string)
			if !isStr {
				continue
			}
			count++
			if len(result) < groupsSoftCap {
				result = append(result, s)
			}
		}
		if len(result) == 0 {
			return nil, false
		}
		if count > groupsSoftCap {
			slog.Warn("token groups claim exceeded cap; truncated",
				slog.Int("total", count),
				slog.Int("cap", groupsSoftCap))
		}
		return result, true

	case []string:
		if len(val) == 0 {
			return []string{}, true
		}
		result := val
		if len(result) > groupsSoftCap {
			slog.Warn("token groups claim exceeded cap; truncated",
				slog.Int("total", len(result)),
				slog.Int("cap", groupsSoftCap))
			result = result[:groupsSoftCap]
		}
		return result, true

	default:
		return nil, false
	}
}
