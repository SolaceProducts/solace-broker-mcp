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

package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestReadinessState_Evaluate exercises the full decision domain of
// ReadinessState.Evaluate: starting (not initialized), ready, shutting_down
// precedence over both starting and ready, and an unready required listener.
func TestReadinessState_Evaluate(t *testing.T) {
	t.Parallel()

	failing := func(name string) (string, func() error) {
		return name, func() error { return errors.New("not bound") }
	}
	ok := func(name string) (string, func() error) {
		return name, func() error { return nil }
	}

	tests := []struct {
		name       string
		setup      func(s *ReadinessState)
		wantStatus string
		wantReady  bool
		// reasonHas, when non-empty, must be a substring of the returned reason.
		reasonHas string
	}{
		{
			name:       "not initialized yields starting",
			setup:      func(_ *ReadinessState) {},
			wantStatus: "starting",
			wantReady:  false,
		},
		{
			name:       "initialized with no listeners yields ready",
			setup:      func(s *ReadinessState) { s.SetInitialized() },
			wantStatus: "ready",
			wantReady:  true,
		},
		{
			name: "shutting down takes precedence over starting",
			setup: func(s *ReadinessState) {
				s.SetShuttingDown()
			},
			wantStatus: "shutting_down",
			wantReady:  false,
		},
		{
			name: "shutting down takes precedence over ready",
			setup: func(s *ReadinessState) {
				s.SetInitialized()
				s.SetShuttingDown()
			},
			wantStatus: "shutting_down",
			wantReady:  false,
		},
		{
			name: "initialized with a failing listener yields unready naming the listener",
			setup: func(s *ReadinessState) {
				s.SetInitialized()
				s.RegisterListener(failing("metrics-listener"))
			},
			wantStatus: "unready",
			wantReady:  false,
			reasonHas:  "metrics-listener",
		},
		{
			name: "first failing listener wins",
			setup: func(s *ReadinessState) {
				s.SetInitialized()
				s.RegisterListener(ok("first-ok"))
				s.RegisterListener(failing("second-bad"))
				s.RegisterListener(failing("third-bad"))
			},
			wantStatus: "unready",
			wantReady:  false,
			reasonHas:  "second-bad",
		},
		{
			name: "all listeners healthy yields ready",
			setup: func(s *ReadinessState) {
				s.SetInitialized()
				s.RegisterListener(ok("a"))
				s.RegisterListener(ok("b"))
			},
			wantStatus: "ready",
			wantReady:  true,
		},
		{
			name: "not initialized but a failing listener still reports starting",
			setup: func(s *ReadinessState) {
				s.RegisterListener(failing("metrics-listener"))
			},
			wantStatus: "starting",
			wantReady:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := NewReadinessState()
			tt.setup(s)

			gotStatus, gotReady, gotReason := s.Evaluate()
			if gotStatus != tt.wantStatus {
				t.Errorf("status = %q, want %q", gotStatus, tt.wantStatus)
			}
			if gotReady != tt.wantReady {
				t.Errorf("ready = %v, want %v", gotReady, tt.wantReady)
			}
			if tt.reasonHas != "" && !strings.Contains(gotReason, tt.reasonHas) {
				t.Errorf("reason = %q, want substring %q", gotReason, tt.reasonHas)
			}
		})
	}
}

// TestReadinessState_SetInitializedIdempotent confirms SetInitialized can be
// called repeatedly without changing the outcome.
func TestReadinessState_SetInitializedIdempotent(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()
	s.SetInitialized()
	s.SetInitialized()
	if status, ready, _ := s.Evaluate(); status != "ready" || !ready {
		t.Errorf("after double SetInitialized: status=%q ready=%v, want ready/true", status, ready)
	}
}

// TestReadinessState_Concurrent exercises concurrent writers and readers under
// -race: SetInitialized, SetShuttingDown, RegisterListener, and Evaluate run
// simultaneously. The assertion is the absence of a data race; the final state
// is non-deterministic by design.
func TestReadinessState_Concurrent(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers * 4)

	for i := 0; i < workers; i++ {
		go func() { defer wg.Done(); s.SetInitialized() }()
		go func() { defer wg.Done(); s.SetShuttingDown() }()
		go func() {
			defer wg.Done()
			s.RegisterListener("l", func() error { return nil })
		}()
		go func() {
			defer wg.Done()
			_, _, _ = s.Evaluate()
		}()
	}
	wg.Wait()
}

