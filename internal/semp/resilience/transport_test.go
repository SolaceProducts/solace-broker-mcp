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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/defaults"
)

// TestNewTunedTransport_GranularTimeoutsAreSet locks in the production
// timeout posture for outbound SEMP transports: the TCP dial,
// TLSHandshakeTimeout, ResponseHeaderTimeout, and ExpectContinueTimeout are
// all bounded, so a broker that black-holes the connection attempt, stalls in
// handshake, or accepts the connection then never sends headers fails fast
// rather than holding a MaxConcurrentPerBroker semaphore slot for the full
// client-level request timeout.
//
// The dial bound and ResponseHeaderTimeout are both derived from
// sempCfg.RequestTimeoutDuration so the "strictly less than the outer client
// timeout" relationship is preserved even when an operator customizes
// request_timeout_duration in broker config.
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

	// Without a DialContext the transport falls back to net/http's zeroDialer,
	// which has no Timeout — so a broker whose network path silently drops SYNs
	// stalls in connect() until the outer client timeout, holding its semaphore
	// slot the whole time. That is the one connection phase the other granular
	// timeouts below do not cover.
	if tr.DialContext == nil {
		t.Error("DialContext = nil; the transport falls back to net/http's unbounded zeroDialer, " +
			"so a black-holed connection attempt holds a per-broker slot for the full request timeout")
	}
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

// TestDialTimeout_DerivedFromRequestTimeout pins the derivation rather than a
// hardcoded constant, for the same reason ResponseHeaderTimeout is derived: a
// fixed 10s dial bound would never fire for an operator who sets
// request_timeout_duration below it, silently restoring the unbounded-dial
// exposure this fix removes.
//
// The zero case is not reachable in production — config.Load substitutes
// DefaultSEMPRequestTimeoutDuration when the field is unset and validation
// rejects a non-positive value — but a directly constructed SEMPConfig hits it
// (see the TLS tests below), and there the dial bound is the only bound on the
// TCP connect, because http.Client.Timeout and ResponseHeaderTimeout are zero
// too. TLSHandshakeTimeout and ExpectContinueTimeout are constants and still
// apply, but they bound later stages. Falling back to the ceiling is strictly
// safer than falling back to "unbounded".
func TestDialTimeout_DerivedFromRequestTimeout(t *testing.T) {
	tests := []struct {
		name           string
		requestTimeout time.Duration
		want           time.Duration
	}{
		{"shipped default is capped by the ceiling", time.Minute, dialTimeoutCeiling},
		{"well above the ceiling stays at the ceiling", 10 * time.Minute, dialTimeoutCeiling},
		{"at twice the ceiling the halves meet", 20 * time.Second, dialTimeoutCeiling},
		{"aggressive tuning derives below the ceiling", 10 * time.Second, 5 * time.Second},
		{"very aggressive tuning derives further down", 5 * time.Second, 2500 * time.Millisecond},
		{"unset falls back to the ceiling, not to unbounded", 0, dialTimeoutCeiling},
		{"negative falls back to the ceiling", -1 * time.Second, dialTimeoutCeiling},
		// Integer division truncates, so anything under 2ns halves to zero.
		// Positive, so it clears validation, and zero on net.Dialer means
		// unbounded — the precise defect this function exists to remove.
		{"1ns does not truncate to unbounded", 1 * time.Nanosecond, 1 * time.Nanosecond},
		{"2ns is the first value that halves cleanly", 2 * time.Nanosecond, 1 * time.Nanosecond},
		{"3ns truncates down but stays positive", 3 * time.Nanosecond, 1 * time.Nanosecond},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dialTimeout(tt.requestTimeout)
			if got != tt.want {
				t.Errorf("dialTimeout(%s) = %s, want %s", tt.requestTimeout, got, tt.want)
			}
			if got <= 0 {
				t.Errorf("dialTimeout(%s) = %s; a non-positive dial timeout means unbounded", tt.requestTimeout, got)
			}
			// The invariant that makes the bound useful at all — but only where
			// it is satisfiable. At 1ns there is no smaller positive duration,
			// so "strictly positive" and "strictly less than the outer timeout"
			// cannot both hold. Boundedness wins: an equal bound fires at the
			// same instant as the outer timeout, which is indistinguishable in
			// practice, whereas a zero bound is unbounded and is the whole
			// defect. Everything from 2ns up satisfies both.
			if tt.requestTimeout >= 2 && got >= tt.requestTimeout {
				t.Errorf("dialTimeout(%s) = %s must be strictly less than the outer request timeout, "+
					"or the outer timeout wins and the dial bound never fires", tt.requestTimeout, got)
			}
			if tt.requestTimeout == 1 && got != tt.requestTimeout {
				t.Errorf("dialTimeout(1ns) = %s, want 1ns: the degenerate case trades the "+
					"strictly-less property for staying bounded", got)
			}
		})
	}
}

