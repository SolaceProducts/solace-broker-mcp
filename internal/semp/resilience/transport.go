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
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
)

// ProxyDirect is the EffectiveProxy label for a destination that bypasses the
// proxy — either because no proxy variable is set, or because NO_PROXY exempts
// it. A fixed label rather than an empty string so the log field is always
// present and `proxy != direct` is a usable query.
const ProxyDirect = "direct"

// proxyResolver is the one symbol both the transport's Proxy field and
// EffectiveProxy read, so the label a broker logs cannot drift from the
// resolution that transport actually performs. They resolved identically when
// written — both named http.ProxyFromEnvironment — but nothing tied them
// together, so a future change to one was free to leave the other behind.
// Changing the source of truth now means changing this line.
var proxyResolver = http.ProxyFromEnvironment

// idleConnTimeout is how long an idle keep-alive connection sits in the pool
// before the client closes it. Matches the value in http.DefaultTransport.
// Without this, an idle connection lives until the broker closes it — which
// can produce a "connection reset by peer" on the first request after a quiet
// period and trigger an unnecessary retry.
const idleConnTimeout = 90 * time.Second

// tlsHandshakeTimeout bounds how long the transport waits for a TLS
// handshake. Without it, a broker stuck in handshake holds one of the
// MaxConnsPerHost connection slots for the full request timeout
// window. 10s tolerates network outliers while bounding the failure
// window to a small fraction of the request timeout.
const tlsHandshakeTimeout = 10 * time.Second

// expectContinueTimeout caps how long the transport waits for a "100
// Continue" before sending the body. SEMP requests do not use
// Expect/100-continue, but setting this is standard HTTP-client
// hygiene against a misconfigured peer that signals 100-continue.
const expectContinueTimeout = 1 * time.Second

// dialTimeoutCeiling is the upper bound on how long the transport waits for a
// TCP connection to a broker. Without a bound the transport falls back to
// net/http's zeroDialer, which has no Timeout, so a broker whose network path
// silently drops SYNs — a security group that omits this server's CIDR, a
// default-deny NetworkPolicy, a stale DNS answer after a failover — stalls in
// connect() until the outer client timeout, holding one of the
// MaxConnsPerHost connection slots and its per-broker semaphore slot for that
// whole window. Because a connection error is retryable, the chain repeats.
// 10s matches tlsHandshakeTimeout: the same tolerance for network outliers,
// applied one layer down.
const dialTimeoutCeiling = 10 * time.Second

// dialKeepAlive matches http.DefaultTransport's dialer. Note what this does and
// does not do: a zero-value net.Dialer already sends keep-alive probes, at Go's
// 15s default (see net.Dialer.KeepAlive — only a negative value disables them),
// so this does not turn anything on. It relaxes that idle interval to 30s. Both
// values sit far inside idleConnTimeout, and on an active connection
// ResponseHeaderTimeout bounds a stall long before probes conclude, so the
// change is immaterial to pool hygiene; it is here for consistency with
// DefaultTransport rather than for effect.
const dialKeepAlive = 30 * time.Second

// dialTimeout derives the TCP connect bound from the outer per-request timeout,
// for the same reason ResponseHeaderTimeout is derived (see NewTunedTransport):
// a granular timeout must stay strictly below the outer client-level timeout or
// the outer one wins and the granular bound never fires. A hardcoded 10s would
// be inert for an operator who sets request_timeout_duration to 5s.
//
// A non-positive requestTimeout falls back to the ceiling rather than to zero.
// Production cannot reach that state — config.Load substitutes
// defaults.DefaultSEMPRequestTimeoutDuration when the field is unset and
// validation rejects a non-positive value — but a directly constructed
// SEMPConfig can, and there http.Client.Timeout and ResponseHeaderTimeout are
// zero too, leaving this the only bound on the TCP connect. (TLSHandshakeTimeout
// and ExpectContinueTimeout are constants and still apply, but they bound later
// stages, not connect.) Zero would mean unbounded, which is the defect.
//
// The result is never zero. Integer division truncates, so any requestTimeout
// below 2ns would otherwise halve to zero — positive, so it clears validation,
// yet producing exactly the unbounded dial this function exists to prevent.
// A time.Nanosecond bound fails the dial immediately, which is the faithful
// reading of a 1ns request budget and is in any case strictly better than
// waiting forever. Spelled with the unit rather than a bare 1, since a raw
// literal in a duration context reads as easily as one second.
func dialTimeout(requestTimeout time.Duration) time.Duration {
	if requestTimeout <= 0 {
		return dialTimeoutCeiling
	}
	return max(time.Nanosecond, min(dialTimeoutCeiling, requestTimeout/2))
}

// newSEMPDialer builds the transport's dialer. It exists so the dial bound is
// observable at all: net.Dialer.Timeout cannot be read back off a DialContext
// closure, so a test can otherwise only assert that DialContext is non-nil,
// never what timeout it carries.
//
// This narrows the untested surface rather than eliminating it. A test can now
// pin that a given requestTimeout yields a dialer with the derived Timeout, but
// nothing can observe which argument NewTunedTransport actually passes — that
// one expression stays guarded only by review.
func newSEMPDialer(requestTimeout time.Duration) *net.Dialer {
	return &net.Dialer{
		Timeout:   dialTimeout(requestTimeout),
		KeepAlive: dialKeepAlive,
	}
}

