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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// runMiddleware sends a request carrying the given headers through Middleware
// and returns the correlation ID observed by the downstream handler.
// nextInvoked reports whether the wrapped handler actually ran (it always must:
// the middleware never rejects).
func runMiddleware(t *testing.T, headers map[string]string) (gotID string, nextInvoked bool) {
	t.Helper()
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nextInvoked = true
		gotID = From(r.Context())
	}))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return gotID, nextInvoked
}

const validTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
const validTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// TestMiddleware_TraceparentExtraction pins that a well-formed traceparent
// yields its 32-hex trace-id segment as the correlation ID.
func TestMiddleware_TraceparentExtraction(t *testing.T) {
	t.Parallel()
	gotID, nextInvoked := runMiddleware(t, map[string]string{headerTraceparent: validTraceparent})
	if !nextInvoked {
		t.Fatal("next handler was not invoked")
	}
	if gotID != validTraceID {
		t.Errorf("correlation ID = %q, want trace-id %q", gotID, validTraceID)
	}
}

// TestMiddleware_FutureVersionTraceparentToleratesTrailingFields pins forward
// compatibility: an unrecognized (future) version may carry extra trailing
// fields and still yields its trace-id. Only the known version 00 is held to
// exactly four fields (see TestMiddleware_MalformedTraceparentFallsBack).
func TestMiddleware_FutureVersionTraceparentToleratesTrailingFields(t *testing.T) {
	t.Parallel()
	tp := "01-" + validTraceID + "-00f067aa0ba902b7-01-extrafield"
	gotID, _ := runMiddleware(t, map[string]string{headerTraceparent: tp})
	if gotID != validTraceID {
		t.Errorf("correlation ID = %q, want trace-id %q from future-version traceparent", gotID, validTraceID)
	}
}

// TestMiddleware_TraceparentPrecedence pins that a valid traceparent wins over
// a present X-Correlation-ID.
func TestMiddleware_TraceparentPrecedence(t *testing.T) {
	t.Parallel()
	gotID, _ := runMiddleware(t, map[string]string{
		headerTraceparent:   validTraceparent,
		headerCorrelationID: "client-supplied-id",
	})
	if gotID != validTraceID {
		t.Errorf("correlation ID = %q, want traceparent trace-id %q to win", gotID, validTraceID)
	}
}

// TestMiddleware_MalformedTraceparentFallsBack pins that every malformed
// traceparent shape falls back to a present, valid X-Correlation-ID.
func TestMiddleware_MalformedTraceparentFallsBack(t *testing.T) {
	t.Parallel()
	const fallback = "fallback-correlation-id"
	cases := []struct {
		name        string
		traceparent string
	}{
		{"too few fields", "00-" + validTraceID + "-00f067aa0ba902b7"},
		{"empty", ""},
		{"trace-id too short", "00-4bf92f3577b34da6-00f067aa0ba902b7-01"},
		{"trace-id too long", "00-" + validTraceID + "ab-00f067aa0ba902b7-01"},
		{"non-hex trace-id", "00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		{"uppercase trace-id", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01"},
		{"all-zero trace-id", "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		{"reserved version ff", "ff-" + validTraceID + "-00f067aa0ba902b7-01"},
		{"bad version length", "0-" + validTraceID + "-00f067aa0ba902b7-01"},
		{"bad span length", "00-" + validTraceID + "-00f067aa-01"},
		{"bad flags length", "00-" + validTraceID + "-00f067aa0ba902b7-1"},
		{"trailing fields on version 00", validTraceparent + "-extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotID, _ := runMiddleware(t, map[string]string{
				headerTraceparent:   tc.traceparent,
				headerCorrelationID: fallback,
			})
			if gotID != fallback {
				t.Errorf("correlation ID = %q, want fallback %q", gotID, fallback)
			}
		})
	}
}

// TestMiddleware_CorrelationIDEchoed pins that a valid X-Correlation-ID is
// echoed verbatim when no traceparent is present.
func TestMiddleware_CorrelationIDEchoed(t *testing.T) {
	t.Parallel()
	const id = "abc-123_XYZ.456"
	gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: id})
	if gotID != id {
		t.Errorf("correlation ID = %q, want %q echoed", gotID, id)
	}
}

// TestMiddleware_CorrelationIDTrimmed pins that surrounding whitespace is
// trimmed from an otherwise-valid X-Correlation-ID.
func TestMiddleware_CorrelationIDTrimmed(t *testing.T) {
	t.Parallel()
	gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: "  padded-id  "})
	if gotID != "padded-id" {
		t.Errorf("correlation ID = %q, want trimmed %q", gotID, "padded-id")
	}
}

