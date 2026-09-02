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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

func testOperation() *sempv2.Operation {
	return &sempv2.Operation{ID: "testOp", Method: "GET", Path: "/SEMP/v2/monitor/test"}
}

// A pool that has realized no brokers reports nothing. Brokers are created
// lazily, so a configured-but-unused broker has no semaphore and no load; an
// empty snapshot is correct, not a missing reading.
func TestBrokerPool_OccupancySnapshot_SkipsUnrealizedBrokers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)
	defer pool.Close()

	if got := pool.OccupancySnapshot(); len(got) != 0 {
		t.Fatalf("OccupancySnapshot() on a fresh pool = %v, want empty", got)
	}

	// Realizing one broker must add exactly that one.
	if _, err := pool.GetSEMPv2("prod-us"); err != nil {
		t.Fatalf("GetSEMPv2: %v", err)
	}

	got := pool.OccupancySnapshot()
	if len(got) != 1 {
		t.Fatalf("OccupancySnapshot() = %v, want 1 entry after realizing one broker", got)
	}
	if got[0].Broker != "prod-us" {
		t.Errorf("Broker = %q, want prod-us", got[0].Broker)
	}
	if got[0].InFlight != 0 {
		t.Errorf("InFlight = %d, want 0 for an idle broker", got[0].InFlight)
	}
	if got[0].Limit != defaults.DefaultMaxConcurrentPerBroker {
		t.Errorf("Limit = %d, want the configured cap %d", got[0].Limit, defaults.DefaultMaxConcurrentPerBroker)
	}
}

// The count must track requests genuinely in flight, not merely be a plausible
// constant. A handler held open parks the request inside the semaphore, so the
// snapshot has to move to 1 and back to 0.
//
// This is what distinguishes the reading from a len/cap mix-up: a swapped pair
// reports the cap as occupancy and never changes.
func TestBrokerPool_OccupancySnapshot_TracksRequestsInFlight(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	pool := semp.NewBrokerPool(newTestServerConfig(t, server.URL), nil)
	defer pool.Close()

	client, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv2: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = client.Execute(context.Background(), testOperation(), map[string]any{})
	}()

	<-arrived // the request is now inside the handler, holding a semaphore slot.

	occ := occupancyFor(t, pool, "prod-us")
	if occ.InFlight != 1 {
		t.Errorf("InFlight while a request is held open = %d, want 1", occ.InFlight)
	}
	if occ.Limit != defaults.DefaultMaxConcurrentPerBroker {
		t.Errorf("Limit = %d, want %d", occ.Limit, defaults.DefaultMaxConcurrentPerBroker)
	}

	close(release)
	<-done

	// The slot is released on the way out of Sender.Do, which happens after
	// Execute returns to its caller, so poll rather than assert immediately.
	waitForInFlight(t, pool, "prod-us", 0)
}

// Two brokers under different load must be individually attributable — the
// whole point of keying this by alias. A single aggregate number cannot tell an
// operator which broker to look at.
func TestBrokerPool_OccupancySnapshot_DistinguishesBrokers(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer busy.Close()
	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer idle.Close()

	cfg := writeTestConfig(t, [2]string{"busy-broker", busy.URL}, [2]string{"idle-broker", idle.URL})
	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	busyClient, err := pool.GetSEMPv2("busy-broker")
	if err != nil {
		t.Fatalf("GetSEMPv2(busy-broker): %v", err)
	}
	idleClient, err := pool.GetSEMPv2("idle-broker")
	if err != nil {
		t.Fatalf("GetSEMPv2(idle-broker): %v", err)
	}
	// Realize the idle broker's client with a completed call, so both brokers
	// are in the snapshot and only their occupancy differs.
	if _, err := idleClient.Execute(context.Background(), testOperation(), map[string]any{}); err != nil {
		t.Fatalf("Execute(idle-broker): %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = busyClient.Execute(context.Background(), testOperation(), map[string]any{})
	}()
	<-arrived

	if got := occupancyFor(t, pool, "busy-broker").InFlight; got != 1 {
		t.Errorf("busy-broker InFlight = %d, want 1", got)
	}
	if got := occupancyFor(t, pool, "idle-broker").InFlight; got != 0 {
		t.Errorf("idle-broker InFlight = %d, want 0", got)
	}

	close(release)
	<-done
}

// The snapshot is sorted by alias so successive readings line up in a log.
func TestBrokerPool_OccupancySnapshot_SortedByAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{})
	}))
	defer server.Close()

	cfg := writeTestConfig(t,
		[2]string{"zulu", server.URL},
		[2]string{"alpha", server.URL},
		[2]string{"mike", server.URL},
	)
	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	for _, alias := range []string{"zulu", "alpha", "mike"} {
		if _, err := pool.GetSEMPv2(alias); err != nil {
			t.Fatalf("GetSEMPv2(%s): %v", alias, err)
		}
	}

	got := pool.OccupancySnapshot()
	want := []string{"alpha", "mike", "zulu"}
	if len(got) != len(want) {
		t.Fatalf("OccupancySnapshot() = %v, want %d entries", got, len(want))
	}
	for i, alias := range want {
		if got[i].Broker != alias {
			t.Errorf("entry %d = %q, want %q (snapshot must be sorted by alias)", i, got[i].Broker, alias)
		}
	}
}

