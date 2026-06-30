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

package main

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/observability/health"
)

// TestDrainAndShutdown_FlipsReadyzBeforeDelay proves the SIGTERM drain order
// required by SOL-151288 AC #2 and AC #5: SetShuttingDown happens FIRST (so
// /readyz reports 503 immediately, well within the 1s budget), the propagation
// window is honored, and only THEN is the server shut down.
//
// We assert the order without a multi-second sleep by using a short drain delay
// (30ms) and sampling IsShuttingDown from a watcher goroutine that fires almost
// immediately. The watcher observes "shutting down" while drainAndShutdown is
// still inside its delay, proving SetShuttingDown ran before Shutdown.
func TestDrainAndShutdown_FlipsReadyzBeforeDelay(t *testing.T) {
	srv, addr := startTestServer(t)
	readiness := health.NewReadinessState()
	readiness.SetInitialized()

	if readiness.IsShuttingDown() {
		t.Fatal("precondition: readiness must not be shutting down before drain")
	}

	const drainDelay = 100 * time.Millisecond

	// Sample readiness shortly after drain begins but well before the delay
	// elapses. At that point SetShuttingDown must already have run (it is the
	// first thing drainAndShutdown does) AND the server must still be accepting
	// connections (Shutdown only runs after the delay). The 10ms sample vs the
	// 100ms drain leaves a wide margin so CI scheduling jitter can't flip it.
	var midDrainShuttingDown, midDrainAccepting atomic.Bool
	sampled := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		midDrainShuttingDown.Store(readiness.IsShuttingDown())
		midDrainAccepting.Store(canConnect(addr))
		close(sampled)
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drainAndShutdown(ctx, srv, readiness, drainDelay); err != nil {
		t.Fatalf("drainAndShutdown returned error: %v", err)
	}
	elapsed := time.Since(start)

	<-sampled

	if !midDrainShuttingDown.Load() {
		t.Error("IsShuttingDown was false mid-drain; SetShuttingDown must run FIRST, before the delay")
	}
	if !midDrainAccepting.Load() {
		t.Error("server stopped accepting mid-drain; Shutdown must run AFTER the delay, not before")
	}
	if elapsed < drainDelay {
		t.Errorf("drainAndShutdown returned after %v, want at least the drain delay %v", elapsed, drainDelay)
	}

	// After drainAndShutdown returns, the server has shut down and no longer
	// accepts connections.
	if canConnect(addr) {
		t.Error("server still accepting connections after drainAndShutdown returned")
	}
}

// TestDrainAndShutdown_SetsShuttingDownEvenWithZeroDelay proves the readiness
// flip is unconditional on the drain delay: with a zero delay it still happens.
func TestDrainAndShutdown_SetsShuttingDownEvenWithZeroDelay(t *testing.T) {
	srv, _ := startTestServer(t)
	readiness := health.NewReadinessState()
	readiness.SetInitialized()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := drainAndShutdown(ctx, srv, readiness, 0); err != nil {
		t.Fatalf("drainAndShutdown returned error: %v", err)
	}
	if !readiness.IsShuttingDown() {
		t.Error("IsShuttingDown is false after drainAndShutdown with zero delay")
	}
}

// startTestServer starts a real *http.Server on a loopback ephemeral port and
// returns it with its address. It is registered for cleanup so a test that does
// not shut it down itself does not leak the goroutine.
func startTestServer(t *testing.T) (*http.Server, string) {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler:           http.NotFoundHandler(),
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	addr := ln.Addr().String()
	// Wait until the listener is actually accepting before returning so the
	// mid-drain "still accepting" assertion is meaningful.
	deadline := time.Now().Add(time.Second)
	for !canConnect(addr) {
		if time.Now().After(deadline) {
			t.Fatalf("test server never started accepting on %s", addr)
		}
		time.Sleep(time.Millisecond)
	}
	return srv, addr
}

// canConnect reports whether a TCP connection to addr can be established.
func canConnect(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
