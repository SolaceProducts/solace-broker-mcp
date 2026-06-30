// Package correlationhdr attaches request-correlation headers to outbound SEMP
// HTTP requests so a single MCP call can be traced across the MCP server's logs
// and the broker's logs.
//
// It is a small leaf package imported by both the SEMPv1 and SEMPv2 clients so
// that header spelling and formatting logic live in exactly one place. Placing
// the logic in the parent internal/semp package would create an import cycle
// (internal/semp imports sempv1 and sempv2; those clients would then need to
// import internal/semp), so the shared code lives here instead. This package
// imports only the correlation reader and the standard library; it imports
// neither sempv1/sempv2 nor their parent, so it is cycle-safe.
package correlationhdr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
)

// Header names attached to outbound SEMP requests. traceparent is the W3C Trace
// Context header (https://www.w3.org/TR/trace-context/); X-Correlation-ID
// carries the correlation identity for non-trace IDs and as a stable companion
// to traceparent. These match the inbound header names the correlation
// middleware reads, so an ID received on one hop is forwarded under the same
// names on the next.
const (
	headerTraceparent   = "traceparent"
	headerCorrelationID = "X-Correlation-ID"
)

// traceIDLen is the length, in characters, of a W3C trace-id (32 lowercase hex).
const traceIDLen = 32

// allZeroTraceID is the trace-id the W3C spec defines as invalid; we never emit
// a traceparent built from it.
const allZeroTraceID = "00000000000000000000000000000000"

// Set attaches correlation headers to req based on the correlation ID stored on
// ctx. It must be called AFTER the authenticator has run, so it cannot clobber
// or be clobbered by auth headers.
//
// Behavior:
//   - No correlation ID on ctx (correlation.From returns ""): attach nothing.
//     This is the capability-off path — when the correlation middleware is not
//     wired, From returns "", so gating transitively honors
//     OBS_CORRELATION_ID_ENABLED without this package reading any config.
//   - A 32-lowercase-hex, non-all-zero trace-id: set X-Correlation-ID to the id
//     verbatim AND set traceparent to "00-<id>-<spanid>-01", where <spanid> is a
//     fresh 16-hex-char value. A new child span per outbound hop is correct W3C
//     behavior.
//   - Any other non-empty id (UUIDv7, or an arbitrary sanitized
//     X-Correlation-ID value): set only X-Correlation-ID to the id verbatim. We
//     do NOT emit a traceparent, because the id is not a valid W3C trace-id and
//     a malformed trace-id would be worse than none.
func Set(ctx context.Context, req *http.Request) {
	id := correlation.From(ctx)
	if id == "" {
		return
	}

	req.Header.Set(headerCorrelationID, id)

	if isTraceID(id) {
		if span, ok := newSpanID(); ok {
			req.Header.Set(headerTraceparent, "00-"+id+"-"+span+"-01")
		}
	}
}

// isTraceID reports whether id is a valid W3C trace-id: exactly 32 lowercase
// hex characters and not the all-zero value. This mirrors the validation the
// correlation package applies to an inbound traceparent's trace-id segment; we
// reimplement it here (rather than import an unexported helper) because that
// package owns its internals and is being edited in parallel.
func isTraceID(id string) bool {
	if len(id) != traceIDLen || id == allZeroTraceID {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// newSpanID returns a fresh 16-hex-char W3C span-id sourced from crypto/rand,
// and ok=false if the (unreachable on modern Go — a failing entropy source
// crashes the runtime internally) read fails. On !ok the caller omits the
// traceparent entirely rather than emit a span that looks real but isn't; the
// X-Correlation-ID header still carries the correlation ID, so cross-system
// correlation is preserved without a fabricated span.
func newSpanID() (string, bool) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(b[:]), true
}