// NewTunedTransport builds an *http.Transport sized for the per-broker
// concurrency cap. Both SEMPv1 and SEMPv2 clients use it so the connection
// pool behaviour stays consistent across protocol versions.
//
// The enforcing per-broker in-flight bound is the shared resilience.Semaphore
// acquired in Sender.Do (one per broker, see semp.NewBrokerClient).
// MaxConnsPerHost = MaxConcurrentPerBroker sizes the connection pool
// consistently with that cap: supplying a custom TLSClientConfig disables
// Go's automatic HTTP/2, so SEMP traffic is HTTP/1.1 and one connection
// carries one in-flight request. With the semaphore admitting at most
// MaxConcurrentPerBroker requests per broker across both protocol clients,
// each client's pool can never be asked for more connections than this, so
// the transport limit is sizing, not a second gate.
//
// Go's http.Transport defaults MaxIdleConnsPerHost to 2. With
// MaxConcurrentPerBroker at 10+, every request beyond the 2nd opens a new
// TCP+TLS handshake; under load this manifests as connection thrashing and
// elevated tail latency. Setting MaxIdleConnsPerHost = MaxConcurrentPerBroker
// lets the pool hold enough idle connections to absorb the configured
// concurrency without re-handshaking.
//
// MaxIdleConns (global across all hosts on this transport) gets headroom of
// 2× MaxConcurrentPerBroker so it never becomes the bottleneck on top of the
// per-host cap. Each broker has its own transport, so this only ever applies
// to connections to a single broker — the headroom is a defensive cushion,
// not a true multi-host budget.
//
// ResponseHeaderTimeout and the DialContext bound are derived from
// sempCfg.RequestTimeoutDuration rather than hardcoded. A granular timeout must
// stay strictly less than the outer client-level request timeout, otherwise the
// outer timeout wins and the regression this transport tuning fixes (a stuck
// broker holding a MaxConnsPerHost connection slot for the full request window)
// silently returns when operators set an aggressive request_timeout_duration in
// broker-config.yaml. See dialTimeout for the dial derivation.
//
// DialContext is set by hand rather than by cloning http.DefaultTransport.
// Clone would copy ForceAttemptHTTP2: true, which overrides the conservative
// auto-disable that the custom TLSClientConfig above relies on — HTTP/2 would
// then multiplex several in-flight requests onto one connection and invalidate
// the MaxConnsPerHost sizing described above. Supplying our own DialContext is
// HTTP/2-neutral: net/http lists a custom DialContext in the same
// conservative-disable set as a custom TLSClientConfig.
//
// Proxy is therefore named explicitly (SOL-153295). A nil Proxy means "never
// proxy" rather than "consult the environment", so before this the server
// ignored HTTPS_PROXY for broker traffic with no error and no log line, while
// IdP traffic honoured it — internal/idpclient does clone DefaultTransport.
// ProxyFromEnvironment does not disturb the HTTP/2 posture above: net/http's
// auto-disable keys off TLSClientConfig and DialContext, not off Proxy.
//
// Two limits, both under "Outbound HTTP Proxy" in docs/configuration.md: the
// environment is read once per process and cached, so this is restart-scoped
// and cannot vary per broker; and NO_PROXY matches the host as written in the
// URL, so a CIDR entry exempts only a broker addressed by IP literal, never
// one addressed by hostname.
func NewTunedTransport(brokerCfg *config.BrokerConfig, sempCfg *config.SEMPConfig) *http.Transport {
	return &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: brokerCfg.InsecureSkipVerify}, //nolint:gosec // G402 — user-configurable TLS skip for dev environments; defaults to false
		DialContext:           newSEMPDialer(sempCfg.RequestTimeoutDuration).DialContext,
		Proxy:                 proxyResolver,
		MaxConnsPerHost:       sempCfg.MaxConcurrentPerBroker,
		MaxIdleConnsPerHost:   sempCfg.MaxConcurrentPerBroker,
		MaxIdleConns:          sempCfg.MaxConcurrentPerBroker * 2,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: sempCfg.RequestTimeoutDuration / 2,
		ExpectContinueTimeout: expectContinueTimeout,
	}
}

// EffectiveProxy reports which proxy the transport above will use for rawURL,
// as a label for logging: a sanitized proxy URL, or ProxyDirect. It answers the
// question the reroute in NewTunedTransport otherwise leaves unanswerable —
// whether an inherited HTTPS_PROXY has put a proxy in front of this broker —
// which is visible on the wire only as a terse `proxyconnect tcp:` dial error.
//
// The result is safe to log. A proxy URL may carry credentials
// (http://user:password@proxy:3128), so it goes through
// config.SanitizeURLString, which drops userinfo. One consequence: that helper
// only recognizes http and https, so a socks5 proxy logs as an unparseable-URL
// placeholder. That still answers "a proxy is in play" — the diagnostic that
// matters here — and failing closed is the right direction for a value that can
// hold a password.
//
// Resolution matches the transport by construction: both read proxyResolver,
// so this cannot drift from what the transport does, including NO_PROXY and the
// loopback exemption. An unparseable rawURL reports
// ProxyDirect rather than an error — config validation rejects such a URL long
// before this runs, and a log label is not the place to surface it.
func EffectiveProxy(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ProxyDirect
	}
	proxyURL, err := proxyResolver(&http.Request{URL: u})
	if err != nil {
		return ProxyDirect
	}
	return proxyLabel(proxyURL)
}

// proxyLabel renders a resolved proxy URL as a log-safe label. Split from
// EffectiveProxy so the credential-stripping step is testable directly:
// ProxyFromEnvironment caches the environment behind a sync.Once, so a test
// that tried to reach this path by setting HTTPS_PROXY would depend on nothing
// else in the binary having resolved a proxy first.
func proxyLabel(proxyURL *url.URL) string {
	if proxyURL == nil {
		return ProxyDirect
	}
	return config.SanitizeURLString(proxyURL.String())
}
