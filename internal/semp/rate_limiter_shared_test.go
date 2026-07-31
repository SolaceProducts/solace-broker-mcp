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

package semp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/config"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// semp.request_min_interval is documented as "Minimum spacing between successive
// SEMP requests per event broker" and defaults to 100ms, so it is live in every
// shipped configuration including the Kubernetes manifest. A broker has two
// Senders — one per protocol — and the limiter must therefore be shared the way
// the in-flight Semaphore already is (see resilience.Semaphore's contract, and
// the SOL-150116 note in sender.go about a Sender-private cap silently allowing
// 2x the configured value).
//
// This test lives at the broker level because the defect is in the wiring, not
// in Sender: each Sender paces itself correctly in isolation, and only
// composing the two against one broker reveals the doubled rate.
//
// Failure model: with a limiter per protocol client, two tickers start together
// and both fire at t=interval. A time.Ticker channel buffers one tick, so the
// second protocol's request finds a tick already waiting and is admitted with
// no spacing at all — the observed gap is ~0 rather than >= interval.
func TestBrokerClient_RateLimiterIsSharedAcrossProtocols(t *testing.T) {
	const (
		interval = 200 * time.Millisecond
		requests = 6
	)

	var (
		mu       sync.Mutex
		arrivals []time.Time
	)
	record := func() {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		mu.Unlock()
	}

	// One server answering both protocols: SEMPv1 posts to /SEMP, SEMPv2 gets a
	// versioned path.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record()
		if r.URL.Path == "/SEMP" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<rpc-reply><rpc><show/></rpc><execute-result code="ok"/></rpc-reply>`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"meta":{}}`))
	}))
	defer server.Close()

	retries := 0
	minInterval := interval
	sempCfg := &config.SEMPConfig{
		RequestTimeoutDuration: 5 * time.Second,
		Retries:                &retries,
		RequestMinInterval:     &minInterval,
		RetryMinInterval:       1 * time.Millisecond,
		RetryMaxInterval:       5 * time.Millisecond,
		// Deliberately generous: the semaphore must be provably not the
		// constraint, so any spacing observed comes from the rate limiter.
		MaxConcurrentPerBroker: 10,
	}
	brokerCfg := &config.BrokerConfig{
		URL:  server.URL,
		Auth: config.AuthConfig{Mode: "basic", Username: "admin", Password: "secret"},
	}

	bc, err := semp.NewBrokerClient("rate-limit-test", brokerCfg, sempCfg, nil)
	if err != nil {
		t.Fatalf("NewBrokerClient: %v", err)
	}
	defer bc.Close()

	op := &sempv2.Operation{
		ID:     "getAbout",
		Method: http.MethodGet,
		Path:   "/SEMP/v2/monitor/about",
	}

	// Sequential alternation is enough to expose the defect and is far less
	// flaky than racing goroutines: an idle protocol's ticker always has a tick
	// buffered, so its next request is admitted immediately.
	start := time.Now()
	for i := range requests {
		if i%2 == 0 {
			if _, err := bc.SEMPv2().Execute(context.Background(), op, nil); err != nil {
				t.Fatalf("request %d (SEMPv2): %v", i, err)
			}
			continue
		}
		if _, err := bc.SEMPv1().Execute(context.Background(), `<rpc><show><version/></show></rpc>`); err != nil {
			t.Fatalf("request %d (SEMPv1): %v", i, err)
		}
	}
	elapsed := time.Since(start)

	mu.Lock()
	got := append([]time.Time(nil), arrivals...)
	mu.Unlock()

	if len(got) != requests {
		t.Fatalf("broker saw %d requests, want %d", len(got), requests)
	}

	// Per-pair spacing. A small tolerance absorbs scheduler jitter without
	// weakening the assertion: the defect produces gaps near zero, not near the
	// interval.
	const tolerance = 20 * time.Millisecond
	for i := 1; i < len(got); i++ {
		gap := got[i].Sub(got[i-1])
		if gap < interval-tolerance {
			t.Errorf("gap between request %d and %d was %v, want >= %v — the rate limiter is "+
				"being applied per protocol client instead of per broker, so the broker receives "+
				"up to 2x the configured rate", i-1, i, gap, interval)
		}
	}

	// Aggregate rate. This is the contract docs/configuration.md states, and it
	// is robust to per-pair timing noise. No upper bound is asserted.
	if want := time.Duration(requests-1) * interval; elapsed < want-tolerance {
		t.Errorf("%d alternating requests completed in %v, want at least %v", requests, elapsed, want)
	}
}
