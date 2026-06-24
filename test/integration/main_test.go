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

package integration_test

import (
	"crypto/x509"
	"os"
	"testing"
)

// TestMain seeds the process-global system certificate pool with a clean
// environment before any test in this package runs.
//
// On Linux (but not macOS), crypto/x509 reads SSL_CERT_FILE when it lazily
// loads the system roots, and caches the result process-wide via sync.Once
// (crypto/x509/root.go). The first trigger wins: whichever test first calls
// x509.SystemCertPool() — which idpclient.NewHTTPClient does on the
// SSL_CERT_FILE path — or performs a TLS handshake with nil RootCAs, seeds
// that cache with the SSL_CERT_FILE value in effect at that moment, for the
// rest of the test binary.
//
// Without this, Test_OIDCJWKSRefreshTimeout_SSLCertFile (which points
// SSL_CERT_FILE at a test CA via t.Setenv) would leak that CA into the
// global root cache. t.Setenv restores the env var on cleanup but does not
// reset the sync.Once, so the leaked CA persists for the lifetime of the
// test binary. Today no other test in this package asserts an untrusted
// server is rejected, so nothing breaks — but the next such test added to
// the integration tier would inherit an order-dependent Linux CI flake.
// Firing the sync.Once here with the env cleared makes those assertions
// deterministic regardless of test order. macOS ignores SSL_CERT_FILE for
// system roots, which is why the leak only surfaces in Linux CI.
//
// This mirrors the TestMain in internal/auth/middleware_test.go; the two
// guards stay in sync because they protect the same hazard from the same
// shared dependency on x509.SystemCertPool.
func TestMain(m *testing.M) {
	os.Unsetenv("SSL_CERT_FILE")
	os.Unsetenv("SSL_CERT_DIR")
	_, _ = x509.SystemCertPool()
	os.Exit(m.Run())
}
