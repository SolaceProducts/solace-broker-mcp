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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sort"
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

// audienceMismatchLoggedCap bounds audienceMismatchLogged (SOL-155161). A
// real deployment's distinct (broker, requested, returned) shapes number in
// the tens at most — one entry per configured broker alias, since a real
// IdP returns a stable canonicalization for the same request every time.
// 10,000 is far above that, so it is never reached in ordinary operation;
// it exists only to stop an IdP that never settles into a stable aud shape
// from growing this map once per call for the life of the process.
const audienceMismatchLoggedCap = 10_000

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

// warnIfAudienceMismatch logs (INFO the first time for a given shape, DEBUG
// after — see the throttle paragraph below) when accessToken is a JWT whose
// "aud" claim differs from requestedAudience. This is a diagnostic, not a
// security control: the broker is the resource server and its own audience
// validation is the actual enforcement point, but it only checks its own
// configured value — it never sees what this process asked the IdP for. If
// an IdP canonicalizes or ignores the requested audience (e.g. Entra's
// "api://" resource-URI prefixing) and happens to issue a value the broker
// still accepts, nothing else in this call chain would ever surface that
// the per-broker audience config is inert. This is that surfacing, and it
// changes no outcome: the token is returned unconditionally either way
// (SOL-152981).
//
// Deliberately never a hard failure: a strict equality check would risk
// failing a legitimately-configured IdP integration this project hasn't
// tested against, trading a diagnostic gap for an availability regression.
//
// Logged at INFO, not WARN, even on first occurrence: this process is an
// OAuth client, and per RFC 9068 and Microsoft's own Entra docs, a client
// has no basis to judge access-token content — only the resource server
// owns the aud claim's meaning. Canonicalization rules are IdP-specific and
// undocumented to the party requesting the token, so a mismatch here is at
// least as likely to be the IdP behaving exactly as designed (Entra's case)
// as it is a real problem, and WARN would assert the latter either way.
// INFO keeps the line visible at the default log level without implying an
// incident this process has no basis to claim (SOL-155161).
//
// Throttled per distinct (broker, requested, returned) mismatch, not fired
// on every call: some real IdPs canonicalize the requested value on every
// exchange, so an unconditional per-call line would fire on literally every
// successful call against those IdPs, forever — training an operator to
// ignore this process's output. The first occurrence of a given mismatch
// shape still logs at INFO; every later occurrence of that exact shape logs
// at DEBUG instead.
//
// Keyed on the full tuple, not broker alias alone: alias alone would let an
// already-throttled mismatch shape silently swallow a later,
// differently-shaped mismatch on that same alias — exactly the failure mode
// this throttle exists to avoid ("must not silence real problems").
// requestedAudience is included because this function has no package-level
// guarantee it stays constant per alias — it arrives as a plain per-call
// parameter, not something this package validates.
//
// Silently no-ops (no log line at all, either level) when:
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
// substrings including "token", which "aud_claim" doesn't.
func (e *Exchanger) warnIfAudienceMismatch(ctx context.Context, brokerAlias, requestedAudience, accessToken string) {
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

	level := slog.LevelInfo
	key := audienceMismatchKey(brokerAlias, requestedAudience, claims.Audience)
	switch _, alreadyLogged := e.audienceMismatchLogged.Load(key); {
	case alreadyLogged:
		level = slog.LevelDebug
	case e.audienceMismatchLoggedCount.Load() >= audienceMismatchLoggedCap:
		// Cap reached: stop remembering new shapes rather than grow
		// audienceMismatchLogged without bound. level stays INFO — an
		// occasional repeated INFO line from an IdP that never settles
		// into a stable set of shapes is the safe failure mode here, not
		// unbounded process-lifetime memory growth.
	default:
		if _, loaded := e.audienceMismatchLogged.LoadOrStore(key, struct{}{}); loaded {
			level = slog.LevelDebug
		} else {
			e.audienceMismatchLoggedCount.Add(1)
		}
	}
	slog.Log(ctx, level, "token exchange: issued access token's aud claim differs from the requested audience (may reflect expected IdP canonicalization behavior, not necessarily an error)",
		slog.String("broker", brokerAlias),
		slog.String("requested_audience", requestedAudience),
		slog.Any("aud_claim", boundedAudienceList(claims.Audience)))
}

// audienceMismatchKey identifies one distinct (broker, requested, returned)
// mismatch shape for the audienceMismatchLogged throttle, built from the
// full, untruncated aud claim (never boundedAudienceList's truncated display
// copy, to avoid folding two different long values onto the same key) and
// sorted so multi-value aud claims compare equal regardless of incidental
// IdP reordering between otherwise-identical issuances.
//
// JSON-encoded rather than delimiter-joined: a raw separator byte between
// fields is not injective — e.g. aud ["tenant-a\x1ftenant-b"] and aud
// ["tenant-a", "tenant-b"] would join to the same string under a fixed
// separator, silently demoting a later, different mismatch to DEBUG.
// json.Marshal of a fixed-shape struct escapes any embedded separator, so
// distinct inputs always encode to distinct keys.
//
// SHA-256'd rather than returned as the raw JSON: the aud claim is
// IdP-controlled and unbounded in length before it reaches this function,
// so a pathological or compromised IdP returning a very large aud array
// would otherwise inflate map memory — audienceMismatchLoggedCap bounds
// entry count, not entry size. Hashing fixes every entry at 32 bytes; a
// collision would at most demote one mismatch's log line to DEBUG, never
// alter the token returned, and isn't a practical concern at SHA-256's
// collision resistance for a non-adversarial dedup key.
func audienceMismatchKey(brokerAlias, requestedAudience string, aud jwtAudience) string {
	sorted := make([]string, len(aud))
	copy(sorted, aud)
	sort.Strings(sorted)
	encoded, _ := json.Marshal(struct {
		BrokerAlias       string   `json:"broker_alias"`
		RequestedAudience string   `json:"requested_audience"`
		Audience          []string `json:"audience"`
	}{brokerAlias, requestedAudience, sorted})
	sum := sha256.Sum256(encoded)
	return string(sum[:])
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
