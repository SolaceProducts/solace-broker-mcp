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

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/observability/correlation"
)

// obsConfig builds an ObservabilityConfig with the two flags this story cares
// about; all other observability flags default to their zero value.
func obsConfig(panicRecovery, correlationID bool) config.ObservabilityConfig {
	return config.ObservabilityConfig{
		PanicRecoveryEnabled: panicRecovery,
		CorrelationIDEnabled: correlationID,
	}
}

// get drives a GET request for path through handler and returns the recorder.
func get(handler http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// muxWithPanicRoute builds on the shared buildMux routes (/livez, /health,
// /ready) and adds a /panic route whose handler always panics. Reusing the
// shared buildMux keeps the probe routes identical to production, so the test
// proves they still resolve through the recovery wrapper.
func muxWithPanicRoute() *http.ServeMux {
	mux := buildMux(func() []string { return nil }, func(context.Context, string) error { return nil })
	mux.HandleFunc("/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom from a route inside the mux")
	})
	return mux
}

// TestBuildRootHandler_PanicRecoveryWrapsWholeMux pins AC #1/#2: with the flag
// ON, the assembled root handler catches a panic from a route INSIDE the mux
// and returns 500 — proving recovery is the outermost wrapper around the whole
// mux — while the standalone probe routes still pass through to 200, and the
// process keeps running.
func TestBuildRootHandler_PanicRecoveryWrapsWholeMux(t *testing.T) {
	t.Parallel()
	root := buildRootHandler(muxWithPanicRoute(), obsConfig(true, false))

	// A panic in a mux route is caught -> 500.
	if rec := get(root, "/panic"); rec.Code != http.StatusInternalServerError {
		t.Errorf("/panic status = %d, want %d (recovery must wrap the whole mux)", rec.Code, http.StatusInternalServerError)
	}
	// The standalone probe routes still reach their handlers through the wrapper.
	for _, path := range []string{"/livez", "/health"} {
		if rec := get(root, path); rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want %d through the recovery wrapper", path, rec.Code, http.StatusOK)
		}
	}
}

// TestBuildRootHandler_FlagOffPanicPropagates pins AC #4: with the flag OFF the
// recovery middleware is NOT wired, so a panic propagates out of the root
// handler (today's behaviour) instead of becoming a 500. We catch the re-panic
// here to assert it WAS re-raised.
func TestBuildRootHandler_FlagOffPanicPropagates(t *testing.T) {
	t.Parallel()
	root := buildRootHandler(muxWithPanicRoute(), obsConfig(false, false))

	defer func() {
		if r := recover(); r == nil {
			t.Error("panic did not propagate with recovery flag OFF; want it to escape the root handler")
		}
	}()
	_ = get(root, "/panic") // must panic; the deferred recover() asserts it did.
	t.Error("ServeHTTP returned without panicking; recovery flag OFF must let panics propagate")
}

// TestChainOrder_CorrelationInsideRecovery pins AC #5: it assembles the SAME
// layering main() builds — recovery wrapping the whole mux, and the /mcp route
// gated on correlation.Middleware INSIDE the mux — then proves both halves of
// the order at once:
//
//   - correlation ran INSIDE recovery: the inner /mcp handler observes a
//     non-empty correlation ID (correlation.From != "").
//   - recovery is OUTSIDE correlation: a panic in a sibling mux route is still
//     caught and turned into a 500.
//
// This test fails if a future PR reorders the chain (e.g. wraps recovery inside
// correlation) or leaves the mux unwrapped. It mirrors how main() wires the
// /mcp endpoint (see cmd/server/main.go) and will need updating if that wiring
// changes.
func TestChainOrder_CorrelationInsideRecovery(t *testing.T) {
	t.Parallel()
	cfg := obsConfig(true, true)

	var seenID string
	mux := buildMux(func() []string { return nil }, func(context.Context, string) error { return nil })

	// Inner /mcp handler records the correlation ID it observes.
	var mcpEndpoint http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = correlation.From(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	// Gate correlation onto /mcp exactly as main() does.
	if correlation.Enabled(cfg) {
		mcpEndpoint = correlation.Middleware(mcpEndpoint)
	}
	mux.Handle("/mcp", mcpEndpoint)
	mux.HandleFunc("/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	root := buildRootHandler(mux, cfg)

	// correlation ran INSIDE recovery: a non-empty ID reached the inner handler.
	if rec := get(root, "/mcp"); rec.Code != http.StatusOK {
		t.Fatalf("/mcp status = %d, want %d", rec.Code, http.StatusOK)
	}
	if seenID == "" {
		t.Error("inner /mcp handler saw no correlation ID; correlation.Middleware must run INSIDE recovery")
	}

	// recovery is OUTSIDE: a panic in a sibling route is still caught.
	if rec := get(root, "/panic"); rec.Code != http.StatusInternalServerError {
		t.Errorf("/panic status = %d, want %d; recovery must remain the outermost wrapper", rec.Code, http.StatusInternalServerError)
	}
}
