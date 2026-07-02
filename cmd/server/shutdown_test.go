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
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SolaceDev/solace-broker-mcp/internal/observability/health"
)

// TestDrainDelayForSignal proves the SIGINT-vs-SIGTERM drain policy (reviewer
// non-blocking comment #1, SOL-151288): SIGTERM (orchestrator-initiated) honors
// the configured drain window so K8s can deregister the pod; SIGINT (a local
// Ctrl-C) skips the drain entirely because there is no orchestrator to wait for.
func TestDrainDelayForSignal(t *testing.T) {
	tests := []struct {
		name       string
		sig        os.Signal
		configured time.Duration
		want       time.Duration
	}{
		{
			name:       "SIGINT skips the drain regardless of configured delay",
			sig:        os.Interrupt,
			configured: 10 * time.Second,
			want:       0,
		},
		{
			name:       "SIGTERM honors the configured drain window",
			sig:        syscall.SIGTERM,
			configured: 10 * time.Second,
			want:       10 * time.Second,
		},
		{
			name:       "SIGTERM with a zero configured delay stays zero",
			sig:        syscall.SIGTERM,
			configured: 0,
			want:       0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := drainDelayForSignal(tt.sig, tt.configured); got != tt.want {
				t.Errorf("drainDelayForSignal(%v, %v) = %v, want %v", tt.sig, tt.configured, got, tt.want)
			}
		})
	}
}

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
	if err := drainAndShutdown(srv, readiness, drainDelay, 5*time.Second); err != nil {
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

	if err := drainAndShutdown(srv, readiness, 0, 5*time.Second); err != nil {
		t.Fatalf("drainAndShutdown returned error: %v", err)
	}
	if !readiness.IsShuttingDown() {
		t.Error("IsShuttingDown is false after drainAndShutdown with zero delay")
	}
}

// captureShutdowner is a test seam standing in for *http.Server. It records the
// remaining time on the context handed to Shutdown, measured at the instant
// Shutdown is called, so a test can assert how much of the timeout budget is
// actually available then.
type captureShutdowner struct {
	shutdownCalledAt time.Time
	deadline         time.Time
	hadDeadline      bool
	shutdownErr      error // returned from Shutdown to exercise the fallback when set
	closeCalled      bool
}

func (c *captureShutdowner) Shutdown(ctx context.Context) error {
	c.shutdownCalledAt = time.Now()
	c.deadline, c.hadDeadline = ctx.Deadline()
	return c.shutdownErr
}

func (c *captureShutdowner) Close() error {
	c.closeCalled = true
	return nil
}

// remainingBudgetAtShutdown returns how much time was left on the Shutdown
// context at the moment Shutdown was invoked.
func (c *captureShutdowner) remainingBudgetAtShutdown() time.Duration {
	return c.deadline.Sub(c.shutdownCalledAt)
}

// TestDrainAndShutdown_ShutdownGetsFullBudgetAfterDrain proves the fix for the
// Copilot finding (SOL-151288): the shutdown-timeout budget is created AFTER the
// drain sleep, so the drain delay does NOT eat into the time Shutdown gets to
// drain in-flight requests.
//
// Under the old code the timeout context was built before the drain sleep, so
// at the moment Shutdown was called only (shutdownTimeout - drainDelay) of the
// budget remained. Using a deadline-capturing seam we assert the budget present
// AT THE SHUTDOWN CALL is the FULL shutdownTimeout (not shutdownTimeout minus
// the drain), with a wide lower bound that tolerates context-creation overhead.
// ms-scale drain delay; no multi-second sleeps.
func TestDrainAndShutdown_ShutdownGetsFullBudgetAfterDrain(t *testing.T) {
	const (
		drainDelay      = 40 * time.Millisecond
		shutdownTimeout = 30 * time.Second // the production default
	)

	cs := &captureShutdowner{}
	readiness := health.NewReadinessState()
	readiness.SetInitialized()

	start := time.Now()
	if err := drainAndShutdown(cs, readiness, drainDelay, shutdownTimeout); err != nil {
		t.Fatalf("drainAndShutdown returned error: %v", err)
	}

	// Readiness still flips, and Shutdown ran only after the drain delay.
	if !readiness.IsShuttingDown() {
		t.Error("IsShuttingDown is false after drainAndShutdown")
	}
	if cs.shutdownCalledAt.Sub(start) < drainDelay {
		t.Errorf("Shutdown was called %v after start, before the %v drain delay elapsed",
			cs.shutdownCalledAt.Sub(start), drainDelay)
	}
	if !cs.hadDeadline {
		t.Fatal("Shutdown context had no deadline; the timeout budget was not applied")
	}

	// The core assertion: the budget available when Shutdown is called is the
	// FULL shutdownTimeout, NOT shutdownTimeout - drainDelay. The old bug would
	// leave at most (shutdownTimeout - drainDelay). We require the remaining
	// budget to exceed that buggy ceiling by a wide margin, while staying just
	// under the full timeout (only context-creation overhead is lost).
	remaining := cs.remainingBudgetAtShutdown()
	buggyCeiling := shutdownTimeout - drainDelay
	if remaining <= buggyCeiling {
		t.Errorf("Shutdown got only %v of budget (<= %v), so the %v drain consumed the budget; "+
			"the timeout context must be created AFTER the drain sleep", remaining, buggyCeiling, drainDelay)
	}
	// Sanity upper bound: never more than the full timeout.
	if remaining > shutdownTimeout {
		t.Errorf("Shutdown budget %v exceeds the full timeout %v", remaining, shutdownTimeout)
	}
	// And it should be within a small slack of the full timeout.
	if shutdownTimeout-remaining > time.Second {
		t.Errorf("Shutdown budget %v is more than 1s short of the full %v timeout", remaining, shutdownTimeout)
	}
}

// TestGracefulShutdown_ForcedCloseFallback proves the Shutdown→Close fallback is
// preserved: when Shutdown returns an error, gracefulShutdown forces Close and
// propagates the original error.
func TestGracefulShutdown_ForcedCloseFallback(t *testing.T) {
	wantErr := context.DeadlineExceeded
	cs := &captureShutdowner{shutdownErr: wantErr}

	err := gracefulShutdown(cs, 50*time.Millisecond)
	if err != wantErr {
		t.Errorf("gracefulShutdown returned %v, want the original Shutdown error %v", err, wantErr)
	}
	if !cs.closeCalled {
		t.Error("forced Close was not called after Shutdown failed; the fallback is missing")
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
