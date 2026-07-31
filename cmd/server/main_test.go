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
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testMux creates a mux using the shared buildMux function with a minimal
// MCP server. This ensures tests use the same route definitions as main().
func testMux() *http.ServeMux {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version(),
	}, nil)
	readiness := health.NewReadinessState()
	readiness.SetInitialized()
	mux := buildMux(readiness)

	// Register /mcp endpoint for testing (without auth middleware)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)
	mux.Handle("/mcp", mcpHandler)

	return mux
}

// TestLivez_GET_ReturnsOK pins the canonical liveness endpoint: 200 with the
// exact process-alive body and JSON content type.
func TestLivez_GET_ReturnsOK(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /livez status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"alive"}` {
		t.Errorf("GET /livez body = %q, want %q", rec.Body.String(), `{"status":"alive"}`)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /livez Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestLivez_POST_ReturnsMethodNotAllowed(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/livez", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /livez status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestHealth_GET_ReturnsOK pins that /health preserves its ORIGINAL shipped body
// {"status":"healthy"} — it is NOT a body-identical alias of /livez. /health is
// retained for backward compatibility so external consumers that parse
// .status == "healthy" keep working; /livez is the canonical liveness endpoint
// and returns {"status":"alive"}.
func TestHealth_GET_ReturnsOK(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"healthy"}` {
		t.Errorf("GET /health body = %q, want %q", rec.Body.String(), `{"status":"healthy"}`)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /health Content-Type = %q, want %q", ct, "application/json")
	}
}

func TestHealth_POST_ReturnsMethodNotAllowed(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/health", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /health status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestLivezAndHealthShareStatusButNotBody proves the back-compat contract:
// /livez and /health share status (200 GET, 405 non-GET) and Content-Type, but
// their bodies DIFFER deliberately — /livez returns {"status":"alive"} (canonical
// liveness) and /health returns {"status":"healthy"} (original shipped body
// retained for external consumers). They are NOT body-identical aliases.
func TestLivezAndHealthShareStatusButNotBody(t *testing.T) {
	mux := testMux()

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		livezRec := httptest.NewRecorder()
		mux.ServeHTTP(livezRec, httptest.NewRequestWithContext(context.Background(), method, "/livez", nil))

		healthRec := httptest.NewRecorder()
		mux.ServeHTTP(healthRec, httptest.NewRequestWithContext(context.Background(), method, "/health", nil))

		if livezRec.Code != healthRec.Code {
			t.Errorf("%s: status /livez=%d /health=%d, want equal", method, livezRec.Code, healthRec.Code)
		}
		if livezRec.Header().Get("Content-Type") != healthRec.Header().Get("Content-Type") {
			t.Errorf("%s: Content-Type /livez=%q /health=%q, want equal",
				method, livezRec.Header().Get("Content-Type"), healthRec.Header().Get("Content-Type"))
		}
	}

	// On GET the bodies must differ: /livez is the canonical alive signal,
	// /health preserves its original healthy body for back-compat.
	livezGet := httptest.NewRecorder()
	mux.ServeHTTP(livezGet, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/livez", nil))
	healthGet := httptest.NewRecorder()
	mux.ServeHTTP(healthGet, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil))

	if got, want := livezGet.Body.String(), `{"status":"alive"}`; got != want {
		t.Errorf("GET /livez body = %q, want %q", got, want)
	}
	if got, want := healthGet.Body.String(), `{"status":"healthy"}`; got != want {
		t.Errorf("GET /health body = %q, want %q", got, want)
	}
	if livezGet.Body.String() == healthGet.Body.String() {
		t.Errorf("/livez and /health bodies must differ, both = %q", livezGet.Body.String())
	}
}

func TestMCPEndpoint_POST_ReachesMCPHandler(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", nil)
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	// The MCP handler should respond — not 404. The exact status depends on
	// the request body (we sent no body), but anything other than 404 proves
	// the route is wired correctly.
	if rec.Code == http.StatusNotFound {
		t.Error("POST /mcp returned 404 — route not registered")
	}
}

