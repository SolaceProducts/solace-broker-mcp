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

// Package integration_test holds in-process integration tests that compose
// multiple internal/ components and exercise them through their public APIs.
// See README.md in this directory for what belongs here and what does not.
package integration_test

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
)

// TestCredentialsAreIsolatedPerBroker is the in-process integration test that
// guards against the worst-case multi-broker bug: a credential configured for
// one broker reaching another broker's wire.
//
// It composes the real BrokerPool, the real BrokerClient, the real
// auth.NewAuthenticator dispatcher, and the real SEMPv1 protocol client. It
// stands in three fake brokers via httptest.NewServer and observes the
// Authorization header each one receives. Two invariants are asserted:
//
//   1. Each broker received only its own configured Authorization header — no
//      foreign credentials ever appeared on any broker's traffic.
//   2. Each broker received exactly the number of requests scheduled for it —
//      no requests were silently re-routed to a peer broker.
//
// Distributing requests asymmetrically (10/20/30) makes invariant 2 a real
// check: a partial-leakage bug that re-routes some broker-a traffic to
// broker-b would show up as broker-a being short and broker-b over-counted,
// even if the foreign-header check happened to miss the case.
//
// Scope: static-credential modes only (basic, bearer). OAuth introduces a
// qualitatively different isolation question (shared Exchanger and cache
// across brokers) that lives in its own integration test when T6 lands.
func TestCredentialsAreIsolatedPerBroker(t *testing.T) {
	type fakeBroker struct {
		alias        string
		wantAuth     string
		wantRequests int
		mu           sync.Mutex
		seen         map[string]int
	}

	brokers := []*fakeBroker{
		{alias: "broker-a", wantAuth: "Basic " + base64Encode("alice:passA"), wantRequests: 10, seen: map[string]int{}},
		{alias: "broker-b", wantAuth: "Basic " + base64Encode("bob:passB"), wantRequests: 20, seen: map[string]int{}},
		{alias: "broker-c", wantAuth: "Bearer token-C", wantRequests: 30, seen: map[string]int{}},
	}

	servers := make([]*httptest.Server, len(brokers))
	for i, fb := range brokers {
		fb := fb // capture
		servers[i] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth := r.Header.Get("Authorization")
			fb.mu.Lock()
			fb.seen[gotAuth]++
			fb.mu.Unlock()
			// SEMPv1 success envelope so the protocol client doesn't error out.
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<rpc-reply><rpc><show><version/></show></rpc><execute-result code="ok"/></rpc-reply>`))
		}))
		defer servers[i].Close()
	}

	// Multi-broker config: matches each fake broker with its credentials.
	cfgYAML := "mcp_client_auth:\n  mode: disabled\nbrokers:\n"
	cfgYAML += fmt.Sprintf("  broker-a:\n    url: %s\n    auth:\n      mode: basic\n      username: alice\n      password: passA\n", servers[0].URL)
	cfgYAML += fmt.Sprintf("  broker-b:\n    url: %s\n    auth:\n      mode: basic\n      username: bob\n      password: passB\n", servers[1].URL)
	cfgYAML += fmt.Sprintf("  broker-c:\n    url: %s\n    auth:\n      mode: bearer\n      token: token-C\n", servers[2].URL)

	cfgPath := filepath.Join(t.TempDir(), "broker-config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	// Schedule wantRequests calls per broker, then interleave so brokers are
	// not run as three serial blocks.
	var schedule []string
	for _, fb := range brokers {
		for range fb.wantRequests {
			schedule = append(schedule, fb.alias)
		}
	}
	interleaved := make([]string, 0, len(schedule))
	for offset := range len(brokers) {
		for i := offset; i < len(schedule); i += len(brokers) {
			interleaved = append(interleaved, schedule[i])
		}
	}

	var wg sync.WaitGroup
	wg.Add(len(interleaved))
	for _, alias := range interleaved {
		alias := alias
		go func() {
			defer wg.Done()
			client, err := pool.GetSEMPv1(alias)
			if err != nil {
				t.Errorf("GetSEMPv1(%q): %v", alias, err)
				return
			}
			// <show> command engages the Sender's retry-safe path on SEMPv1
			// (matches production retry posture). Body content is irrelevant —
			// the fake broker accepts anything and the test asserts on the
			// Authorization header only.
			if _, err := client.Execute(t.Context(), `<rpc><show><version/></show></rpc>`); err != nil {
				t.Errorf("Execute on %q: %v", alias, err)
			}
		}()
	}
	wg.Wait()

	// wg.Wait() above synchronizes with every handler goroutine, so fb.seen
	// has no concurrent writers at this point. The assertion loop is the only
	// goroutine touching it. No lock needed here — fb.mu is still load-bearing
	// during the concurrent phase (inside the handler), but not here.
	for _, fb := range brokers {
		// Visible under `go test -v` as evidence the assertion had data to
		// assert on. Quiet by default so unrelated test runs aren't noisy.
		t.Logf("%s saw: %v (expected %d × %q)", fb.alias, fb.seen, fb.wantRequests, fb.wantAuth)

		// Invariant 1: no foreign Authorization headers ever appeared.
		for gotAuth := range fb.seen {
			if gotAuth != fb.wantAuth {
				t.Errorf("%s: received foreign Authorization header %q (own header is %q); full seen=%v",
					fb.alias, gotAuth, fb.wantAuth, fb.seen)
			}
		}

		// Invariant 2: this broker received exactly wantRequests calls with
		// its own header — no over-count (foreign traffic re-routed here) and
		// no under-count (own traffic re-routed elsewhere).
		if got := fb.seen[fb.wantAuth]; got != fb.wantRequests {
			t.Errorf("%s: expected exactly %d requests with %q, got %d; full seen=%v",
				fb.alias, fb.wantRequests, fb.wantAuth, got, fb.seen)
		}
	}
}

// base64Encode produces the standard-encoded representation of "user:pass"
// used in Basic Auth headers, so the test's expected-header literals stay
// readable.
func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
