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

package tools

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/resilience"
)

// realDoErr issues one GET with client and returns the error net/http produced.
// It is the same value retryablehttp hands Sender.errorHandler as doErr, which
// wraps it unchanged as RetriesExhaustedError.Err — so these tests classify
// what the stdlib really returns, not a hand-built imitation of it.
func realDoErr(t *testing.T, client *http.Client, url string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("GET %s succeeded; the test needs it to fail", url)
	}
	return err
}

// closedPortURL returns a URL on host whose port nothing listens on.
func closedPortURL(t *testing.T, network, host string) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), network, net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s %s: %v", network, host, err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return "http://" + addr + "/SEMP/v2/monitor/msgVpns"
}

// hangingServer accepts requests and never answers until the test ends.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

// droppingListener accepts each connection and closes it without a response.
func droppingListener(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return "http://" + ln.Addr().String() + "/"
}

// stallingListener accepts each connection and holds it open without writing,
// so a TLS client waits for a ServerHello that never comes.
func stallingListener(t *testing.T) string {
	t.Helper()
	return rawListener(t, func(net.Conn) {})
}

// garbageListener answers each connection with bytes that are neither TLS nor
// HTTP, the way a non-HTTP service (such as the SMF port) does.
func garbageListener(t *testing.T) string {
	t.Helper()
	return rawListener(t, func(c net.Conn) { _, _ = c.Write([]byte("SMF\x00\x01\x02\x03\x04")) })
}