// TestNewSEMPDialer_CarriesTheDerivedBound asserts the derived value actually
// reaches a dialer, which the table above cannot: net.Dialer.Timeout is not
// readable back off a DialContext closure, so the transport-level tests can only
// check that DialContext is non-nil.
//
// Honest limit: this pins the mapping from requestTimeout to dialer, not the
// argument NewTunedTransport passes. Substituting a constant at that one call
// site still passes this suite — verified by mutation — so that expression is
// guarded by review, not by a test.
func TestNewSEMPDialer_CarriesTheDerivedBound(t *testing.T) {
	for _, requestTimeout := range []time.Duration{time.Minute, 10 * time.Second, 5 * time.Second, 0} {
		d := newSEMPDialer(requestTimeout)
		if want := dialTimeout(requestTimeout); d.Timeout != want {
			t.Errorf("newSEMPDialer(%s).Timeout = %s, want the derived %s — the dialer must carry "+
				"the value derived from the request timeout, not a constant", requestTimeout, d.Timeout, want)
		}
		if d.Timeout <= 0 {
			t.Errorf("newSEMPDialer(%s).Timeout = %s; non-positive means an unbounded dial", requestTimeout, d.Timeout)
		}
		if d.KeepAlive != dialKeepAlive {
			t.Errorf("newSEMPDialer(%s).KeepAlive = %s, want %s", requestTimeout, d.KeepAlive, dialKeepAlive)
		}
	}
}

// TestNewTunedTransport_DialContextHonorsCallerContext is the behavioural half:
// it proves the wired dialer actually consults the caller's context, which is
// what lets Sender.Do release its per-broker semaphore slot when the retry
// budget or the caller's deadline fires mid-connect.
//
// An already-cancelled context is used deliberately. A real black-holed SYN is
// not reproducible in a unit test, and probing a reserved address is
// environment-dependent — an unroutable range may return ENETUNREACH promptly
// on one machine and hang on another, which would make this test's outcome a
// property of the network rather than of the code. Cancellation is exact.
func TestNewTunedTransport_DialContextHonorsCallerContext(t *testing.T) {
	tr := NewTunedTransport(&config.BrokerConfig{}, &config.SEMPConfig{
		MaxConcurrentPerBroker: 10,
		RequestTimeoutDuration: defaults.DefaultSEMPRequestTimeoutDuration,
	})
	if tr.DialContext == nil {
		t.Fatal("DialContext = nil, want a bounded dialer")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// 192.0.2.1 is TEST-NET-1 (RFC 5737) and is never routed. With the context
	// already cancelled the dialer must refuse before touching the network, so
	// the address is never actually contacted and no timing assumption applies.
	conn, err := tr.DialContext(ctx, "tcp", "192.0.2.1:1943")
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("DialContext with a cancelled context returned %v, want context.Canceled — "+
			"a dialer that ignores the caller's context would hold a per-broker slot past the deadline", err)
	}
}

// TestNewTunedTransport_TLSVerificationOnByDefault locks in the production
// default: an unset insecure_skip_verify yields a transport with cert
// verification on.
func TestNewTunedTransport_TLSVerificationOnByDefault(t *testing.T) {
	tr := NewTunedTransport(&config.BrokerConfig{}, &config.SEMPConfig{})

	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want non-nil")
	}
	if tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true for zero-value broker config, want false (verification on)")
	}
}

// TestNewTunedTransport_TLSVerificationSkippedWhenConfigured asserts
// insecure_skip_verify: true propagates into the transport (dev opt-out
// for self-signed certs).
func TestNewTunedTransport_TLSVerificationSkippedWhenConfigured(t *testing.T) {
	tr := NewTunedTransport(&config.BrokerConfig{InsecureSkipVerify: true}, &config.SEMPConfig{})

	if tr.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig = nil, want non-nil")
	}
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = false, want true (config opt-out not honoured)")
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
	// Go's http.Transport only auto-enables HTTP/2 when TLSClientConfig is
	// nil; the HTTP/1.1 guarantee above therefore depends on the transport
	// continuing to supply a custom TLSClientConfig, not just on
	// ForceAttemptHTTP2 staying false.
	if tr.TLSClientConfig == nil {
		t.Error("TLSClientConfig = nil re-enables Go's automatic HTTP/2, breaking the MaxConnsPerHost concurrency bound")
	}
}
