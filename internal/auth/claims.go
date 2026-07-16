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
	"log/slog"
)

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
		result := make([]string, 0, min(len(val), groupsSoftCap))
		for _, elem := range val {
			s, isStr := elem.(string)
			if !isStr {
				continue
			}
			result = append(result, s)
			if len(result) >= groupsSoftCap {
				break
			}
		}
		if len(result) == 0 {
			return nil, false
		}
		if len(result) >= groupsSoftCap && len(val) > groupsSoftCap {
			slog.Warn("token groups claim exceeded cap; truncated",
				slog.Int("total", len(val)),
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
