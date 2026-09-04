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

	"github.com/gowebpki/jcs"
)

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
