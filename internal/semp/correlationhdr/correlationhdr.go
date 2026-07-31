// Package correlationhdr attaches request-correlation headers to outbound SEMP
// HTTP requests so a single MCP call can be traced across the MCP server's logs
// and the broker's logs.
//
// It is a small leaf package imported by both the SEMPv1 and SEMPv2 clients so
// that the outbound-header formatting logic lives in exactly one place. The
// header names and the W3C trace-id validation come from the correlation
// package (the single definition shared with the inbound middleware), so an ID
// received on one hop is forwarded under the same names on the next. Placing
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

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// randRead is the entropy source for span-ids. It is a package var (not a direct
// crypto/rand.Read call) so a test can force the all-zero and error draws that
// newSpanID must reject — draws otherwise unreachable (~1 in 2^64). Production never reassigns it.
var randRead = rand.Read

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

	req.Header.Set(correlation.HeaderCorrelationID, id)

	if correlation.ValidTraceID(id) {
		if span, ok := newSpanID(); ok {
			req.Header.Set(correlation.HeaderTraceparent, "00-"+id+"-"+span+"-01")
		}
	}
}

// newSpanID returns a fresh 16-hex-char W3C span-id sourced from crypto/rand,
// and ok=false when no valid span-id can be produced. ok=false in two cases:
//   - the read fails (unreachable on modern Go — a failing entropy source
//     crashes the runtime internally), and
//   - the read succeeds but yields all-zero bytes. The all-zero span-id is
//     invalid per W3C Trace Context, so we reject it rather than emit
//     "0000000000000000". This is astronomically rare (1 in 2^64) but a real
//     possibility, so we fold it into the same omit-the-traceparent path.
//
// On !ok the caller omits the traceparent entirely rather than emit a span that
// looks real but isn't; the X-Correlation-ID header still carries the
// correlation ID, so cross-system correlation is preserved without a fabricated
// span.
func newSpanID() (string, bool) {
	var b [8]byte
	if _, err := randRead(b[:]); err != nil {
		return "", false
	}
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "", false
	}
	return hex.EncodeToString(b[:]), true
}
