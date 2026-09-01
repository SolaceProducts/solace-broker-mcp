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
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/defaults"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/health"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/hooks"
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
	if err := drainAndShutdown(srv, readiness, drainDelay, 5*time.Second, nil, nil); err != nil {
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

	if err := drainAndShutdown(srv, readiness, 0, 5*time.Second, nil, nil); err != nil {
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
	if err := drainAndShutdown(cs, readiness, drainDelay, shutdownTimeout, nil, nil); err != nil {
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

	err := gracefulShutdown(cs, 50*time.Millisecond, nil, nil)
	if err != wantErr {
		t.Errorf("gracefulShutdown returned %v, want the original Shutdown error %v", err, wantErr)
	}
	if !cs.closeCalled {
		t.Error("forced Close was not called after Shutdown failed; the fallback is missing")
	}
}

// blockingShutdowner is a shutdowner whose Shutdown blocks until Close is
// called (SOL-151437), so the "second signal forces close" paths can be driven
// deterministically without any real drain or shutdown sleeps. Shutdown also
// returns if its context is cancelled, so a broken force path fails the test via
// the timeout budget rather than hanging forever.
//
//   - closeCount records EVERY Close call (not just the first), so a test can
//     assert Close ran exactly once and would catch a production double-close —
//     a sync.Once guard would instead silently swallow the second call. The
//     channel close itself is still guarded by closeOnce so the fake cannot
//     panic on a double close while we measure it.
//   - shutdownReturned is closed when Shutdown actually returns, letting a test
//     prove the Shutdown goroutine unblocked and exited after a forced Close
//     (AC #5, "no goroutine leak") rather than trusting a comment.
type blockingShutdowner struct {
	closed           chan struct{}
	shutdownReturned chan struct{}
	closeOnce        sync.Once
	closeCount       atomic.Int64
}

func newBlockingShutdowner() *blockingShutdowner {
	return &blockingShutdowner{
		closed:           make(chan struct{}),
		shutdownReturned: make(chan struct{}),
	}
}

func (b *blockingShutdowner) Shutdown(ctx context.Context) error {
	defer close(b.shutdownReturned)
	select {
	case <-b.closed:
		return http.ErrServerClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *blockingShutdowner) Close() error {
	b.closeCount.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

// TestDrainAndShutdown_SecondSignalDuringDrainForcesClose proves AC #1
// (SOL-151437): a second signal arriving while the drain window is sleeping
// short-circuits it — forced Close, and the graceful step is skipped — instead
// of waiting the window out. We use a long drain window the test must NOT wait
// out; the second signal is pre-armed so the drain select picks it immediately.
func TestDrainAndShutdown_SecondSignalDuringDrainForcesClose(t *testing.T) {
	srv := newBlockingShutdowner()
	readiness := health.NewReadinessState()
	readiness.SetInitialized()

	// A drain window far longer than this test should ever run. If the second
	// signal is honored we return in microseconds; if it is dropped we fail
	// fast on the elapsed-time assertion below rather than waiting out the
	// full window and relying on the -timeout guard to catch the regression.
	const drainDelay = 3 * time.Second
	forceSig := make(chan os.Signal, 1)
	forceSig <- syscall.SIGTERM

	start := time.Now()
	if err := drainAndShutdown(srv, readiness, drainDelay, 30*time.Second, forceSig, nil); err != nil {
		t.Fatalf("drainAndShutdown returned error on forced close: %v", err)
	}
	elapsed := time.Since(start)

	if got := srv.closeCount.Load(); got != 1 {
		t.Errorf("forced Close called %d times for a second signal during the drain window, want exactly 1", got)
	}
	if elapsed >= drainDelay {
		t.Errorf("drainAndShutdown waited out the %v drain window (took %v); the second signal must short-circuit it", drainDelay, elapsed)
	}
	// Readiness must still have flipped before the forced exit (AC #5 / #2).
	if !readiness.IsShuttingDown() {
		t.Error("readiness did not flip to shutting-down before the forced exit")
	}
}

// TestDrainAndShutdown_SecondSignalDuringGracefulForcesClose proves AC #2
// (SOL-151437) through the real production call path: with a zero drain delay
// (the SIGINT case) drainAndShutdown goes straight to the graceful wait, and a
// second signal there forces an immediate Close that unblocks the in-flight
// Shutdown. blockingShutdowner.Shutdown blocks until Close, so the graceful wait
// is genuinely in progress when the signal arrives.
func TestDrainAndShutdown_SecondSignalDuringGracefulForcesClose(t *testing.T) {
	srv := newBlockingShutdowner()
	readiness := health.NewReadinessState()
	readiness.SetInitialized()

	forceSig := make(chan os.Signal, 1)
	forceSig <- syscall.SIGINT

	done := make(chan error, 1)
	go func() {
		done <- drainAndShutdown(srv, readiness, 0, 30*time.Second, forceSig, nil)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("drainAndShutdown returned %v on forced close, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("drainAndShutdown did not return after a second signal during the graceful wait; force path is broken")
	}
	if got := srv.closeCount.Load(); got != 1 {
		t.Errorf("forced Close called %d times for a second signal during the graceful wait, want exactly 1", got)
	}
	// AC #5 (no goroutine leak): the Shutdown goroutine spawned by
	// gracefulShutdown must actually unblock and exit after the forced Close,
	// not linger. Close (and the deferred context cancel) should release it.
	select {
	case <-srv.shutdownReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown goroutine did not return after forced Close; goroutine leak on the force path")
	}
}

// recordingHandler is a minimal slog.Handler that captures every record so a
// test can assert exactly what was logged. It records all levels; the test
// filters. Guarded by a mutex because slog handlers must be safe for concurrent
// Handle calls, though these tests log from a single goroutine.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// TestForceClose_LogsSingleWarnNamingSignal proves AC #4 (SOL-151437): a forced
// exit emits exactly ONE WARN line, it describes the forced shutdown, and it
// names the triggering signal. forceClose is the shared reaction for both the
// drain-window and graceful-wait force paths, so testing it once covers both.
// It swaps the process-global slog default, so it must not run in parallel with
// other tests in this package (none here call t.Parallel).
func TestForceClose_LogsSingleWarnNamingSignal(t *testing.T) {
	rec := &recordingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(rec))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := newBlockingShutdowner()
	forceClose(srv, syscall.SIGTERM)

	var warns []slog.Record
	for _, r := range rec.records {
		if r.Level == slog.LevelWarn {
			warns = append(warns, r)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("forceClose emitted %d WARN lines, want exactly 1", len(warns))
	}
	w := warns[0]
	if !strings.Contains(w.Message, "forcing immediate shutdown") {
		t.Errorf("WARN message %q does not describe the forced shutdown", w.Message)
	}
	var sigAttr string
	w.Attrs(func(a slog.Attr) bool {
		if a.Key == "signal" {
			sigAttr = a.Value.String()
		}
		return true
	})
	if sigAttr != syscall.SIGTERM.String() {
		t.Errorf("WARN signal attr = %q, want the triggering signal %q", sigAttr, syscall.SIGTERM.String())
	}
	if got := srv.closeCount.Load(); got != 1 {
		t.Errorf("forceClose invoked Close %d times, want exactly 1", got)
	}
}

// TestGracefulShutdown_RunsHooksAfterShutdown proves shutdownHooks actually
// gets invoked on a normal shutdown (SOL-152449) — the registry parameter
// isn't just plumbed through and ignored.
func TestGracefulShutdown_RunsHooksAfterShutdown(t *testing.T) {
	cs := &captureShutdowner{}
	registry := hooks.NewRegistry()
	var hookRan atomic.Bool
	registry.Register("test-hook", func(context.Context) error {
		hookRan.Store(true)
		return nil
	})

	if err := gracefulShutdown(cs, 50*time.Millisecond, nil, registry); err != nil {
		t.Fatalf("gracefulShutdown returned error: %v", err)
	}
	if !hookRan.Load() {
		t.Error("registered hook did not run after a normal shutdown")
	}
}

// TestGracefulShutdown_SecondSignalSkipsHooks proves AC #3 (SOL-152449): a
// second signal forcing an immediate close must skip registered hooks
// entirely — "stop now" outranks a telemetry flush.
func TestGracefulShutdown_SecondSignalSkipsHooks(t *testing.T) {
	srv := newBlockingShutdowner()
	registry := hooks.NewRegistry()
	var hookRan atomic.Bool
	registry.Register("test-hook", func(context.Context) error {
		hookRan.Store(true)
		return nil
	})

	forceSig := make(chan os.Signal, 1)
	forceSig <- syscall.SIGTERM

	if err := gracefulShutdown(srv, 30*time.Second, forceSig, registry); err != nil {
		t.Errorf("gracefulShutdown returned %v on forced close, want nil", err)
	}
	if hookRan.Load() {
		t.Error("registered hook ran despite a second-signal forced close; hooks must be skipped")
	}
}

// TestGracefulShutdown_SecondSignalDuringHookFlushCutsItShort proves a second
// signal WHILE hooks are running (SOL-152449) cancels the flush immediately
// instead of waiting out the budget — same "stop now" guarantee as the rest
// of shutdown.
func TestGracefulShutdown_SecondSignalDuringHookFlushCutsItShort(t *testing.T) {
	cs := &captureShutdowner{} // Shutdown returns nil immediately, no delay.
	registry := hooks.NewRegistry()

	hookStarted := make(chan struct{})
	hookCanceled := make(chan struct{})
	registry.Register("blocker", func(ctx context.Context) error {
		close(hookStarted)
		<-ctx.Done()
		close(hookCanceled)
		return ctx.Err()
	})

	forceSig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- gracefulShutdown(cs, 30*time.Second, forceSig, registry) }()

	select {
	case <-hookStarted:
	case <-time.After(time.Second):
		t.Fatal("hook never started; test cannot exercise the mid-flush signal")
	}

	start := time.Now()
	forceSig <- syscall.SIGTERM

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("gracefulShutdown returned %v on a second signal, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("gracefulShutdown did not return after a second signal during hook flush")
	}

	hookBudget := time.Duration(defaults.DefaultShutdownHookTimeoutSeconds) * time.Second
	if elapsed := time.Since(start); elapsed >= hookBudget {
		t.Errorf("gracefulShutdown took %v after the second signal, want well under the %v hook budget", elapsed, hookBudget)
	}

	select {
	case <-hookCanceled:
	case <-time.After(time.Second):
		t.Error("hook's context was never canceled after the second signal")
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