func TestNewHTTPServer_TimeoutsAreSet(t *testing.T) {
	// Asserts the production timeout posture: ReadHeaderTimeout / ReadTimeout
	// / IdleTimeout are all non-zero (close Slowloris-class and idle
	// keep-alive exhaustion vectors), and WriteTimeout is deliberately zero
	// (preserves long-lived MCP streamable HTTP / SSE responses).
	srv := newHTTPServer(":0", http.NewServeMux())

	if want := 10 * time.Second; srv.ReadHeaderTimeout != want {
		t.Errorf("ReadHeaderTimeout = %s, want %s", srv.ReadHeaderTimeout, want)
	}
	if want := 30 * time.Second; srv.ReadTimeout != want {
		t.Errorf("ReadTimeout = %s, want %s", srv.ReadTimeout, want)
	}
	if want := 120 * time.Second; srv.IdleTimeout != want {
		t.Errorf("IdleTimeout = %s, want %s", srv.IdleTimeout, want)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s, want 0 (intentional — preserves SSE streams)", srv.WriteTimeout)
	}
}

func TestUnknownRoute_Returns404(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /unknown status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestStartServer_PortConflict_SendsToChannel(t *testing.T) {
	// Occupy a port to force an "address already in use" startup error.
	lc := &net.ListenConfig{}
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	srv := &http.Server{
		Addr:    l.Addr().String(),
		Handler: http.NewServeMux(),
	}

	errCh := startServer(srv, "", "")

	select {
	case startErr := <-errCh:
		if startErr == nil {
			t.Fatal("expected error for port conflict, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: expected startup error to be sent to channel, not os.Exit")
	}
}

func TestStartServer_NormalShutdown_NoErrorSent(t *testing.T) {
	// Verify that ErrServerClosed (the result of a clean Shutdown) is filtered
	// out and not sent to the error channel.
	//
	// We call Shutdown before startServer so http.Server.ListenAndServe sees
	// shuttingDown=true on entry and returns ErrServerClosed without binding
	// any port. This exercises the same filter path without the port-reuse
	// race of binding-then-rebinding to the same address.
	srv := &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: http.NewServeMux(),
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	errCh := startServer(srv, "", "")

	select {
	case startErr := <-errCh:
		t.Errorf("expected no error for ErrServerClosed, got: %v", startErr)
	case <-time.After(200 * time.Millisecond):
		// expected: ListenAndServe returned ErrServerClosed and was filtered out
	}
}

func TestHealthConfigFromFile_ReadsPortFromYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Port != 8080 {
		t.Errorf("Port = %d, want 8080", got.Port)
	}
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", got.Scheme)
	}
}

func TestHealthConfigFromFile_ReturnsZeroPortWhenNoPortField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("log_level: info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
}

func TestHealthConfigFromFile_ReturnsDefaultsWhenNoConfigFound(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck

	got := healthConfigFromFile()
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", got.Scheme)
	}
}

func TestHealthConfigFromFile_ReturnsDefaultsOnInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("{{invalid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Port != 0 {
		t.Errorf("Port = %d, want 0", got.Port)
	}
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http", got.Scheme)
	}
}

func TestHealthConfigFromFile_LocalPathFallback(t *testing.T) {
	t.Setenv("CONFIG_FILE", "")
	origDir, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck

	if err := os.WriteFile("broker-config.yaml", []byte("port: 7070\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := healthConfigFromFile()
	if got.Port != 7070 {
		t.Errorf("Port = %d, want 7070", got.Port)
	}
}

func TestHealthConfigFromFile_DetectsTLS(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "port: 9090\ntls_cert_file: /etc/certs/server.pem\ntls_key_file: /etc/certs/server-key.pem\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", got.Scheme)
	}
	if got.Port != 9090 {
		t.Errorf("Port = %d, want 9090", got.Port)
	}
}

