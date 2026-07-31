package correlationhdr_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/correlationhdr"
)

// newReq builds a throwaway request to receive headers; the URL is irrelevant.
func newReq(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://broker.invalid/SEMP", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return req
}

// isLowerHexLen reports whether s is exactly n lowercase-hex chars.
func isLowerHexLen(s string, n int) bool {
	if len(s) != n {
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

// TestSet_EmptyContext_NoHeaders pins that with no correlation ID on the
// context (capability off / middleware not wired), neither header is attached.
func TestSet_EmptyContext_NoHeaders(t *testing.T) {
	req := newReq(t)
	correlationhdr.Set(context.Background(), req)

	if got := req.Header.Get("X-Correlation-ID"); got != "" {
		t.Errorf("X-Correlation-ID = %q, want absent", got)
	}
	if got := req.Header.Get("traceparent"); got != "" {
		t.Errorf("traceparent = %q, want absent", got)
	}
}

// TestSet_TraceID_EmitsTraceparent pins that a 32-hex W3C trace-id yields both
// X-Correlation-ID (verbatim) and a well-formed traceparent with a fresh span.
func TestSet_TraceID_EmitsTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := correlation.With(context.Background(), traceID)
	req := newReq(t)
	correlationhdr.Set(ctx, req)

	if got := req.Header.Get("X-Correlation-ID"); got != traceID {
		t.Errorf("X-Correlation-ID = %q, want %q", got, traceID)
	}

	tp := req.Header.Get("traceparent")
	parts := strings.Split(tp, "-")
	if len(parts) != 4 {
		t.Fatalf("traceparent = %q, want 4 hyphen-separated fields", tp)
	}
	if parts[0] != "00" {
		t.Errorf("traceparent version = %q, want 00", parts[0])
	}
	if parts[1] != traceID {
		t.Errorf("traceparent trace-id = %q, want %q", parts[1], traceID)
	}
	if !isLowerHexLen(parts[2], 16) {
		t.Errorf("traceparent span-id = %q, want 16 lowercase-hex chars", parts[2])
	}
	if parts[2] == "0000000000000000" {
		t.Errorf("traceparent span-id is all-zero, want a fresh non-zero span")
	}
	if parts[3] != "01" {
		t.Errorf("traceparent flags = %q, want 01", parts[3])
	}
}

// TestSet_FreshSpanPerCall pins that each call mints a new span-id, matching
// correct W3C behavior of a new child span per outbound hop.
func TestSet_FreshSpanPerCall(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	ctx := correlation.With(context.Background(), traceID)

	req1 := newReq(t)
	correlationhdr.Set(ctx, req1)
	req2 := newReq(t)
	correlationhdr.Set(ctx, req2)

	span1 := strings.Split(req1.Header.Get("traceparent"), "-")[2]
	span2 := strings.Split(req2.Header.Get("traceparent"), "-")[2]
	if span1 == span2 {
		t.Errorf("span-ids equal across calls (%q); want a fresh span each call", span1)
	}
}

// TestSet_NonTraceIDs_NoTraceparent pins that any ID that is not a 32-hex
// trace-id sets only X-Correlation-ID and never emits a (malformed) traceparent.
func TestSet_NonTraceIDs_NoTraceparent(t *testing.T) {
	cases := map[string]string{
		"uuidv7":         "0190f3a1-7c2e-7b3d-9f1a-2c4e6a8b0d1f",
		"arbitrary":      "order-12345-abc",
		"allZeroTraceID": "00000000000000000000000000000000",
		"uppercaseHex":   "4BF92F3577B34DA6A3CE929D0E0E4736",
		"shortHex":       "4bf92f3577b34da6a3ce929d0e0e473",   // 31 chars
		"longHex":        "4bf92f3577b34da6a3ce929d0e0e47360", // 33 chars
		"nonHex32":       "4bf92f3577b34da6a3ce929d0e0e473g",  // has 'g'
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := correlation.With(context.Background(), id)
			req := newReq(t)
			correlationhdr.Set(ctx, req)

			if got := req.Header.Get("X-Correlation-ID"); got != id {
				t.Errorf("X-Correlation-ID = %q, want %q", got, id)
			}
			if got := req.Header.Get("traceparent"); got != "" {
				t.Errorf("traceparent = %q, want absent for non-trace-id %q", got, id)
			}
		})
	}
}
