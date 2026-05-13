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

	"github.com/SolaceDev/solace-broker-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testMux creates a mux using the shared buildMux function with a minimal
// MCP server. This ensures tests use the same route definitions as main().
func testMux() *http.ServeMux {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "solace-broker-mcp",
		Version: version.Version(),
	}, nil)
	mux := buildMux()

	// Register /mcp endpoint for testing (without auth middleware)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)
	mux.Handle("/mcp", mcpHandler)

	return mux
}

func TestHealth_GET_ReturnsOK(t *testing.T) {
	mux := testMux()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != `{"status": "ok"}` {
		t.Errorf("GET /health body = %q, want %q", rec.Body.String(), `{"status": "ok"}`)
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
