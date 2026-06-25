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

package idpclient

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// defaultTransportMu serializes any test that mutates http.DefaultTransport.
// Tests that need a custom transport (today: only the type-assertion
// failure-path test below) must Lock before assigning and Unlock after
// restore — see TestNewHTTPClient_DefaultTransportNotHTTPTransport for the
// pattern. The default Go test runner runs in-package tests sequentially,
// so the mutex is belt-and-suspenders against a future contributor adding
// t.Parallel() to a sibling test that also touches the transport.
var defaultTransportMu sync.Mutex

// TestNewHTTPClient_SSLCertFile covers the five SSL_CERT_FILE branches
// (unset, missing file, unreadable file, garbage contents, valid PEM) and
// asserts the SOL-150219 invariant: every returned client carries the
// default timeout when no Options are passed.
//
// The "valid PEM" case points SSL_CERT_FILE at a test CA via t.Setenv,
// which would otherwise leak that CA into the process-wide x509 root
// cache via the SystemCertPool sync.Once. Test-binary-wide isolation is
// set up by TestMain in main_test.go — see the doc comment there for
// the full hazard description. The end-to-end propagation
// of the bound through go-oidc's RemoteKeySet into lazy JWKS refresh is
// covered by an integration test in test/integration/ that exercises the
// auth middleware against a hung fake IdP.
func TestNewHTTPClient_SSLCertFile(t *testing.T) {
	// Spin up a throwaway TLS server purely to obtain a self-signed cert we
	// can write to disk and feed into the "valid PEM" branch. We never make
	// a request through this server here.
	tlsServer := httptest.NewTLSServer(nil)
	defer tlsServer.Close()

	tmpDir := t.TempDir()
	validCert := filepath.Join(tmpDir, "ca.crt")
	if err := os.WriteFile(
		validCert,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}),
		0600,
	); err != nil {
		t.Fatalf("write valid cert: %v", err)
	}

	garbageFile := filepath.Join(tmpDir, "garbage.crt")
	if err := os.WriteFile(garbageFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatalf("write garbage file: %v", err)
	}

	unreadableFile := filepath.Join(tmpDir, "unreadable.crt")
	if err := os.WriteFile(unreadableFile, []byte("data"), 0000); err != nil {
		t.Fatalf("write unreadable file: %v", err)
	}

	// NewHTTPClient always returns a bounded client on success, including
	// when SSL_CERT_FILE is unset, so the JWKS-refresh timeout (SOL-150219)
	// applies on every code path. It returns (nil, err) only on a
	// cert-loading failure.
	tests := []struct {
		name      string
		envValue  string
		wantNil   bool
		wantError bool
	}{
		{"env empty", "", false, false},
		{"nonexistent file", "/no/such/file.crt", true, true},
		{"unreadable file", unreadableFile, true, true},
		{"non-PEM contents", garbageFile, true, true},
		{"valid PEM", validCert, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SSL_CERT_FILE", tt.envValue)
			client, err := NewHTTPClient()
			if tt.wantError && err == nil {
				t.Error("expected error")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if tt.wantNil && client != nil {
				t.Error("expected nil client")
			}
			if !tt.wantNil && client == nil {
				t.Error("expected non-nil client")
			}
			// Invariant (SOL-150219): every client NewHTTPClient hands back
			// must carry the default bound when no Options are passed. This
			// is the cheap guard against anyone reintroducing an unbounded
			// IdP client on either branch.
			if client != nil && client.Timeout != defaults.DefaultOIDCHTTPTimeout {
				t.Errorf("client.Timeout = %v, want %v (IdP client must carry default bound)",
					client.Timeout, defaults.DefaultOIDCHTTPTimeout)
			}
		})
	}
}

// TestNewHTTPClient_WithTimeout asserts that the WithTimeout Option
// overrides the default, and that no Option leaves the default in place.
// These are the call-shape contracts every integration test depending on
// fast-feedback timeouts relies on.
func TestNewHTTPClient_WithTimeout(t *testing.T) {
	t.Run("default applies when no options", func(t *testing.T) {
		c, err := NewHTTPClient()
		if err != nil {
			t.Fatalf("NewHTTPClient: %v", err)
		}
		if c.Timeout != defaults.DefaultOIDCHTTPTimeout {
			t.Errorf("Timeout = %v, want default %v", c.Timeout, defaults.DefaultOIDCHTTPTimeout)
		}
	})

	t.Run("WithTimeout overrides the default", func(t *testing.T) {
		c, err := NewHTTPClient(WithTimeout(500 * time.Millisecond))
		if err != nil {
			t.Fatalf("NewHTTPClient: %v", err)
		}
		if c.Timeout != 500*time.Millisecond {
			t.Errorf("Timeout = %v, want 500ms", c.Timeout)
		}
	})

	// Non-positive timeouts must fail loudly. Zero is the dangerous case:
	// http.Client treats Timeout: 0 as "no bound at all" — silently
	// undoing the SOL-150219 protection. Negative is an obvious caller
	// bug (immediate-deadline on every request).
	t.Run("WithTimeout(0) returns error", func(t *testing.T) {
		c, err := NewHTTPClient(WithTimeout(0))
		if err == nil {
			t.Error("expected error for zero timeout")
		}
		if c != nil {
			t.Error("expected nil client when constructor errors")
		}
	})

	t.Run("WithTimeout(negative) returns error", func(t *testing.T) {
		c, err := NewHTTPClient(WithTimeout(-1 * time.Second))
		if err == nil {
			t.Error("expected error for negative timeout")
		}
		if c != nil {
			t.Error("expected nil client when constructor errors")
		}
	})
}

// fakeRoundTripper satisfies http.RoundTripper without being *http.Transport.
// Used to drive the type-assertion failure path in newBaseTransport.
type fakeRoundTripper struct{}

func (fakeRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// TestNewHTTPClient_DefaultTransportNotHTTPTransport pins the safe
// type-assertion behavior of newBaseTransport. http.DefaultTransport is a
// public package var declared as RoundTripper; tests, observability
// auto-instrumentation, and some libraries (go-vcr, OpenTelemetry
// middleware) replace it with their own concrete type. When that happens
// NewHTTPClient must return a clear error rather than panicking with a
// runtime.TypeAssertionError deep inside startup.
//
// defaultTransportMu serializes against any future test that also mutates
// the package var; t.Cleanup restores the original before unlock so
// observers always see either pre-swap or post-restore, never a torn
// state. The mutex is overkill against today's package state (no test
// calls t.Parallel) but cheap insurance against a future contributor
// adding parallelism to a sibling test.
func TestNewHTTPClient_DefaultTransportNotHTTPTransport(t *testing.T) {
	defaultTransportMu.Lock()
	orig := http.DefaultTransport
	// Restore + unlock as a single deferred unit so the lock is never
	// released while the fake transport is still in place. t.Cleanup
	// runs after deferred functions, which would invert the ordering
	// and open a parallel-test race window.
	defer func() {
		http.DefaultTransport = orig
		defaultTransportMu.Unlock()
	}()

	http.DefaultTransport = fakeRoundTripper{}

	c, err := NewHTTPClient()
	if err == nil {
		t.Fatal("expected error when DefaultTransport is not *http.Transport")
	}
	if c != nil {
		t.Errorf("expected nil client on assertion failure, got %#v", c)
	}
	if !strings.Contains(err.Error(), "http.DefaultTransport") {
		t.Errorf("error should mention http.DefaultTransport, got: %v", err)
	}
}