// TestReadinessState_ProbeRunsOutsideLock proves Evaluate does not hold the
// internal RWMutex while invoking a listener probe. The probe calls back into
// the same ReadinessState (RegisterListener, which takes the write lock); if
// Evaluate still held the read lock, that re-entrant call would deadlock. The
// test therefore deadlocks (and the package test binary times out) on a
// regression, and passes when probes run on a snapshot taken outside the lock.
func TestReadinessState_ProbeRunsOutsideLock(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()
	s.SetInitialized()

	probed := make(chan struct{}, 1)
	s.RegisterListener("reentrant", func() error {
		// Re-enter the state while the probe runs. This needs the write lock,
		// which would block forever if Evaluate held the read lock here.
		s.RegisterListener("added-during-probe", func() error { return nil })
		probed <- struct{}{}
		return nil
	})

	status, ready, _ := s.Evaluate()
	if status != "ready" || !ready {
		t.Errorf("Evaluate() = (%q, %v), want (\"ready\", true)", status, ready)
	}
	select {
	case <-probed:
	default:
		t.Fatal("probe was not invoked")
	}
}

// TestReadinessState_NilProbeFailsSafe proves RegisterListener with a nil status
// probe does not panic in Evaluate and instead reports unready with a reason
// that names the misconfigured listener, surfacing the misconfiguration on
// /readyz rather than crashing the handler.
func TestReadinessState_NilProbeFailsSafe(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()
	s.SetInitialized()
	s.RegisterListener("metrics-listener", nil)

	status, ready, reason := s.Evaluate() // must not panic
	if status != "unready" || ready {
		t.Errorf("Evaluate() = (%q, %v), want (\"unready\", false)", status, ready)
	}
	if !strings.Contains(reason, "metrics-listener") {
		t.Errorf("reason = %q, want it to name the listener", reason)
	}
}

// TestReadyzHandler_StatusMatrix pins the fixed-body /readyz cases — those whose
// response body is a constant string: starting (503), ready (200), and
// shutting_down precedence (503). The dynamic-reason unready case is covered by
// TestReadyzHandler_UnreadyReasonSafeJSON and non-GET by TestReadyzHandler_NonGET.
func TestReadyzHandler_StatusMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		setup    func(s *ReadinessState)
		wantCode int
		wantBody string // exact response body for these fixed-status cases
	}{
		{
			name:     "before init returns 503 starting",
			setup:    func(_ *ReadinessState) {},
			wantCode: http.StatusServiceUnavailable,
			wantBody: `{"status":"starting"}`,
		},
		{
			name:     "after init returns 200 ready",
			setup:    func(s *ReadinessState) { s.SetInitialized() },
			wantCode: http.StatusOK,
			wantBody: `{"status":"ready"}`,
		},
		{
			name: "draining returns 503 shutting_down even when initialized",
			setup: func(s *ReadinessState) {
				s.SetInitialized()
				s.SetShuttingDown()
			},
			wantCode: http.StatusServiceUnavailable,
			wantBody: `{"status":"shutting_down"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := NewReadinessState()
			tt.setup(s)

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
			ReadyzHandler(s).ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

// TestReadyzHandler_UnreadyReasonSafeJSON asserts the unready body is 503,
// valid JSON, names the failing listener, and that a listener error message
// containing a double quote is safely encoded (not hand-concatenated).
func TestReadyzHandler_UnreadyReasonSafeJSON(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()
	s.SetInitialized()
	// Error text intentionally contains a double quote to prove safe encoding.
	s.RegisterListener("metrics-listener", func() error {
		return errors.New(`bind failed on "0.0.0.0:9090"`)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	ReadyzHandler(s).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v (raw=%q)", err, rec.Body.String())
	}
	if body.Status != "unready" {
		t.Errorf("status = %q, want %q", body.Status, "unready")
	}
	if !strings.Contains(body.Reason, "metrics-listener") {
		t.Errorf("reason = %q, want it to name the listener", body.Reason)
	}
	if !strings.Contains(body.Reason, `"0.0.0.0:9090"`) {
		t.Errorf("reason = %q, want the quoted error text preserved (safe encoding)", body.Reason)
	}
}

// TestReadyzHandler_NonGET pins that any non-GET method is rejected with 405,
// mirroring the liveness/health probes.
func TestReadyzHandler_NonGET(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()
	s.SetInitialized()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), method, "/readyz", nil)
		ReadyzHandler(s).ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// TestReadyzHandler_NoBrokerDependency proves AC #4 structurally: ReadyzHandler
// is constructed solely from ReadinessState — it accepts no broker client,
// alias func, or probe func — so it cannot read broker state or make broker
// calls. An initialized, non-draining handler returns 200 with no broker in
// scope.
func TestReadyzHandler_NoBrokerDependency(t *testing.T) {
	t.Parallel()
	s := NewReadinessState()
	s.SetInitialized()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	ReadyzHandler(s).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (readiness must not depend on any broker)", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); got != `{"status":"ready"}` {
		t.Errorf("body = %q, want %q", got, `{"status":"ready"}`)
	}
}
