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

package resilience

import (
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// TestNewTunedTransport_GranularTimeoutsAreSet locks in the production
// timeout posture for outbound SEMP transports: TLSHandshakeTimeout,
// ResponseHeaderTimeout, and ExpectContinueTimeout are all set, so a broker
// stuck in handshake or one that accepts the connection then never sends
// headers fails fast rather than holding a MaxConcurrentPerBroker semaphore
// slot for the full client-level request timeout.
//
// ResponseHeaderTimeout is derived from sempCfg.RequestTimeoutDuration so the
// "strictly less than the outer client timeout" relationship is preserved
// even when an operator customizes request_timeout_duration in broker config.
func TestNewTunedTransport_GranularTimeoutsAreSet(t *testing.T) {
	brokerCfg := &config.BrokerConfig{
		URL:                "https://broker.example.com:1943",
		InsecureSkipVerify: false,
	}
	sempCfg := &config.SEMPConfig{
		MaxConcurrentPerBroker: 10,
		RequestTimeoutDuration: defaults.DefaultSEMPRequestTimeoutDuration,
	}

	tr := NewTunedTransport(brokerCfg, sempCfg)

	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %s, want %s", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
	}
	wantHdr := sempCfg.RequestTimeoutDuration / 2
	if tr.ResponseHeaderTimeout != wantHdr {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, wantHdr)
	}
	if tr.ResponseHeaderTimeout >= sempCfg.RequestTimeoutDuration {
		t.Errorf("ResponseHeaderTimeout (%s) must be strictly less than RequestTimeoutDuration (%s); the granular timeout will never fire", tr.ResponseHeaderTimeout, sempCfg.RequestTimeoutDuration)
	}
	if tr.ExpectContinueTimeout != expectContinueTimeout {
		t.Errorf("ExpectContinueTimeout = %s, want %s", tr.ExpectContinueTimeout, expectContinueTimeout)
	}
}

// TestResponseHeaderTimeout_TracksOperatorConfiguredRequestTimeout verifies
// the derived relationship still holds when an operator sets an aggressive
// request_timeout_duration (e.g. 10s) — the regression the PR fixes (a stuck
// broker holding a semaphore slot for the full outer timeout) must not
// silently come back because the granular timeout was hardcoded.
func TestResponseHeaderTimeout_TracksOperatorConfiguredRequestTimeout(t *testing.T) {
	brokerCfg := &config.BrokerConfig{URL: "https://broker.example.com:1943"}
	sempCfg := &config.SEMPConfig{
		MaxConcurrentPerBroker: 10,
		RequestTimeoutDuration: 10 * time.Second,
	}

	tr := NewTunedTransport(brokerCfg, sempCfg)

	if tr.ResponseHeaderTimeout >= sempCfg.RequestTimeoutDuration {
		t.Errorf("ResponseHeaderTimeout (%s) must be strictly less than RequestTimeoutDuration (%s)", tr.ResponseHeaderTimeout, sempCfg.RequestTimeoutDuration)
	}
	if want := 5 * time.Second; tr.ResponseHeaderTimeout != want {
		t.Errorf("ResponseHeaderTimeout = %s, want %s (half of operator-configured request timeout)", tr.ResponseHeaderTimeout, want)
	}
}

// TestNewTunedTransport_MaxConnsPerHostEnforcesConcurrencyCap verifies the
// per-broker in-flight bound is actually enforced at the transport. The
// transport supplies a custom TLSClientConfig, which disables Go's automatic
// HTTP/2, so SEMP traffic is HTTP/1.1 and one connection carries exactly one
// in-flight request — MaxConnsPerHost is therefore a true concurrency cap.
// Without it, a burst of tool calls against a slow broker opens unbounded
// TCP+TLS connections to the management plane.
func TestNewTunedTransport_MaxConnsPerHostEnforcesConcurrencyCap(t *testing.T) {
	brokerCfg := &config.BrokerConfig{URL: "https://broker.example.com:1943"}
	sempCfg := &config.SEMPConfig{
		MaxConcurrentPerBroker: 7,
		RequestTimeoutDuration: defaults.DefaultSEMPRequestTimeoutDuration,
	}

	tr := NewTunedTransport(brokerCfg, sempCfg)

	if tr.MaxConnsPerHost != sempCfg.MaxConcurrentPerBroker {
		t.Errorf("MaxConnsPerHost = %d, want %d (per-broker concurrency cap unenforced)",
			tr.MaxConnsPerHost, sempCfg.MaxConcurrentPerBroker)
	}
	if tr.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 = true would multiplex streams over one connection, breaking the MaxConnsPerHost concurrency bound")
	}
}
