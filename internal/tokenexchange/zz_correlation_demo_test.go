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

package tokenexchange

// DEMO ONLY — not part of the test suite proper. Prints the real JSON log
// output of every flow around the SOL-153364 correlation-ID fix.
// Run with:
//
//	go test ./internal/tokenexchange/ -run TestDemoCorrelationFlows -v
//
// Delete this file when done; it is deliberately not committed.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/correlation"
)

func demoDump(t *testing.T, title string, logs *jsonLogBuffer) {
	t.Helper()
	logs.mu.Lock()
	raw := logs.buf.String()
	logs.buf.Reset()
	logs.mu.Unlock()
	fmt.Printf("\n════ %s ════\n", title)
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line != "" {
			fmt.Println(line)
		}
	}
}

func TestDemoCorrelationFlows(t *testing.T) {
	// NOT parallel: swaps the global logger.
	logs := captureJSONLogs(t)

	// ── Flow 1 + 2: cold path, then cache hit ─────────────────────────────
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("demo-token", 3600))
	}))
	defer srv.Close()

	e := newTestExchanger(t, srv.URL)
	input := validInput()
	input.BrokerAlias = "demo-broker"

	// Client request req-A arrives; middleware seeded its ID.
	ctxA := correlation.With(context.Background(), "req-A")
	if _, err := e.Exchange(ctxA, input); err != nil {
		t.Fatal(err)
	}
	demoDump(t, "FLOW 1 — cold path (cache miss → IdP → cached), request req-A", logs)

	// A later request req-B for the same broker: pure cache hit.
	ctxB := correlation.With(context.Background(), "req-B")
	if _, err := e.Exchange(ctxB, input); err != nil {
		t.Fatal(err)
	}
	demoDump(t, "FLOW 2 — cache hit, request req-B (no IdP, no detached lines)", logs)

	// ── Flow 3: singleflight — winner req-W plus waiter req-X ─────────────
	entered := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-gate
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("shared-token", 3600))
	}))
	defer srv3.Close()

	e3 := newTestExchanger(t, srv3.URL)
	input3 := validInput()
	input3.BrokerAlias = "demo-shared-broker"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = e3.Exchange(correlation.With(context.Background(), "req-W-winner"), input3)
	}()
	<-entered // winner's IdP call is in flight
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = e3.Exchange(correlation.With(context.Background(), "req-X-waiter"), input3)
	}()
	time.Sleep(150 * time.Millisecond) // demo-only: let the waiter join the flight
	close(gate)
	wg.Wait()
	demoDump(t, "FLOW 3 — burst: req-W-winner triggers the exchange, req-X-waiter shares it", logs)

	// ── Flow 4: caller cancels mid-exchange; detached call finishes ───────
	entered4 := make(chan struct{})
	gate4 := make(chan struct{})
	var once4 sync.Once
	srv4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once4.Do(func() { close(entered4) })
		<-gate4
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("orphaned-token", 3600))
	}))
	defer srv4.Close()

	e4 := newTestExchanger(t, srv4.URL)
	input4 := validInput()
	input4.BrokerAlias = "demo-bail-broker"

	cancelCtx, cancel := context.WithCancel(correlation.With(context.Background(), "req-C-cancelled"))
	done := make(chan struct{})
	go func() { _, _ = e4.Exchange(cancelCtx, input4); close(done) }()
	<-entered4
	cancel()
	<-done
	close(gate4)
	time.Sleep(300 * time.Millisecond) // demo-only: let the detached goroutine finish logging
	demoDump(t, "FLOW 4 — req-C-cancelled bails mid-exchange; detached call completes anyway", logs)

	// ── Flow 5: IdP failure (HTTP 500) ─────────────────────────────────────
	srv5 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv5.Close()

	e5 := newTestExchanger(t, srv5.URL)
	input5 := validInput()
	input5.BrokerAlias = "demo-error-broker"
	_, _ = e5.Exchange(correlation.With(context.Background(), "req-D-error"), input5)
	demoDump(t, "FLOW 5 — IdP returns 500 for request req-D-error", logs)

	// ── Flow 6: correlation disabled (no ID on the caller ctx) ────────────
	srv6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successJSON("plain-token", 3600))
	}))
	defer srv6.Close()

	e6 := newTestExchanger(t, srv6.URL)
	input6 := validInput()
	input6.BrokerAlias = "demo-disabled-broker"
	if _, err := e6.Exchange(context.Background(), input6); err != nil {
		t.Fatal(err)
	}
	demoDump(t, "FLOW 6 — OBS_CORRELATION_ID_ENABLED=false (no correlation_id anywhere)", logs)
}
