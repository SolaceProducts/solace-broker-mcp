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

// Package idpclient builds the HTTP clients used to talk to identity
// providers. Single source for the shared TLS root configuration
// (SSL_CERT_FILE escape hatch) and the timeout bound (SOL-150219), used
// today by internal/auth's OIDC token verifier and tomorrow by the
// SOL-150799 RFC 8693 token exchange.
//
// Transport construction is package-private so consumers never assemble
// parts themselves; newBaseTransport is the seam where a future
// NewMTLSHTTPClient sibling will plug in (mTLS authenticates only the
// IdP's token endpoint; JWKS is always public).
package idpclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// config holds the per-call configuration applied by NewHTTPClient.
// Unexported; callers vary it through Options.
type config struct {
	timeout time.Duration
}

// Option configures a single NewHTTPClient call.
type Option func(*config)

// WithTimeout overrides the default HTTP client timeout. Production
// callers do not use this — tests pass a short value to assert
// SOL-150219 regression coverage in bounded wall-clock.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

// newBaseTransport returns a fresh *http.Transport carrying the shared TLS
// root configuration: the system trust store plus, when SSL_CERT_FILE is
// set, the PEM bundle that file points at. On macOS and Windows, Go's
// default TLS verification delegates to the OS-native trust store and
// ignores SSL_CERT_FILE; an explicit RootCAs pool bypasses that
// delegation so the env var works consistently on every platform.
func newBaseTransport() (*http.Transport, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()

	certFile := os.Getenv("SSL_CERT_FILE")
	if certFile == "" {
		return t, nil
	}
	certPEM, err := os.ReadFile(filepath.Clean(certFile))
	if err != nil {
		return nil, fmt.Errorf("SSL_CERT_FILE %q: %w", certFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, fmt.Errorf("SSL_CERT_FILE %q contains no valid PEM certificates", certFile)
	}
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	return t, nil
}

// NewHTTPClient returns an HTTP client for talking to identity provider
// endpoints that require no client authentication on the transport: OIDC
// discovery, JWKS refresh, and RFC 8693 token exchange with body/header
// client-auth methods. The returned client is always bounded by
// defaults.DefaultOIDCHTTPTimeout (SOL-150219); pass WithTimeout to
// override. Production callers pass no Options.
//
// Returns an error when the resolved timeout is non-positive — stdlib's
// http.Client treats a zero Timeout as "no bound at all", which would
// silently undo the SOL-150219 protection. A negative value triggers an
// immediate-deadline failure on every request, also an obvious caller bug.
func NewHTTPClient(opts ...Option) (*http.Client, error) {
	cfg := &config{timeout: defaults.DefaultOIDCHTTPTimeout}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.timeout <= 0 {
		return nil, fmt.Errorf("idpclient: timeout must be positive, got %v", cfg.timeout)
	}
	transport, err := newBaseTransport()
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: cfg.timeout}, nil
}