// rawListener runs serve on each accepted connection and keeps the connection
// open until the test ends. It returns the listener's host:port.
func rawListener(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				serve(c)
				<-done
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// clientCertServer is a TLS server that demands a client certificate, capped
// at maxVersion. Its own Client() trusts the server but sends no certificate,
// so the server rejects the handshake with a fatal alert.
func clientCertServer(t *testing.T, maxVersion uint16) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, MaxVersion: maxVersion}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// SOL-155417: a request that never got an HTTP response used to be reported as
// "(HTTP 0)", which names no cause. Each case below drives a real failure
// through net/http and checks that the agent is told what went wrong, in words,
// without the status and without the host or address that the raw error carries.
func TestBuildErrorMessage_ConnectionFailureNamesCause(t *testing.T) {
	const tail = " Internal retries exhausted; try again later."

	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	hang := hangingServer(t)
	liveAddr := hang.Listener.Addr().String()
	tls12 := clientCertServer(t, tls.VersionTLS12)
	tls13 := clientCertServer(t, tls.VersionTLS13)

	tests := []struct {
		name string
		err  func(t *testing.T) error
		want string
	}{
		{
			name: "connection refused",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{}, closedPortURL(t, "tcp4", "127.0.0.1"))
			},
			want: `Could not connect to broker "dev" (connection refused) after 3 attempts.` + tail,
		},
		{
			name: "connection refused over IPv6",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{}, closedPortURL(t, "tcp6", "::1"))
			},
			want: `Could not connect to broker "dev" (connection refused) after 3 attempts.` + tail,
		},
		{
			// .invalid is reserved by RFC 2606 and never resolves. The transport
			// has no Proxy, so an HTTP_PROXY on the runner cannot answer for it.
			name: "host name not resolvable",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{Transport: &http.Transport{}}, "http://solace-broker-mcp-test.invalid/SEMP")
			},
			want: `Could not connect to broker "dev" (host name could not be resolved) after 3 attempts.` + tail,
		},
		{
			// A dialer whose deadline has already passed fails the dial with the
			// same timeout error a firewall that drops SYNs produces, without
			// depending on any unroutable address behaving the same everywhere.
			name: "connection timed out",
			err: func(t *testing.T) error {
				tr := &http.Transport{DialContext: (&net.Dialer{Timeout: time.Nanosecond}).DialContext}
				return realDoErr(t, &http.Client{Transport: tr}, "http://"+liveAddr+"/")
			},
			want: `Could not connect to broker "dev" (connection timed out) after 3 attempts.` + tail,
		},
		{
			name: "untrusted TLS certificate",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{}, tlsSrv.URL)
			},
			want: `Could not connect to broker "dev" (TLS handshake failed) after 3 attempts.` + tail,
		},
		{
			// Over TCP a server alert is a *net.OpError{Op: "remote error"}, not
			// tls.AlertError (QUIC only). TLS 1.2 rejects during the handshake.
			name: "server rejected the handshake (TLS 1.2)",
			err: func(t *testing.T) error {
				return realDoErr(t, tls12.Client(), tls12.URL)
			},
			want: `Could not connect to broker "dev" (TLS handshake failed) after 3 attempts.` + tail,
		},
		{
			// TLS 1.3 sends the rejection after the client's handshake completes,
			// so the alert surfaces on the first read instead.
			name: "server rejected the handshake (TLS 1.3)",
			err: func(t *testing.T) error {
				return realDoErr(t, tls13.Client(), tls13.URL)
			},
			want: `Could not connect to broker "dev" (TLS handshake failed) after 3 attempts.` + tail,
		},
		{
			// Client.Do replaces the RecordHeaderError with http.ErrSchemeMismatch.
			name: "https URL on a plain-HTTP port",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{}, "https://"+liveAddr+"/")
			},
			want: `Could not connect to broker "dev" (the port serves plain HTTP, not HTTPS: use http://) after 3 attempts.` + tail,
		},
		{
			// Bytes that do not start with "HTTP/" stay a tls.RecordHeaderError.
			name: "https URL on a port that speaks neither TLS nor HTTP",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{}, "https://"+garbageListener(t)+"/")
			},
			want: `Could not connect to broker "dev" (TLS handshake failed) after 3 attempts.` + tail,
		},
		{
			name: "TLS handshake timed out",
			err: func(t *testing.T) error {
				tr := &http.Transport{TLSHandshakeTimeout: 50 * time.Millisecond}
				return realDoErr(t, &http.Client{Transport: tr}, "https://"+stallingListener(t)+"/")
			},
			want: `Could not connect to broker "dev" (TLS handshake timed out) after 3 attempts.` + tail,
		},
		{
			// net/http wraps a failed proxy dial as *net.OpError{Op: "proxyconnect"}.
			name: "proxy connection timed out",
			err: func(t *testing.T) error {
				tr := &http.Transport{
					Proxy:       http.ProxyURL(&url.URL{Scheme: "http", Host: liveAddr}),
					DialContext: (&net.Dialer{Timeout: time.Nanosecond}).DialContext,
				}
				return realDoErr(t, &http.Client{Transport: tr}, "http://broker.example/")
			},
			want: `Could not connect to broker "dev" (proxy connection timed out) after 3 attempts.` + tail,
		},
		{
			name: "proxy refused the connection",
			err: func(t *testing.T) error {
				proxy, err := url.Parse(closedPortURL(t, "tcp4", "127.0.0.1"))
				if err != nil {
					t.Fatal(err)
				}
				tr := &http.Transport{Proxy: http.ProxyURL(proxy)}
				return realDoErr(t, &http.Client{Transport: tr}, "http://broker.example/")
			},
			want: `Could not connect to broker "dev" (proxy connection refused) after 3 attempts.` + tail,
		},
		{
			// The production transport sets ResponseHeaderTimeout below the
			// client timeout, so this is the shape a slow broker actually gives.
			name: "broker accepted the connection but sent no response headers",
			err: func(t *testing.T) error {
				tr := &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}
				return realDoErr(t, &http.Client{Transport: tr}, hang.URL)
			},
			want: `Broker "dev" did not respond within the request timeout after 3 attempts.` + tail,
		},
		{
			name: "client timeout after connecting",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{Timeout: 50 * time.Millisecond}, hang.URL)
			},
			want: `Broker "dev" did not respond within the request timeout after 3 attempts.` + tail,
		},
		{
			// Sender's retry-chain deadline: checkRetry returns ctx.Err() bare.
			name: "retry-chain deadline",
			err:  func(*testing.T) error { return context.DeadlineExceeded },
			want: `Broker "dev" did not respond within the request timeout after 3 attempts.` + tail,
		},
		{
			name: "connection dropped without a response",
			err: func(t *testing.T) error {
				return realDoErr(t, &http.Client{}, droppingListener(t))
			},
			want: `The connection to broker "dev" failed after 3 attempts.` + tail,
		},
		{
			name: "no underlying error recorded",
			err:  func(*testing.T) error { return nil },
			want: `The connection to broker "dev" failed after 3 attempts.` + tail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cause := tt.err(t)
			got, suggestions := buildErrorMessage(&resilience.RetriesExhaustedError{Attempts: 3, Err: cause}, "dev")
			if got != tt.want {
				t.Errorf("message mismatch\n got: %q\nwant: %q\n(cause: %v)", got, tt.want, cause)
			}
			if suggestions != nil {
				t.Errorf("suggestions = %v, want nil", suggestions)
			}
			assertNoConnectionLeak(t, got, cause)
		})
	}
}

