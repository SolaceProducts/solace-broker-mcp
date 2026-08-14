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
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
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

// writeTestServerCert generates a self-signed certificate with the given SANs,
// writes it to a PEM file, and returns the file path plus the keypair for
// serving. The SANs are chosen by the caller so the --health probe tests can
// pin certificates that deliberately do not cover "localhost" — the SAN
// mismatch that used to justify skipping verification altogether (SOL-153167).
func writeTestServerCert(t *testing.T, dnsSANs []string, ipSANs []net.IP) (certPath string, keypair tls.Certificate) {
	t.Helper()
	return writeTestCert(t, dnsSANs, ipSANs, true)
}

// writeTestServerLeaf generates the ordinary production shape — a self-signed
// serving certificate that is not itself a CA — so the pinning path is proven
// against both shapes rather than only the CA-flagged one.
func writeTestServerLeaf(t *testing.T, dnsSAN string) (certPath string, keypair tls.Certificate) {
	t.Helper()
	return writeTestCert(t, []string{dnsSAN}, nil, false)
}

func writeTestCert(t *testing.T, dnsSANs []string, ipSANs []net.IP, isCA bool) (certPath string, keypair tls.Certificate) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "solace-broker-mcp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsSANs,
		IPAddresses:  ipSANs,
		// Self-signed leaf pinned as its own root: Go accepts a chain whose leaf
		// is present in the root pool. isCA selects which of the two real-world
		// shapes this cert takes — writeTestServerLeaf covers the non-CA one.
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPath = filepath.Join(t.TempDir(), "server.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return certPath, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// TestHealthProbeTLSConfig_PinsCertAndTakesServerNameFromDNSSAN is the core of
// SOL-153167: the probe verifies the server's certificate against a pool
// containing that certificate, and borrows the SAN as the verification
// hostname, rather than switching verification off.
func TestHealthProbeTLSConfig_PinsCertAndTakesServerNameFromDNSSAN(t *testing.T) {
	certPath, _ := writeTestServerCert(t, []string{"broker.internal.example"}, nil)

	got, err := healthProbeTLSConfig(certPath)
	if err != nil {
		t.Fatalf("healthProbeTLSConfig: unexpected error: %v", err)
	}
	if got.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false (certificate verification must stay on)")
	}
	if got.RootCAs == nil {
		t.Error("RootCAs = nil, want a pool containing the pinned server certificate")
	}
	if got.ServerName != "broker.internal.example" {
		t.Errorf("ServerName = %q, want %q (the cert's DNS SAN)", got.ServerName, "broker.internal.example")
	}
}

// TestHealthProbeTLSConfig_FallsBackToIPSAN covers a cert issued to an address
// rather than a name, which is common for brokers behind an IP-only listener.
func TestHealthProbeTLSConfig_FallsBackToIPSAN(t *testing.T) {
	certPath, _ := writeTestServerCert(t, nil, []net.IP{net.ParseIP("10.1.2.3")})

	got, err := healthProbeTLSConfig(certPath)
	if err != nil {
		t.Fatalf("healthProbeTLSConfig: unexpected error: %v", err)
	}
	if got.ServerName != "10.1.2.3" {
		t.Errorf("ServerName = %q, want %q (the cert's IP SAN)", got.ServerName, "10.1.2.3")
	}
}

func TestHealthProbeTLSConfig_ErrorsWhenCertMissing(t *testing.T) {
	_, err := healthProbeTLSConfig(filepath.Join(t.TempDir(), "absent.pem"))
	if err == nil {
		t.Fatal("healthProbeTLSConfig(missing file) = nil error, want an error")
	}
}

func TestHealthProbeTLSConfig_ErrorsOnMalformedPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := healthProbeTLSConfig(path)
	if err == nil {
		t.Fatal("healthProbeTLSConfig(malformed PEM) = nil error, want an error")
	}
}

// TestHealthProbeTLSConfig_ErrorsWhenCertHasNoSAN guards the case where no
// verification hostname can be derived: better to fail the probe than to fall
// back to unverified TLS.
func TestHealthProbeTLSConfig_ErrorsWhenCertHasNoSAN(t *testing.T) {
	certPath, _ := writeTestServerCert(t, nil, nil)

	_, err := healthProbeTLSConfig(certPath)
	if err == nil {
		t.Fatal("healthProbeTLSConfig(cert with no SAN) = nil error, want an error")
	}
}

