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

package tokenexchange

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
)

// DeduplicationKeyInput defines the fields that determine whether two
// token-exchange requests are logically identical. Every field in this
// struct participates in the key — if a field is here, it matters for
// deduplication; if it's not here, it doesn't.
//
// Used by singleflight (in-flight deduplication) and the future token
// cache (cross-time deduplication).
//
// Every constructor of this struct — today Exchange (exchange.go) and
// OAuthAuthenticator.HandleAuthFailure (internal/semp/auth/oauth.go) — must
// populate the same fields for the same logical (subject, broker, audience)
// tuple, or Invalidate computes a different key than Exchange cached under
// and silently fails to evict the token it meant to (SOL-152981).
type DeduplicationKeyInput struct {
	SubjectToken string
	// BrokerAlias was already in this key before Audience joined it, despite
	// ExchangeInput documenting BrokerAlias as a logging label that "does not
	// appear in the IdP request body" — it doesn't determine what gets
	// exchanged for. Audience, by contrast, is sent to the IdP and does
	// determine that (via Params.AudienceParam), yet wasn't here until now.
	// This key was correct only by the accident that BrokerAlias happens to
	// determine Audience 1:1 today (one audience per broker alias, fixed at
	// construction) — not because the key was self-sufficient. Adding
	// Audience makes that true by construction instead of by coincidence
	// (SOL-152981).
	BrokerAlias string
	Audience    string
}

// String, GoString, and LogValue redact SubjectToken so
// DeduplicationKeyInput never leaks it through fmt formatting or slog
// reflection. Audience is not secret — it's already logged elsewhere (e.g.
// ExchangeError.Audience, ExchangeInput.LogValue) — so it's included here too.
// Value receivers so *DeduplicationKeyInput is covered too. Pattern mirrors
// cache.CachedCredential.
func (d DeduplicationKeyInput) String() string {
	return fmt.Sprintf("DeduplicationKeyInput{BrokerAlias: %q, Audience: %q}", d.BrokerAlias, d.Audience)
}

func (d DeduplicationKeyInput) GoString() string {
	return d.String()
}

func (d DeduplicationKeyInput) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("broker_alias", d.BrokerAlias),
		slog.String("audience", d.Audience),
	)
}

// computeDeduplicationKey produces a deterministic, collision-resistant
// hex string from the input fields. Same inputs always produce the same
// key. Fields are separated by a NUL byte to prevent ("ab","c") from
// colliding with ("a","bc").
func computeDeduplicationKey(input DeduplicationKeyInput) string {
	h := sha256.New()
	h.Write([]byte(input.SubjectToken))
	h.Write([]byte{0x00})
	h.Write([]byte(input.BrokerAlias))
	h.Write([]byte{0x00})
	h.Write([]byte(input.Audience))
	return hex.EncodeToString(h.Sum(nil))
}
