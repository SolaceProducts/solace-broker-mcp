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

package correlation

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strings"
	"time"
)

// Header names for inbound correlation. traceparent is the W3C Trace Context
// header (https://www.w3.org/TR/trace-context/); X-Correlation-ID is the
// fallback when no W3C trace is present.
const (
	headerTraceparent   = "traceparent"
	headerCorrelationID = "X-Correlation-ID"
)

// maxIDLen caps the length of an accepted correlation ID. The value flows into
// every log line for the request and (in a later story) onto outbound HTTP
// headers, so an unbounded inbound value would be a log-flooding and
// header-bloat vector. 128 chars comfortably holds a W3C 32-hex trace-id, a
// UUID, or any sane client-supplied ID while rejecting abuse. A traceparent
// trace-id is always exactly 32 chars and a generated UUIDv7 is 36, so this cap
// only ever bites an oversized X-Correlation-ID.
const maxIDLen = 128

// key is the unexported context-key type under which the correlation ID is
// stored. Only this package can construct key{}, mirroring the unexported-key
// idiom in internal/auth (rawSubjectTokenKey{}, principalKey{}): no other
// package can collide with it or read the value directly. External code seeds
// the ID via With and reads it via From, never by addressing the key.
type key struct{}

// With returns a copy of ctx carrying id as the correlation ID. It does not
// validate id; the middleware is the sole production writer and only stores
// values that already passed sanitize. Exported for tests and for any future
// non-HTTP entry point that needs to seed a correlation ID.
func With(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, key{}, id)
}

// From returns the correlation ID stored on ctx, or "" when none is present
// (including when the middleware is not wired because the capability is off).
// Callers branch on the empty string rather than a separate ok bool: an empty
// correlation ID is indistinguishable from "absent" for logging purposes, so a
// single return keeps call sites simple.
func From(ctx context.Context) string {
	if v, ok := ctx.Value(key{}).(string); ok {
		return v
	}
	return ""
}

// Middleware returns an http.Handler that derives a correlation ID for the
// request and attaches it to the request context before calling next. The
// resolution order is:
//
//  1. traceparent: if present and well-formed, use its 32-hex trace-id segment.
//  2. X-Correlation-ID: if traceparent is absent or malformed, use this header
//     when it holds a usable (sanitized, non-empty, bounded) value.
//  3. Otherwise generate a fresh UUIDv7.
//
// The middleware always attaches a non-empty ID, so downstream code can rely on
// From returning a value once Middleware is in the chain.
//
// Callers gate installation on Enabled (OBS_CORRELATION_ID_ENABLED). When the
// capability is off the middleware is not wired and From returns "".
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := resolveID(r)
		ctx := With(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveID applies the traceparent → X-Correlation-ID → generate precedence.
func resolveID(r *http.Request) string {
	if id, ok := traceIDFromTraceparent(r.Header.Get(headerTraceparent)); ok {
		return id
	}
	if id, ok := sanitize(r.Header.Get(headerCorrelationID)); ok {
		return id
	}
	return Generate()
}

// traceIDFromTraceparent extracts the trace-id from a W3C traceparent header.
//
// Format: "version-traceid-spanid-flags" — four hyphen-separated fields where
// version is 2 hex chars, traceid is 32 hex chars, spanid is 16 hex chars, and
// flags is 2 hex chars. We return only the trace-id (the correlation identity).
//
// ok is false for any header that does not yield a usable trace-id: wrong field
// count, wrong trace-id length, non-hex characters, or the all-zero trace-id
// (which the spec defines as invalid). On a false return the caller falls back
// to X-Correlation-ID. For the known version 00 we require exactly four fields
// and reject trailing data (per W3C Trace Context); for an unrecognized future
// version we tolerate extra trailing fields. We do not require the version or
// flags fields to be meaningful beyond being valid hex of the right length.
func traceIDFromTraceparent(h string) (string, bool) {
	if h == "" {
		return "", false
	}
	parts := strings.Split(h, "-")
	// The trace-id sits in field index 1, so at least the four core fields are
	// required; fewer is always malformed.
	if len(parts) < 4 {
		return "", false
	}
	version, traceID, spanID, flags := parts[0], parts[1], parts[2], parts[3]
	if len(version) != 2 || len(traceID) != 32 || len(spanID) != 16 || len(flags) != 2 {
		return "", false
	}
	// Version ff is reserved/invalid per the spec.
	if version == "ff" {
		return "", false
	}
	// The known version 00 is defined as exactly four fields; trailing data is
	// malformed and must be rejected. Only an unrecognized (future) version may
	// carry extra trailing fields, which we tolerate for forward compatibility.
	if version == "00" && len(parts) != 4 {
		return "", false
	}
	if !isLowerHex(version) || !isLowerHex(traceID) || !isLowerHex(spanID) || !isLowerHex(flags) {
		return "", false
	}
	// All-zero trace-id is invalid per the spec.
	if traceID == "00000000000000000000000000000000" {
		return "", false
	}
	return traceID, true
}

// isLowerHex reports whether s is non-empty and contains only lowercase hex
// digits. The W3C spec mandates lowercase, so we do not accept uppercase here;
// an uppercase traceparent is malformed and falls through to the
// X-Correlation-ID path.
func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sanitize trims surrounding whitespace from a client-supplied correlation ID
// and validates it for safe use in logs and outbound headers. ok is false when
// the trimmed value is empty, exceeds maxIDLen, or contains any byte that is
// not a printable ASCII character (0x21–0x7E). The printable-only rule rejects
// CR, LF, tabs, NUL, and other control characters, closing header-injection and
// log-forging vectors: the value can be dropped verbatim into a slog field or
// an HTTP header without escaping. We reject (fall back to generate) rather
// than strip-and-keep so a hostile value never silently mutates into a
// different-but-accepted ID.
func sanitize(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > maxIDLen {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c < 0x21 || c > 0x7E {
			return "", false
		}
	}
	return v, true
}

// Generate returns a new UUIDv7 (RFC 9562) as a canonical lowercase string.
// UUIDv7 embeds a Unix-millisecond timestamp in its high bits, so generated
// IDs sort roughly by creation time — useful when correlation IDs land in
// time-series logs. The remaining bits are filled from crypto/rand.
//
// We use the stdlib (crypto/rand) rather than adding a UUID dependency: the
// layout is small and self-contained, and the project has no existing UUID
// library to reuse.
func Generate() string {
	var b [16]byte
	// Fail loudly rather than silently emit zeroed/predictable bytes, which
	// would defeat the ID's purpose. On modern Go crypto/rand.Read does not
	// return an error (a failing entropy source crashes internally), so this
	// panic is defensive and unreachable in practice. A panic on the request
	// goroutine is caught by net/http's per-connection recovery (and, once the
	// HTTP panic-recovery middleware is stacked, by that), so it does not crash
	// the process.
	if _, err := rand.Read(b[:]); err != nil {
		panic("correlation: crypto/rand.Read failed: " + err.Error())
	}

	// Timestamp: 48-bit big-endian Unix milliseconds in the first 6 bytes.
	ms := uint64(time.Now().UnixMilli())
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[0:6], ts[2:8])

	// Version 7 in the high nibble of byte 6; variant 10xx in the high bits of
	// byte 8. The low nibble of byte 6 and the low 6 bits of byte 8 keep their
	// random fill.
	b[6] = (b[6] & 0x0F) | 0x70
	b[8] = (b[8] & 0x3F) | 0x80

	return formatUUID(b)
}

// formatUUID renders the 16-byte UUID in canonical 8-4-4-4-12 lowercase form.
func formatUUID(b [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:])
}