// TestProbeHealth_SucceedsOverTLSWithNonLocalhostSAN is the end-to-end proof: a
// real TLS handshake against a server whose certificate does not cover
// "localhost" succeeds with verification enabled, because the pinned cert and
// its SAN are used instead.
func TestProbeHealth_SucceedsOverTLSWithNonLocalhostSAN(t *testing.T) {
	certPath, keypair := writeTestServerCert(t, []string{"broker.internal.example"}, nil)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	tlsCfg, err := healthProbeTLSConfig(certPath)
	if err != nil {
		t.Fatalf("healthProbeTLSConfig: unexpected error: %v", err)
	}

	if err := probeHealth(srv.URL+"/health", tlsCfg); err != nil {
		t.Errorf("probeHealth: unexpected error: %v", err)
	}
}

// TestProbeHealth_FailsWhenCertNotPinned proves verification is genuinely
// enforced: the same server, probed without the pin, must fail. If this test
// passes while the previous one does too, the pin is doing the work.
func TestProbeHealth_FailsWhenCertNotPinned(t *testing.T) {
	_, keypair := writeTestServerCert(t, []string{"broker.internal.example"}, nil)

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	unpinned := &tls.Config{ServerName: "broker.internal.example", MinVersion: tls.VersionTLS12}

	if err := probeHealth(srv.URL+"/health", unpinned); err == nil {
		t.Error("probeHealth = nil error, want failure (unknown authority must not verify)")
	}
}

// TestProbeHealth_FailsOnNonOKStatus keeps the liveness contract: only 200 means
// healthy.
func TestProbeHealth_FailsOnNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	if err := probeHealth(srv.URL+"/health", nil); err == nil {
		t.Error("probeHealth = nil error, want failure on 503")
	}
}

// TestProbeHealth_SucceedsOverPlaintext pins the default deployment shape: no TLS
// configured, no TLS client config, still healthy.
func TestProbeHealth_SucceedsOverPlaintext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := probeHealth(srv.URL+"/health", nil); err != nil {
		t.Errorf("probeHealth: unexpected error: %v", err)
	}
}

// TestHealthConfigFromFile_RetainsCertPath covers the plumbing the probe needs:
// the cert path itself, not just the derived scheme.
func TestHealthConfigFromFile_RetainsCertPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "port: 9090\ntls_cert_file: /etc/certs/server.pem\ntls_key_file: /etc/certs/server-key.pem\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.CertFile != "/etc/certs/server.pem" {
		t.Errorf("CertFile = %q, want /etc/certs/server.pem", got.CertFile)
	}
}

// writeTestCAChain generates a CA and a leaf signed by it, writes the leaf and
// the CA into one PEM bundle (the shape cert-manager and corporate PKI produce),
// and returns the bundle path, the leaf's serving keypair, and a second leaf from
// the same CA carrying the same SAN — the impostor a chain-anchored trust pool
// would wrongly accept.
func writeTestCAChain(t *testing.T, dnsSAN string) (bundlePath string, genuine, impostor tls.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	issue := func(serial int64) tls.Certificate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate leaf key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: "solace-broker-mcp-test"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:     []string{dnsSAN},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create leaf: %v", err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parse leaf: %v", err)
		}
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key, Leaf: leaf}
	}

	genuine = issue(101)
	impostor = issue(102)

	bundlePath = filepath.Join(t.TempDir(), "chain.pem")
	bundle := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: genuine.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})...,
	)
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		t.Fatalf("write chain bundle: %v", err)
	}
	return bundlePath, genuine, impostor
}

// TestHealthProbeTLSConfig_ChainFilePinsLeafNotCA is the regression test for the
// difference between pinning a certificate and trusting its issuer. When
// tls_cert_file is a leaf+CA bundle, anchoring every certificate in the file
// would make any certificate that CA ever issued acceptable — including one an
// impersonator obtained legitimately with the same SAN. Only the configured leaf
// may verify.
func TestHealthProbeTLSConfig_ChainFilePinsLeafNotCA(t *testing.T) {
	bundlePath, genuine, impostor := writeTestCAChain(t, "broker.internal.example")

	tlsCfg, err := healthProbeTLSConfig(bundlePath)
	if err != nil {
		t.Fatalf("healthProbeTLSConfig: unexpected error: %v", err)
	}

	serve := func(keypair tls.Certificate) string {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
		srv.StartTLS()
		t.Cleanup(srv.Close)
		return srv.URL + "/health"
	}

	if err := probeHealth(serve(genuine), tlsCfg.Clone()); err != nil {
		t.Errorf("genuine leaf from chain bundle: unexpected error: %v", err)
	}
	if err := probeHealth(serve(impostor), tlsCfg.Clone()); err == nil {
		t.Error("impostor leaf from the same CA verified; only the configured leaf may")
	}
}