// TestMiddleware_GeneratesWhenNeitherPresent pins that, with no usable inbound
// header, the middleware generates a valid UUIDv7.
func TestMiddleware_GeneratesWhenNeitherPresent(t *testing.T) {
	t.Parallel()
	gotID, _ := runMiddleware(t, nil)
	if !isUUIDv7(gotID) {
		t.Errorf("correlation ID = %q, want a valid UUIDv7", gotID)
	}
}

// TestMiddleware_InvalidInputsGenerate pins that the full invalid-input domain
// for X-Correlation-ID (with no traceparent) causes a generated UUIDv7 rather
// than echoing or sanitizing the bad value.
func TestMiddleware_InvalidInputsGenerate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"whitespace only", "   \t  "},
		{"oversized", strings.Repeat("a", maxIDLen+1)},
		{"CR injection", "id\rinjected"},
		{"LF injection", "id\ninjected"},
		{"CRLF header split", "id\r\nX-Evil: 1"},
		{"NUL byte", "id\x00bad"},
		{"tab", "id\tbad"},
		{"DEL char", "id\x7fbad"},
		{"non-ascii", "idÃ© bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: tc.value})
			if !isUUIDv7(gotID) {
				t.Errorf("for input %q: correlation ID = %q, want a generated UUIDv7 (bad input must not be echoed)", tc.value, gotID)
			}
		})
	}
}

// TestMiddleware_BoundaryLength pins the maxIDLen boundary: exactly maxIDLen is
// accepted, maxIDLen+1 is rejected (and thus generated).
func TestMiddleware_BoundaryLength(t *testing.T) {
	t.Parallel()
	atLimit := strings.Repeat("a", maxIDLen)
	gotID, _ := runMiddleware(t, map[string]string{headerCorrelationID: atLimit})
	if gotID != atLimit {
		t.Errorf("at-limit (%d chars) correlation ID = %q, want it echoed", maxIDLen, gotID)
	}
}

// TestGenerate_ValidUUIDv7 pins Generate's output shape and uniqueness.
func TestGenerate_ValidUUIDv7(t *testing.T) {
	t.Parallel()
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := Generate()
		if !isUUIDv7(id) {
			t.Fatalf("Generate() = %q, not a valid UUIDv7", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("Generate() produced a duplicate: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestFrom_Absent pins that From returns "" when no ID is on the context —
// the contract dependent code relies on when the capability is off (middleware
// not wired).
func TestFrom_Absent(t *testing.T) {
	t.Parallel()
	if got := From(context.Background()); got != "" {
		t.Errorf("From(background) = %q, want \"\"", got)
	}
}

// TestWithFrom_RoundTrip pins the With/From round-trip.
func TestWithFrom_RoundTrip(t *testing.T) {
	t.Parallel()
	const id = "round-trip-id"
	ctx := With(context.Background(), id)
	if got := From(ctx); got != id {
		t.Errorf("From(With(ctx, %q)) = %q, want %q", id, got, id)
	}
}

// TestMiddleware_DoesNotMutateParentContext pins that the stamped ID lands only
// on the derived context handed to next, not on the original request context.
func TestMiddleware_DoesNotMutateParentContext(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set(headerCorrelationID, "some-id")
	parentCtx := req.Context()

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := From(parentCtx); got != "" {
		t.Errorf("parent context was polluted: From = %q, want \"\"", got)
	}
}

// TestMiddleware_EndToEndPropagation is the httptest.Server integration test:
// it proves the correlation ID survives the full HTTP round-trip and reaches a
// real downstream handler's context.
func TestMiddleware_EndToEndPropagation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, From(r.Context()))
	})))
	defer srv.Close()

	// Subtests are intentionally NOT parallel: a parallel subtest defers its
	// body until the parent returns, by which point defer srv.Close() has
	// already shut the server down.
	t.Run("traceparent propagates end to end", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(headerTraceparent, validTraceparent)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != validTraceID {
			t.Errorf("downstream saw correlation ID %q, want %q", string(body), validTraceID)
		}
	})

	t.Run("generated when no header", func(t *testing.T) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !isUUIDv7(string(body)) {
			t.Errorf("downstream saw %q, want a generated UUIDv7", string(body))
		}
	})
}

// isUUIDv7 reports whether s is a canonical lowercase UUID with version nibble
// 7 and RFC 4122/9562 variant bits (10xx). It is a test-only structural check.
func isUUIDv7(s string) bool {
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	// Version nibble sits at index 14; variant high bits at index 19.
	if s[14] != '7' {
		return false
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
		return true
	default:
		return false
	}
}