func TestHealthConfigFromFile_PartialTLSIsHTTP(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "port: 9090\ntls_cert_file: /etc/certs/server.pem\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.Scheme != "http" {
		t.Errorf("Scheme = %q, want http (only cert set, no key)", got.Scheme)
	}
}

func TestReady_POST_ReturnsMethodNotAllowed(t *testing.T) {
	mux := buildMux(health.NewReadinessState())
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/ready", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /ready status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestReady_BeforeInit_Returns503Starting pins that /ready is now a
// body-identical alias of /readyz: before SetInitialized it returns
// 503 {"status":"starting"}. The legacy broker-coupled behaviour (ready=true /
// per-broker errors array) is gone — /ready no longer dials any broker.
func TestReady_BeforeInit_Returns503Starting(t *testing.T) {
	readiness := health.NewReadinessState() // not initialized
	mux := buildMux(readiness)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /ready status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec.Body.String() != `{"status":"starting"}` {
		t.Errorf("GET /ready body = %q, want %q", rec.Body.String(), `{"status":"starting"}`)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET /ready Content-Type = %q, want %q", ct, "application/json")
	}
}

// TestReady_AfterInit_Returns200Ready pins that once SetInitialized has been
// called, /ready returns 200 {"status":"ready"} — the same MCP-server-only
// readiness signal as /readyz.
func TestReady_AfterInit_Returns200Ready(t *testing.T) {
	readiness := health.NewReadinessState()
	readiness.SetInitialized()
	mux := buildMux(readiness)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /ready status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"ready"}` {
		t.Errorf("GET /ready body = %q, want %q", rec.Body.String(), `{"status":"ready"}`)
	}
}

// TestReadyAndReadyzBodiesIdentical proves the alias contract: /ready and
// /readyz return byte-identical status and body for both the pre-init and the
// post-init states. They are the SAME handler, so a future change to readiness
// semantics applies to both endpoints automatically.
func TestReadyAndReadyzBodiesIdentical(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialized bool
	}{
		{"before init", false},
		{"after init", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			readiness := health.NewReadinessState()
			if tc.initialized {
				readiness.SetInitialized()
			}
			mux := buildMux(readiness)

			readyRec := httptest.NewRecorder()
			mux.ServeHTTP(readyRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/ready", nil))
			readyzRec := httptest.NewRecorder()
			mux.ServeHTTP(readyzRec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil))

			if readyRec.Code != readyzRec.Code {
				t.Errorf("status /ready=%d /readyz=%d, want equal", readyRec.Code, readyzRec.Code)
			}
			if readyRec.Body.String() != readyzRec.Body.String() {
				t.Errorf("body /ready=%q /readyz=%q, want identical", readyRec.Body.String(), readyzRec.Body.String())
			}
			if readyRec.Header().Get("Content-Type") != readyzRec.Header().Get("Content-Type") {
				t.Errorf("Content-Type /ready=%q /readyz=%q, want equal",
					readyRec.Header().Get("Content-Type"), readyzRec.Header().Get("Content-Type"))
			}
		})
	}
}

// TestReadyz_BeforeInit_Returns503 pins that /readyz wired through the real
// buildMux returns 503 {"status":"starting"} before SetInitialized.
func TestReadyz_BeforeInit_Returns503(t *testing.T) {
	readiness := health.NewReadinessState() // not initialized
	mux := buildMux(readiness)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec.Body.String() != `{"status":"starting"}` {
		t.Errorf("GET /readyz body = %q, want %q", rec.Body.String(), `{"status":"starting"}`)
	}
}

// TestReadyz_AfterInit_Returns200 pins that /readyz wired through the real
// buildMux returns 200 {"status":"ready"} once SetInitialized has been called.
func TestReadyz_AfterInit_Returns200(t *testing.T) {
	readiness := health.NewReadinessState()
	readiness.SetInitialized()
	mux := buildMux(readiness)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status":"ready"}` {
		t.Errorf("GET /readyz body = %q, want %q", rec.Body.String(), `{"status":"ready"}`)
	}
}