// TestHealthProbeTLSConfig_AcceptsNonCALeaf covers the ordinary production shape:
// a server certificate that is not itself a CA. Pinning must still verify it.
func TestHealthProbeTLSConfig_AcceptsNonCALeaf(t *testing.T) {
	certPath, keypair := writeTestServerLeaf(t, "broker.internal.example")

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	tlsCfg, err := healthProbeTLSConfig(certPath)
	if err != nil {
		t.Fatalf("healthProbeTLSConfig: unexpected error: %v", err)
	}
	if err := probeHealth(srv.URL+"/health", tlsCfg); err != nil {
		t.Errorf("non-CA self-signed leaf: unexpected error: %v", err)
	}
}

// TestHealthConfigFromFile_SubstitutesEnvVarsInCertPath pins the probe to the
// same config normalization LoadConfig applies. ${VAR} references are documented
// as usable anywhere in the YAML, so a cert path written that way must resolve
// for the probe exactly as it does for the server.
func TestHealthConfigFromFile_SubstitutesEnvVarsInCertPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "port: 9090\ntls_cert_file: \"${TEST_TLS_CERT}\"\ntls_key_file: \"${TEST_TLS_KEY}\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)
	t.Setenv("TEST_TLS_CERT", "/etc/certs/server.pem")
	t.Setenv("TEST_TLS_KEY", "/etc/certs/server-key.pem")

	got := healthConfigFromFile()
	if got.CertFile != "/etc/certs/server.pem" {
		t.Errorf("CertFile = %q, want /etc/certs/server.pem (${VAR} must be substituted)", got.CertFile)
	}
	if got.Scheme != "https" {
		t.Errorf("Scheme = %q, want https", got.Scheme)
	}
}

// TestHealthConfigFromFile_TrimsCertPath matches LoadConfig, which trims both TLS
// paths before any downstream reader sees them.
func TestHealthConfigFromFile_TrimsCertPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "tls_cert_file: \"  /etc/certs/server.pem  \"\ntls_key_file: \"  /etc/certs/server-key.pem  \"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)

	got := healthConfigFromFile()
	if got.CertFile != "/etc/certs/server.pem" {
		t.Errorf("CertFile = %q, want the trimmed path", got.CertFile)
	}
}

// TestHealthExitCode_HTTPSPinsConfiguredCert closes the wiring gap: nothing
// previously asserted that an https healthConfig actually causes the certificate
// at CertFile to be pinned, because that decision lived in main().
func TestHealthExitCode_HTTPSPinsConfiguredCert(t *testing.T) {
	certPath, keypair := writeTestServerLeaf(t, "broker.internal.example")

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{keypair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	hc := healthConfig{Scheme: "https", CertFile: certPath}
	if code := healthExitCode(hc, srv.URL+"/health", io.Discard); code != 0 {
		t.Errorf("healthExitCode = %d, want 0", code)
	}

	// Same server, but the configured cert path is one the probe cannot use:
	// the https branch must fail rather than fall back to unverified TLS.
	hcBad := healthConfig{Scheme: "https", CertFile: filepath.Join(t.TempDir(), "absent.pem")}
	if code := healthExitCode(hcBad, srv.URL+"/health", io.Discard); code != 1 {
		t.Errorf("healthExitCode with unusable cert = %d, want 1", code)
	}
}

// TestHealthExitCode_HTTPNeedsNoCert pins the default deployment shape: the http
// branch must not touch CertFile at all.
func TestHealthExitCode_HTTPNeedsNoCert(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hc := healthConfig{Scheme: "http", CertFile: "/nonexistent/should-not-be-read.pem"}
	if code := healthExitCode(hc, srv.URL+"/health", io.Discard); code != 0 {
		t.Errorf("healthExitCode = %d, want 0", code)
	}
}

// TestHealthExitCode_ReportsCertFailureReason keeps the operator's only diagnostic
// channel non-empty: Docker surfaces the probe's output in State.Health.Log, so a
// silent exit 1 makes "cert unreadable" indistinguishable from "server down".
func TestHealthExitCode_ReportsCertFailureReason(t *testing.T) {
	var out bytes.Buffer
	hc := healthConfig{Scheme: "https", CertFile: filepath.Join(t.TempDir(), "absent.pem")}

	if code := healthExitCode(hc, "https://localhost:9090/health", &out); code != 1 {
		t.Fatalf("healthExitCode = %d, want 1", code)
	}
	if out.Len() == 0 {
		t.Fatal("no diagnostic written; Docker health log would be empty")
	}
	if !strings.Contains(out.String(), "absent.pem") {
		t.Errorf("diagnostic %q does not name the certificate file", out.String())
	}
}

// TestHealthExitCode_ReportsNonOKStatus distinguishes an unhealthy server from an
// unreachable one in the same channel.
func TestHealthExitCode_ReportsNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var out bytes.Buffer
	if code := healthExitCode(healthConfig{Scheme: "http"}, srv.URL+"/health", &out); code != 1 {
		t.Fatalf("healthExitCode = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "503") {
		t.Errorf("diagnostic %q does not report the status code", out.String())
	}
}

// TestHealthProbeTimeout_IsUnderDockerHealthcheckTimeout keeps the probe's own
// deadline strictly inside the Dockerfile HEALTHCHECK timeout, parsed from the
// Dockerfile itself so the two cannot drift apart.
//
// Whichever deadline fires first decides the outcome, and they must not be the
// same: when Docker kills the probe the exit is silent, whereas when the probe
// times out itself it writes the reason to stderr and Docker records it in
// State.Health.Log. On a hung server — precisely when an operator needs the
// diagnostic — an equal deadline turns that into a coin flip.
func TestHealthProbeTimeout_IsUnderDockerHealthcheckTimeout(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	m := regexp.MustCompile(`HEALTHCHECK[^\n]*--timeout=(\d+)s`).FindSubmatch(data)
	if m == nil {
		t.Fatal("no HEALTHCHECK --timeout=<N>s found in Dockerfile; keep this test and the Dockerfile in sync")
	}
	seconds, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parse Dockerfile timeout %q: %v", m[1], err)
	}
	dockerTimeout := time.Duration(seconds) * time.Second

	if healthProbeTimeout >= dockerTimeout {
		t.Errorf("healthProbeTimeout = %v, want strictly less than the Dockerfile HEALTHCHECK timeout of %v, so the probe reports its own failure instead of being killed silently", healthProbeTimeout, dockerTimeout)
	}
}