// assertNoConnectionLeak checks that msg carries no status-0 text and none of
// the host, address or port the raw cause names.
func assertNoConnectionLeak(t *testing.T, msg string, cause error) {
	t.Helper()
	if strings.Contains(msg, "HTTP 0") {
		t.Errorf("message %q still says HTTP 0", msg)
	}
	for _, leak := range []string{"127.0.0.1", "::1", ".invalid", "dial tcp", "x509", "EOF", "Get \"",
		"remote error", "proxyconnect", "handshake timeout", "broker.example"} {
		if strings.Contains(msg, leak) {
			t.Errorf("message %q leaks %q from the raw cause %v", msg, leak, cause)
		}
	}
}

func TestBuildErrorMessage_ConnectionFailureWording(t *testing.T) {
	refused := realDoErr(t, &http.Client{}, closedPortURL(t, "tcp4", "127.0.0.1"))

	tests := []struct {
		name  string
		err   error
		alias string
		want  string
	}{
		{
			"one attempt is singular",
			&resilience.RetriesExhaustedError{Attempts: 1, Err: refused},
			"dev",
			`Could not connect to broker "dev" (connection refused) after 1 attempt. Internal retries exhausted; try again later.`,
		},
		{
			"no alias falls back to the broker",
			&resilience.RetriesExhaustedError{Attempts: 3, Err: refused},
			"",
			`Could not connect to the broker (connection refused) after 3 attempts. Internal retries exhausted; try again later.`,
		},
		{
			"wrapped by the tool layer (errors.As traversal)",
			fmt.Errorf("executing tool %q: %w", "list-queues", &resilience.RetriesExhaustedError{Attempts: 3, Err: refused}),
			"dev",
			`Could not connect to broker "dev" (connection refused) after 3 attempts. Internal retries exhausted; try again later.`,
		},
		{
			// Windows reports a refused dial as WSAECONNREFUSED (10061), which
			// syscall.ECONNREFUSED does not match there. Built by hand because
			// this test cannot produce it on a non-Windows runner.
			"windows refused dial",
			&resilience.RetriesExhaustedError{Attempts: 3, Err: &net.OpError{
				Op: "dial", Net: "tcp",
				Err: os.NewSyscallError("connectex", syscall.Errno(10061)),
			}},
			"dev",
			`Could not connect to broker "dev" (connection refused) after 3 attempts. Internal retries exhausted; try again later.`,
		},
		{
			// A read deadline on an open connection is a timeout too, but not a
			// connect failure: only a dial timeout means the broker was unreachable.
			"read timeout on an open connection",
			&resilience.RetriesExhaustedError{Attempts: 3, Err: &net.OpError{
				Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded,
			}},
			"dev",
			`Broker "dev" did not respond within the request timeout after 3 attempts. Internal retries exhausted; try again later.`,
		},
		{
			// A response was received, so the status is real and stays.
			"an HTTP status still reports the status",
			&resilience.RetriesExhaustedError{StatusCode: 503, Attempts: 3},
			"dev",
			"Request failed after 3 attempts (HTTP 503). Internal retries exhausted; try again later.",
		},
		{
			// Ticket criterion 2: a write that may have reached the broker keeps the
			// SOL-155411 wording. It must not become "try again later" just because
			// the cause is now classifiable.
			"non-idempotent connection failure keeps the do-not-retry wording",
			&resilience.RetriesExhaustedError{Attempts: 1, NonIdempotent: true, Err: refused},
			"dev",
			"Request failed and was deliberately not retried, because repeating this " +
				"operation is not safe: the broker may have already applied it. Check the " +
				"current state before deciding whether to issue it again.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := buildErrorMessage(tt.err, tt.alias)
			if got != tt.want {
				t.Errorf("message mismatch\n got: %q\nwant: %q", got, tt.want)
			}
			assertNoConnectionLeak(t, got, tt.err)
		})
	}
}

// The agent-facing message changes; the retry contract must not. A connection
// failure is still retryable and still carries no status.
func TestRetriesExhaustedConnectionFailureStaysRetryable(t *testing.T) {
	refused := realDoErr(t, &http.Client{}, closedPortURL(t, "tcp4", "127.0.0.1"))
	err := &resilience.RetriesExhaustedError{Attempts: 3, Err: refused}
	if !isRetryable(err) {
		t.Error("connection failure after retries must stay retryable")
	}
	if errors.Unwrap(err) != refused {
		t.Error("cause must stay reachable for logs")
	}
}
