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

package correlationhdr

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

// newReqInternal builds a throwaway request to receive headers; the URL is
// irrelevant. The black-box test file has its own newReq helper; this internal
// test file (package correlationhdr) needs its own copy.
func newReqInternal(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://broker.invalid/SEMP", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return req
}

// TestSet_AllZeroSpanDraw_OmitsTraceparent forces the randRead seam to yield an
// all-zero span-id — the W3C-invalid draw that is otherwise ~1 in 2^64 — and
// pins that Set still emits X-Correlation-ID verbatim but omits the traceparent
// rather than fabricating a "0000000000000000" span.
func TestSet_AllZeroSpanDraw_OmitsTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	// Leave b untouched (zero-valued) and report success, so newSpanID sees an
	// all-zero draw.
	randRead = func(b []byte) (int, error) { return len(b), nil }

	ctx := correlation.With(context.Background(), traceID)
	req := newReqInternal(t)
	Set(ctx, req)

	if got := req.Header.Get(correlation.HeaderCorrelationID); got != traceID {
		t.Errorf("X-Correlation-ID = %q, want %q", got, traceID)
	}
	if got := req.Header.Get(correlation.HeaderTraceparent); got != "" {
		t.Errorf("traceparent = %q, want absent on all-zero span draw", got)
	}
}

// TestSet_RandReadError_OmitsTraceparent forces the randRead seam to return an
// error — unreachable in production on modern Go — and pins that Set still emits
// X-Correlation-ID verbatim but omits the traceparent.
func TestSet_RandReadError_OmitsTraceparent(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	randRead = func(b []byte) (int, error) { return 0, errors.New("forced entropy failure") }

	ctx := correlation.With(context.Background(), traceID)
	req := newReqInternal(t)
	Set(ctx, req)

	if got := req.Header.Get(correlation.HeaderCorrelationID); got != traceID {
		t.Errorf("X-Correlation-ID = %q, want %q", got, traceID)
	}
	if got := req.Header.Get(correlation.HeaderTraceparent); got != "" {
		t.Errorf("traceparent = %q, want absent on randRead error", got)
	}
}