// TestRegisterMetadataRoutes exercises the mux wiring for PRM (RFC 9728):
// both the bare path and the §3.1 canonical path (derived from resource_url)
// must return the same document. Non-oauth modes register nothing.
func TestRegisterMetadataRoutes(t *testing.T) {
	oauthCfg := func(resourceURL string) *config.ServerConfig {
		return &config.ServerConfig{
			Port: 9090,
			MCPClientAuth: config.MCPClientAuthConfig{
				Mode:        config.AuthModeOAuth,
				Issuer:      "https://auth.example.com",
				Audience:    "solace-mcp-server",
				ResourceURL: resourceURL,
			},
		}
	}

	tests := []struct {
		name        string
		cfg         *config.ServerConfig
		bareStatus  int
		canonPath   string // "" means the canonical route should not be registered
		canonStatus int
	}{
		{
			name:        "oauth mode — both paths registered",
			cfg:         oauthCfg("http://localhost:9090/mcp"),
			bareStatus:  http.StatusOK,
			canonPath:   "/.well-known/oauth-protected-resource/mcp",
			canonStatus: http.StatusOK,
		},
		{
			// Pins that the canonical path is derived from resource_url, not
			// hardcoded to /mcp — regressing the derivation would fail here.
			name:        "path prefix from ingress — canonical follows resource_url path",
			cfg:         oauthCfg("https://gateway.example.com/broker/mcp"),
			bareStatus:  http.StatusOK,
			canonPath:   "/.well-known/oauth-protected-resource/broker/mcp",
			canonStatus: http.StatusOK,
		},
		{
			name:       "empty path — canonical collides with bare, skip",
			cfg:        oauthCfg("https://mcp.example.com"),
			bareStatus: http.StatusOK,
		},
		{
			name:       "static mode — no registration",
			cfg:        &config.ServerConfig{MCPClientAuth: config.MCPClientAuthConfig{Mode: config.AuthModeStatic}},
			bareStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := http.NewServeMux()
			registerMetadataRoutes(mux, tt.cfg)

			bareRec := httptest.NewRecorder()
			bareReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/.well-known/oauth-protected-resource", nil)
			mux.ServeHTTP(bareRec, bareReq)
			if bareRec.Code != tt.bareStatus {
				t.Errorf("bare path status = %d, want %d", bareRec.Code, tt.bareStatus)
			}

			if tt.canonPath == "" {
				return
			}

			canonRec := httptest.NewRecorder()
			canonReq := httptest.NewRequestWithContext(context.Background(), http.MethodGet, tt.canonPath, nil)
			mux.ServeHTTP(canonRec, canonReq)
			if canonRec.Code != tt.canonStatus {
				t.Errorf("canonical path status = %d, want %d", canonRec.Code, tt.canonStatus)
			}
			if bareRec.Body.String() != canonRec.Body.String() {
				t.Errorf("body mismatch: bare=%q canonical=%q", bareRec.Body.String(), canonRec.Body.String())
			}
		})
	}
}
