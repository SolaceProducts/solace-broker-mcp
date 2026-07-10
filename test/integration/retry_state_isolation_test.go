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
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/config"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/auth"
	"github.com/SolaceDev/solace-broker-mcp/internal/semp/resilience"
)

// TestSenderRetryStateIsolatedUnderConcurrency composes resilience.Sender with
// a real auth.BasicAuthenticator + a shared resilience.SafeCookieJar and
// verifies that each Sender.Do() call owns its own retryState — the "retry
// once" 401 budget is per-request, not shared across concurrent requests.
//
// The invariant is meaningless in isolation on either component alone: Sender
// declares the retry-once cap, BasicAuthenticator declares the recovery
// strategy, and the observable failure mode ("only one goroutine's retry
// fires") only emerges when the two are composed under concurrent load. The
// test therefore belongs in this tier, per test/integration/README.md.
//
// Failure model: if retryState were accidentally shared across concurrent
// Do() calls (e.g. hoisted to a Sender field in some future refactor), the
// first goroutine to reach checkRetry flips auth401Retried and denies every
// other goroutine its retry. The correct per-request design leaves each
// caller free to exercise its one-shot budget independently.
//
// Fires N concurrent requests against a Sender whose fake broker 401s the
// first hit per caller and 200s the retry. Each caller carries a unique
// X-Test-Goroutine-ID header; the server tracks first-hit-per-caller under a
// mutex for atomic check-and-set. Assertions cover both per-goroutine status
// and the aggregate hit count so a failure output points directly at
// retry-state leak.
//
// The test server emits no Set-Cookie header. This keeps the shared
// SafeCookieJar empty throughout the run so the only cross-goroutine state
// left in play is retryState itself — the invariant this test observes. Jar
// concurrent set/get/clear behavior is separately covered by
// TestSafeCookieJar_ConcurrentSetClear in the resilience package.
//
// Runs under -race (integration suite always does); the race detector is a
// second, independent detection path for accidental sharing.
func TestSenderRetryStateIsolatedUnderConcurrency(t *testing.T) {
	const goroutines = 50
	const goroutineIDHeader = "X-Test-Goroutine-ID"

	// Server tracks first-hit-per-caller under a mutex. sync.Map is not used
	// because we need atomic check-and-set semantics: "have I seen this ID
	// before? if not, record it AND return 401" must be one indivisible step,
	// otherwise two concurrent retries for the same caller could both be
	// treated as first hits.
	var mu sync.Mutex
	seen := make(map[string]bool)
	var totalHits atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		totalHits.Add(1)
		id := r.Header.Get(goroutineIDHeader)

		mu.Lock()
		firstHit := !seen[id]
		seen[id] = true
		mu.Unlock()

		if firstHit {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`unauthorized`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer server.Close()

	// Compose the real components. The jar is shared between the
	// BasicAuthenticator (which clears it on 401) and http.Client.Jar (which
	// the stdlib reads/writes during Do) — the same wiring semp.NewBrokerClient
	// performs in production.
	jar, err := resilience.NewSafeCookieJar()
	if err != nil {
		t.Fatalf("NewSafeCookieJar: %v", err)
	}
	authn := auth.NewBasicAuthenticator("admin", "secret", "test-broker", jar)

	httpClient := server.Client()
	httpClient.Jar = jar

	retries := 3
	minInterval := time.Duration(0)
	sempCfg := &config.SEMPConfig{
		Retries:            &retries,
		RequestMinInterval: &minInterval,
		RetryMinInterval:   1 * time.Millisecond,
		RetryMaxInterval:   10 * time.Millisecond,
	}
	// Semaphore is sized to the concurrency so goroutines are not serialized
	// into batches (the default production cap is smaller). Serialization would
	// shrink the race window inside checkRetry and mask cross-request state
	// leaks — the exact bug this test is designed to observe.
	sender := resilience.New(httpClient, sempCfg, authn, server.URL, resilience.NewSemaphore(goroutines))

	// Starting gate: all goroutines block on start, then race into Do()
	// simultaneously — maximizing the chance they overlap inside checkRetry.
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	statuses := make([]int, goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		done.Add(1)
		go func(idx int) {
			defer done.Done()
			id := fmt.Sprintf("g-%d", idx)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+"/test", nil)
			if err != nil {
				errs[idx] = err
				return
			}
			req.Header.Set(goroutineIDHeader, id)

			start.Wait()

			resp, err := sender.Do(context.Background(), req)
			if err != nil {
				errs[idx] = err
				return
			}
			statuses[idx] = resp.StatusCode
			resp.Body.Close()
		}(i)
	}

	start.Done() // release the gate
	done.Wait()

	// Every goroutine must have recovered to 200 via its own retry budget.
	// A shared retryState would leave most goroutines stuck at 401.
	var failed int
	for i := range goroutines {
		if errs[i] != nil {
			t.Errorf("goroutine %d: unexpected error: %v", i, errs[i])
			failed++
			continue
		}
		if statuses[i] != http.StatusOK {
			t.Errorf("goroutine %d: status = %d, want 200 (retry budget likely denied by shared retryState)", i, statuses[i])
			failed++
		}
	}

	// Total server hits must be exactly 2×N: one 401 + one 200 per caller. A
	// shared retryState typically produces N+1 hits (all N first hits plus
	// exactly one retry). This aggregate check corroborates the per-goroutine
	// assertions above.
	if got, want := totalHits.Load(), int32(2*goroutines); got != want {
		t.Errorf("total server hits = %d, want %d (2 per goroutine); observed %d/%d goroutines failing suggests retry-state leak", got, want, failed, goroutines)
	}

	// Visible under `go test -v` as evidence the assertion had data to assert
	// on. Quiet by default.
	t.Logf("goroutines=%d total_hits=%d expected_hits=%d failed=%d", goroutines, totalHits.Load(), 2*goroutines, failed)
}
