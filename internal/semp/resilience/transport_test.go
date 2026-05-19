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
func TestNewTunedTransport_GranularTimeoutsAreSet(t *testing.T) {
	brokerCfg := &config.BrokerConfig{
		URL:                "https://broker.example.com:1943",
		InsecureSkipVerify: false,
	}
	sempCfg := &config.SEMPConfig{
		MaxConcurrentPerBroker: 10,
	}

	tr := NewTunedTransport(brokerCfg, sempCfg)

	wantTLS := time.Duration(defaults.DefaultTLSHandshakeTimeoutSeconds) * time.Second
	if tr.TLSHandshakeTimeout != wantTLS {
		t.Errorf("TLSHandshakeTimeout = %s, want %s", tr.TLSHandshakeTimeout, wantTLS)
	}
	wantHdr := time.Duration(defaults.DefaultResponseHeaderTimeoutSeconds) * time.Second
	if tr.ResponseHeaderTimeout != wantHdr {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, wantHdr)
	}
	wantExpect := time.Duration(defaults.DefaultExpectContinueTimeoutSeconds) * time.Second
	if tr.ExpectContinueTimeout != wantExpect {
		t.Errorf("ExpectContinueTimeout = %s, want %s", tr.ExpectContinueTimeout, wantExpect)
	}
}

// TestResponseHeaderTimeout_StrictlyLessThanRequestTimeout pins the contract
// the constant comment relies on: the granular header timeout must fire
// before the outer client-level request timeout, otherwise the outer timeout
// wins and the granular one has no effect.
func TestResponseHeaderTimeout_StrictlyLessThanRequestTimeout(t *testing.T) {
	hdr := time.Duration(defaults.DefaultResponseHeaderTimeoutSeconds) * time.Second
	req := defaults.DefaultSEMPRequestTimeoutDuration
	if hdr >= req {
		t.Errorf("DefaultResponseHeaderTimeoutSeconds (%s) must be strictly less than DefaultSEMPRequestTimeoutDuration (%s); the granular timeout will never fire", hdr, req)
	}
}
