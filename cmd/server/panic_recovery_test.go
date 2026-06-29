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

// obsConfig builds an ObservabilityConfig with the correlation flag this story
// cares about; all other observability flags default to their zero value.
// Panic recovery is unconditional and no longer a config flag.
func obsConfig(correlationID bool) config.ObservabilityConfig {
	return config.ObservabilityConfig{
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

// TestBuildRootHandler_PanicRecoveryWrapsWholeMux pins AC #1/#2: recovery is
// unconditional, so the assembled root handler catches a panic from a route
// INSIDE the mux and returns 500 — proving recovery is the outermost wrapper
// around the whole mux — while the standalone probe routes still pass through
// to 200, and the process keeps running.
func TestBuildRootHandler_PanicRecoveryWrapsWholeMux(t *testing.T) {
	t.Parallel()
	root := buildRootHandler(muxWithPanicRoute())

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

// TestChainOrder_CorrelationInsideRecovery pins AC #5 against the REAL wiring:
// it assembles the /mcp route through the production buildMCPEndpoint and wraps
// the mux through the production buildRootHandler — the same two functions
// main() calls — then proves both halves of the chain order at once:
//
//   - correlation ran INSIDE recovery: the inner /mcp handler (standing in for
//     auth→SDK) observes a non-empty correlation ID (correlation.From != "").
//   - recovery is OUTSIDE correlation: a panic in a sibling mux route is still
//     caught and turned into a 500.
//
// Because the /mcp layering comes from buildMCPEndpoint (not a hand-rebuilt
// copy), this test FAILS if a future PR drops correlation from /mcp, or if
// buildRootHandler stops wrapping the mux. It pins correlation's PRESENCE
// inside recovery (the AC #5 essential); it does NOT pin the body-limit-vs-
// correlation relative order — both wrap the same handler and neither is
// observable to a normal GET, so asserting 413-before-correlation would need a
// separate oversized-body test. The stand-in inner handler substitutes for the
// auth+SDK handler that main() passes as authedHandler; auth's position
// relative to the SDK is fixed where authedHandler is constructed, not here.
func TestChainOrder_CorrelationInsideRecovery(t *testing.T) {
	t.Parallel()
	cfg := obsConfig(true)

	var seenID string
	// Stand-in for authedHandler (auth middleware → MCP SDK handler): records
	// the correlation ID it observes so we can prove correlation ran upstream.
	authedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenID = correlation.From(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	// Assemble /mcp through the SAME function main() uses, so a production
	// reorder of the route-local chain breaks this test.
	mux := buildMux(func() []string { return nil }, func(context.Context, string) error { return nil })
	mux.Handle("/mcp", buildMCPEndpoint(authedHandler, cfg.CorrelationIDEnabled))
	mux.HandleFunc("/panic", func(http.ResponseWriter, *http.Request) {
		panic("boom")
	})

	root := buildRootHandler(mux)

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
