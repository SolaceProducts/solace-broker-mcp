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
	"net/http"
	"reflect"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
	"github.com/SolaceProducts/solace-broker-mcp/internal/idpclient"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
)

// TestProxyEnvironmentInvariantAcrossSubsystems ties the server's outbound HTTP
// paths to one invariant: every one of them honours HTTP_PROXY/HTTPS_PROXY/
// NO_PROXY (SOL-153295). Before that change the SEMP transport did not, while
// the IdP client did, and nothing documented or enforced either half — so an
// operator setting HTTPS_PROXY got a half-proxied server. The per-package tests
// cover each site's own construction; this one exists because the invariant is
// cross-cutting and was, until now, carried only by prose in
// docs/configuration.md § "Outbound HTTP Proxy".
//
// Asserted by function identity rather than by behaviour, for the reason given
// in resilience.TestNewTunedTransport_HonorsProxyEnvironment: net/http caches
// the proxy environment behind a sync.Once, so an env-driven assertion would
// depend on test ordering across the whole binary.
//
// Two ways this fails, both of which have a plausible path into the codebase:
// someone reverts the SEMP transport to a bare struct literal while tuning
// connection pooling (a nil Proxy reads as a default, not as an opt-out); or
// someone stops idpclient cloning http.DefaultTransport — the only reason the
// IdP honours the variables — to gain the same explicit control the SEMP
// transport has.
func TestProxyEnvironmentInvariantAcrossSubsystems(t *testing.T) {
	wantProxy := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()

	t.Run("broker SEMP transport", func(t *testing.T) {
		tr := resilience.NewTunedTransport(
			&config.BrokerConfig{URL: "https://broker.example.com:1943"},
			&config.SEMPConfig{
				MaxConcurrentPerBroker: defaults.DefaultMaxConcurrentPerBroker,
				RequestTimeoutDuration: defaults.DefaultSEMPRequestTimeoutDuration,
			},
		)
		assertHonorsProxyEnv(t, tr, wantProxy, "broker SEMP traffic would silently ignore HTTP_PROXY/HTTPS_PROXY")
	})

	t.Run("IdP client transport", func(t *testing.T) {
		client, err := idpclient.NewHTTPClient()
		if err != nil {
			t.Fatalf("idpclient.NewHTTPClient(): %v", err)
		}
		tr, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("idpclient transport is %T, not *http.Transport; this test can no longer verify the invariant", client.Transport)
		}
		assertHonorsProxyEnv(t, tr, wantProxy, "OIDC discovery, JWKS refresh, and token exchange would silently ignore HTTPS_PROXY")
	})

	// The third outbound path, OTLP telemetry export, is deliberately not
	// asserted here: it is gRPC, and grpc-go resolves the proxy itself inside
	// its delegating resolver rather than through an *http.Transport this test
	// could inspect. It synthesizes an "https" target, so HTTPS_PROXY governs
	// export even when OTEL_EXPORTER_OTLP_ENDPOINT is http:// — documented in
	// docs/configuration.md § "Outbound HTTP Proxy". Named here so a reader
	// sees it was considered rather than missed. It would break only if the
	// exporters started passing grpc.WithNoProxy() or a custom dialer, which
	// internal/observability/{tracing,metrics} do not.
}

// assertHonorsProxyEnv checks that tr resolves its proxy through
// http.ProxyFromEnvironment. impact states what breaks for an operator when it
// does not, since a bare "Proxy = nil" says nothing about the consequence.
func assertHonorsProxyEnv(t *testing.T, tr *http.Transport, wantProxy uintptr, impact string) {
	t.Helper()
	if tr.Proxy == nil {
		t.Fatalf("Proxy = nil: a nil Proxy in net/http means never proxy, not consult the environment — %s", impact)
	}
	if reflect.ValueOf(tr.Proxy).Pointer() != wantProxy {
		t.Errorf("Proxy is not http.ProxyFromEnvironment — %s", impact)
	}
}
