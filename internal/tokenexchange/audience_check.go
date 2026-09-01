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
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
)

// auditLogAudienceCap bounds how many aud values, and how many characters of
// each, warnIfAudienceMismatch logs. The claim is IdP-controlled text
// reaching this process in an otherwise-successful exchange response — not
// attacker input in the usual sense, but not something we authored either,
// so it gets the same bounded-logging treatment as other externally-sourced
// text in this codebase.
const (
	auditLogAudienceCap    = 5
	auditLogAudienceMaxLen = 200
)

// jwtAudience accepts the JWT "aud" claim in either of its two valid shapes
// per RFC 7519 §4.1.3: a single string, or an array of strings.
type jwtAudience []string

func (a *jwtAudience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = jwtAudience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = jwtAudience(many)
	return nil
}

// jwtClaims is the minimal claim set this file inspects from an access
// token's payload segment.
type jwtClaims struct {
	Audience jwtAudience `json:"aud"`
}

// decodeJWTClaimsUnverified extracts the claims from a JWT's payload segment
// without verifying its signature. That is the correct trust boundary for
// this check: the token arrived over a TLS connection to a configured,
// trusted IdP, in a response to a request this process built — signature
// verification would only reprove what the transport already establishes.
// This function exists to sanity-check what an already-trusted response
// claims, not to authenticate it.
//
// ok is false, not an error, for anything not shaped like a JWT (not exactly
// three dot-separated segments) or whose payload segment doesn't decode as
// base64url JSON. An opaque (non-JWT) access token is valid per RFC 8693, so
// that case is expected, not suspicious — the caller treats it as "nothing
// to check" either way.
func decodeJWTClaimsUnverified(token string) (jwtClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, false
	}
	// JWS payload segments are unpadded base64url (RFC 7515 §2).
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, false
	}
	var claims jwtClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return jwtClaims{}, false
	}
	return claims, true
}

// warnIfAudienceMismatch logs a WARN when accessToken is a JWT whose "aud"
// claim does not include requestedAudience. This is a diagnostic, not a
// security control: RFC 6750 treats a bearer token as opaque to the party
// relaying it, and this process is that party — the broker is the resource
// server, and its own resource-server audience validation is the actual
// enforcement point. But the broker checks the token against its own
// configured required-audience; it never sees what this process asked the
// IdP for. If an IdP canonicalizes or ignores the requested audience (e.g.
// Entra's "api://" resource-URI prefixing) and happens to issue a value the
// broker still accepts, nothing else in this call chain would ever surface
// that the per-broker audience config is inert. This WARN is that surfacing
// — "did the IdP give us what we asked for", not "is this token valid" — and
// it changes no outcome: the token is returned unconditionally either way
// (SOL-152981).
//
// Deliberately WARN, never a failure, for the same canonicalization reason:
// a strict equality check would risk failing a legitimately-configured IdP
// integration this project hasn't tested against, trading a diagnostic gap
// for an availability regression. If a deployment wants a hard failure on
// mismatch, that belongs behind a new per-broker config flag, not
// unconditional behavior here.
//
// Silently no-ops (no WARN either way) when:
//   - requestedAudience is empty: V1 makes the audience parameter optional,
//     and without a request there is nothing to check the token against.
//   - accessToken is not JWT-shaped or its payload doesn't decode: RFC 8693
//     access tokens may legitimately be opaque, and an opaque token's claims
//     cannot be inspected client-side at all — expected, not suspicious. The
//     broker's own audience validation has the identical blind spot when its
//     access-token-parsing option is off, so this isn't a regression against
//     the backstop it's diagnosing gaps around.
//
// The token itself is never logged. The claimed audiences are logged bounded
// (auditLogAudienceCap values, auditLogAudienceMaxLen chars each) under the
// key "aud_claim" — cmd/server's ReplaceAttr redaction net matches on key
// substrings including "token", which "aud_claim" doesn't. ("audience" isn't
// in that redaction list; requested_audience two lines below is already
// logged unredacted.)
func warnIfAudienceMismatch(ctx context.Context, brokerAlias, requestedAudience, accessToken string) {
	if requestedAudience == "" {
		return
	}
	claims, ok := decodeJWTClaimsUnverified(accessToken)
	if !ok {
		return
	}
	for _, aud := range claims.Audience {
		if aud == requestedAudience {
			return
		}
	}
	slog.WarnContext(ctx, "token exchange: issued access token's aud claim does not include the requested audience",
		slog.String("broker", brokerAlias),
		slog.String("requested_audience", requestedAudience),
		slog.Any("aud_claim", boundedAudienceList(claims.Audience)))
}

// boundedAudienceList caps both the number of audience values and the
// length of each before they reach a log line — see warnIfAudienceMismatch.
func boundedAudienceList(aud jwtAudience) []string {
	n := len(aud)
	if n > auditLogAudienceCap {
		n = auditLogAudienceCap
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = truncateRunes(aud[i], auditLogAudienceMaxLen)
	}
	return out
}

// truncateRunes caps s at max runes, truncating by rune rather than byte so
// a multi-byte character is never split, and folding the ellipsis into the
// cap rather than appending after it so the result never exceeds max runes.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max == 0 {
		return ""
	}
	return string(r[:max-1]) + "…"
}
