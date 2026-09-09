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

package hooks

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunAll_NilRegistryIsNoop proves a nil *Registry (the common case before
// any provider story registers a hook) costs nothing and never blocks.
func TestRunAll_NilRegistryIsNoop(t *testing.T) {
	var r *Registry
	start := time.Now()
	r.RunAll(context.Background())
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("RunAll on a nil Registry took %v, want near-instant", elapsed)
	}
}

// TestRunAll_EmptyRegistryIsNoop proves the same for a constructed but unused
// Registry — the inert case Stories 14/25/46 leave behind until they register.
func TestRunAll_EmptyRegistryIsNoop(t *testing.T) {
	r := NewRegistry()
	start := time.Now()
	r.RunAll(context.Background())
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("RunAll on an empty Registry took %v, want near-instant", elapsed)
	}
}

// TestRunAll_BoundedTotalWithBlockingHooks proves the core budget guarantee:
// three hooks that never return still leave RunAll bounded by ctx's deadline,
// and a fourth registered hook does not push the wall-clock bound out further.
func TestRunAll_BoundedTotalWithBlockingHooks(t *testing.T) {
	r := NewRegistry()
	block := func(ctx context.Context) error {
		<-ctx.Done() // only returns when RunAll's budget expires
		return nil
	}
	for i := 0; i < 4; i++ {
		r.Register("blocker", block)
	}

	const budget = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	r.RunAll(ctx)
	elapsed := time.Since(start)

	if elapsed > budget+100*time.Millisecond {
		t.Errorf("RunAll took %v with 4 blocking hooks, want bounded near the %v budget", elapsed, budget)
	}
}

// TestRunAll_SlowHookDoesNotStarveFastHook proves one stuck hook cannot
// prevent another registered hook from completing: both run concurrently, so
// the fast hook's own effect is observed even though the slow one is
// abandoned when the budget expires.
func TestRunAll_SlowHookDoesNotStarveFastHook(t *testing.T) {
	r := NewRegistry()
	r.Register("slow", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})

	var fastRan atomic.Bool
	r.Register("fast", func(context.Context) error {
		fastRan.Store(true)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	r.RunAll(ctx)

	if !fastRan.Load() {
		t.Error("fast hook did not run; a slow hook must not starve the others")
	}
}

// TestRunAll_PanickingHookDoesNotCrashProcess proves a panic in one hook is
// recovered (via safego.Go) rather than escaping the goroutine and crashing
// the process mid-shutdown — the panic equivalent of a failing hook must be
// contained the same way an ordinary error is.
func TestRunAll_PanickingHookDoesNotCrashProcess(t *testing.T) {
	r := NewRegistry()
	r.Register("panicker", func(context.Context) error {
		panic("boom")
	})

	var fastRan atomic.Bool
	r.Register("fast", func(context.Context) error {
		fastRan.Store(true)
		return nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunAll(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunAll did not return after a hook panicked")
	}
	if !fastRan.Load() {
		t.Error("fast hook did not run; a panicking hook must not prevent the others from running")
	}
}

// TestNames_NilRegistryReturnsNil matches RunAll's own nil-safety: a
// *Registry that was never constructed reports no registrations rather than
// panicking.
func TestNames_NilRegistryReturnsNil(t *testing.T) {
	var r *Registry
	if got := r.Names(); got != nil {
		t.Errorf("Names() on a nil Registry = %v, want nil", got)
	}
}

// TestNames_EmptyRegistryReturnsEmpty matches the constructed-but-unused case
// TestRunAll_EmptyRegistryIsNoop covers for RunAll.
func TestNames_EmptyRegistryReturnsEmpty(t *testing.T) {
	r := NewRegistry()
	if got := r.Names(); len(got) != 0 {
		t.Errorf("Names() on an empty Registry = %v, want empty", got)
	}
}

// TestNames_ReturnsRegisteredNamesInOrder is the core claim this accessor
// exists for (SOL-153965): a wiring test can see exactly what got registered,
// in the order it was registered, without needing to invoke RunAll or inspect
// hook behavior at all.
func TestNames_ReturnsRegisteredNamesInOrder(t *testing.T) {
	r := NewRegistry()
	noop := func(context.Context) error { return nil }
	r.Register("metrics_provider", noop)
	r.Register("tracer_provider", noop)

	want := []string{"metrics_provider", "tracer_provider"}
	if got := r.Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

// TestRunAll_HookErrorDoesNotPropagate proves a failing hook is swallowed
// (logged, not surfaced) — the shutdown exit-status decision belongs to the
// HTTP server's own outcome, not to a lost telemetry flush.
func TestRunAll_HookErrorDoesNotPropagate(t *testing.T) {
	r := NewRegistry()
	r.Register("failing", func(context.Context) error {
		return errors.New("flush failed")
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.RunAll(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunAll did not return after a hook error")
	}
}