// The end-to-end wiring test for the flag: OBS_SATURATION_EVENTS_ENABLED must
// actually reach the Sender that emits the warning. The resilience package
// proves the signal works when its option is set; only this proves the pool
// sets it.
//
// Without it, a pool that silently dropped the option would still pass every
// other test in this change.
func TestBrokerPool_SaturationEventsEnabled_ReachesTheSender(t *testing.T) {
	logs := captureSempLogs(t)

	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
	}))
	// Registered after the server so LIFO releases the parked handlers BEFORE
	// server.Close, which otherwise blocks forever waiting on them.
	defer server.Close()
	defer close(release)

	cfg := writeTestConfig(t, [2]string{"prod-us", server.URL})
	cfg.Observability.SaturationEventsEnabled = true
	cfg.Observability.SaturationThresholdMs = 20
	// One slot, so the second caller is forced to queue at the concurrency gate
	// for longer than the threshold.
	cfg.SEMP.MaxConcurrentPerBroker = 1

	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	client, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv2: %v", err)
	}

	go func() { _, _ = client.Execute(context.Background(), testOperation(), map[string]any{}) }()
	<-arrived // the first request now holds the only slot.

	blocked, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = client.Execute(blocked, testOperation(), map[string]any{}) }()

	waitForLogMessage(t, logs, "broker admission slow: request still waiting to be admitted")
}

// And with the flag off, the same setup must stay silent — otherwise the test
// above would pass on an implementation that ignores the flag entirely.
func TestBrokerPool_SaturationEventsDisabled_SenderStaysSilent(t *testing.T) {
	logs := captureSempLogs(t)

	arrived := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release
	}))
	// Registered after the server so LIFO releases the parked handlers BEFORE
	// server.Close, which otherwise blocks forever waiting on them.
	defer server.Close()
	defer close(release)

	cfg := writeTestConfig(t, [2]string{"prod-us", server.URL})
	cfg.Observability.SaturationThresholdMs = 20
	cfg.SEMP.MaxConcurrentPerBroker = 1
	// SaturationEventsEnabled deliberately left false.

	pool := semp.NewBrokerPool(cfg, nil)
	defer pool.Close()

	client, err := pool.GetSEMPv2("prod-us")
	if err != nil {
		t.Fatalf("GetSEMPv2: %v", err)
	}

	go func() { _, _ = client.Execute(context.Background(), testOperation(), map[string]any{}) }()
	<-arrived

	blocked, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = client.Execute(blocked, testOperation(), map[string]any{}) }()

	time.Sleep(200 * time.Millisecond) // ten thresholds.

	if n := logs.count("broker admission slow: request still waiting to be admitted"); n != 0 {
		t.Errorf("got %d slow-admission warnings with the capability off, want 0", n)
	}
}

// sempLogCapture collects log output written by Sender goroutines while the
// test goroutine reads it. The mutex is load-bearing under -race.
type sempLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *sempLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *sempLogCapture) count(msg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Count(c.buf.String(), msg)
}

func captureSempLogs(t *testing.T) *sempLogCapture {
	t.Helper()
	c := &sempLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

func waitForLogMessage(t *testing.T, logs *sempLogCapture, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if logs.count(msg) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for a %q log line", msg)
}

func occupancyFor(t *testing.T, pool *semp.BrokerPool, alias string) health.BrokerOccupancy {
	t.Helper()
	for _, occ := range pool.OccupancySnapshot() {
		if occ.Broker == alias {
			return occ
		}
	}
	t.Fatalf("no occupancy entry for %q in %v", alias, pool.OccupancySnapshot())
	return health.BrokerOccupancy{}
}

func waitForInFlight(t *testing.T, pool *semp.BrokerPool, alias string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if occupancyFor(t, pool, alias).InFlight == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s InFlight to reach %d; last = %d",
		alias, want, occupancyFor(t, pool, alias).InFlight)
}
