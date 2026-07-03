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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
)

// Fixed /readyz response bodies. These statuses carry no variable data, so they
// are constant strings. The unready body is built with json.Marshal instead
// (see ReadyzHandler) because its reason is dynamic and may contain characters
// that require escaping.
const (
	readyBody        = `{"status":"ready"}`
	startingBody     = `{"status":"starting"}`
	shuttingDownBody = `{"status":"shutting_down"}`
)

// listenerProbe pairs a required-listener name with its status function. The
// status function returns nil when the listener is bound and serving, or a
// non-nil error describing why it is not ready.
type listenerProbe struct {
	name   string
	status func() error
}

// ReadinessState tracks the MCP server's OWN readiness and nothing else. It is
// deliberately decoupled from broker reachability (ADR-004 / ISSUE-026): it
// holds no broker client, makes no broker calls, and runs no background
// goroutine. Readiness is a function of three local facts only:
//
//  1. initialized   — startup completed (SetInitialized).
//  2. shuttingDown  — graceful drain has begun (SetShuttingDown). The mechanism
//     lives here; cmd/server wires it to SIGTERM in drainAndShutdown (SOL-151288).
//  3. listeners      — required-listener probes (RegisterListener) that report
//     whether each listener is bound.
//
// ReadinessState is safe for concurrent use. The /readyz handler reads it under
// load while startup and shutdown write it. initialized and shuttingDown are
// atomic booleans; the listener slice is guarded by an RWMutex.
type ReadinessState struct {
	initialized  atomic.Bool
	shuttingDown atomic.Bool

	mu        sync.RWMutex
	listeners []listenerProbe
}

// NewReadinessState returns a ReadinessState in the not-initialized,
// not-shutting-down state with no registered listeners. In that state Evaluate
// reports "starting".
func NewReadinessState() *ReadinessState {
	return &ReadinessState{}
}

// SetInitialized marks startup as complete. It is idempotent — calling it more
// than once has no additional effect.
func (s *ReadinessState) SetInitialized() {
	s.initialized.Store(true)
}

// SetShuttingDown marks that graceful drain has begun. Once set, Evaluate
// reports "shutting_down" regardless of initialization or listener state, so an
// orchestrator stops routing new traffic to this instance. This is the
// mechanism; cmd/server calls it from the SIGTERM handler (drainAndShutdown,
// SOL-151288) before sleeping the drain window and shutting down.
func (s *ReadinessState) SetShuttingDown() {
	s.shuttingDown.Store(true)
}

// IsShuttingDown reports whether graceful drain has begun.
func (s *ReadinessState) IsShuttingDown() bool {
	return s.shuttingDown.Load()
}

// RegisterListener registers a required-listener probe. status must return nil
// when the listener is bound and serving, or a non-nil error describing why it
// is not. Once initialized and not draining, /readyz is unready while any
// registered probe returns an error.
//
// This is forward-looking: no listener is registered today. The only planned
// required listener is the /metrics listener (Story 15), which does not exist
// yet. The mechanism is built now so that story can register its probe without
// re-plumbing readiness.
func (s *ReadinessState) RegisterListener(name string, status func() error) {
	// Fail safe on a nil probe: a nil status would panic when Evaluate calls it.
	// Substitute a probe that always reports an error so /readyz surfaces the
	// misconfiguration as "unready" (naming the listener) instead of crashing.
	if status == nil {
		status = func() error { return fmt.Errorf("listener %q has no status probe", name) }
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listeners = append(s.listeners, listenerProbe{name: name, status: status})
}

// Evaluate computes the current readiness verdict. Decision order:
//
//  1. shutting down  → ("shutting_down", false, "")   — takes precedence.
//  2. not initialized → ("starting", false, "")
//  3. first listener whose status() != nil → ("unready", false, "<name>: <err>")
//  4. otherwise       → ("ready", true, "")
//
// The shutting-down check is first by design: a draining instance must report
// not-ready even if it is fully initialized with all listeners bound.
func (s *ReadinessState) Evaluate() (status string, ready bool, reason string) {
	if s.IsShuttingDown() {
		return "shutting_down", false, ""
	}
	if !s.initialized.Load() {
		return "starting", false, ""
	}

	// Snapshot the listener slice under the read lock, then release it before
	// invoking any probe. Probe callbacks are arbitrary code that may call back
	// into ReadinessState (e.g. RegisterListener, which takes the write lock);
	// running them under the read lock could deadlock and needlessly blocks
	// RegisterListener while probes run. The snapshot preserves registration
	// order, so "first failing listener wins" is unchanged.
	s.mu.RLock()
	probes := make([]listenerProbe, len(s.listeners))
	copy(probes, s.listeners)
	s.mu.RUnlock()

	for _, l := range probes {
		if err := l.status(); err != nil {
			return "unready", false, fmt.Sprintf("%s: %v", l.name, err)
		}
	}
	return "ready", true, ""
}

// ReadyzHandler returns the unconditional /readyz handler reflecting the MCP
// server's OWN readiness. It is UNCONDITIONAL by design — there is no flag to
// disable it (no OBS_READYZ_STRICT_ENABLED). It is decoupled from the broker:
// it is constructed solely from ReadinessState and therefore makes no broker
// calls and reads no broker state (ADR-004 / ISSUE-026).
//
// GET responds with application/json and:
//   - 200 {"status":"ready"}                       when ready
//   - 503 {"status":"starting"}                    before SetInitialized
//   - 503 {"status":"shutting_down"}               while draining (precedence)
//   - 503 {"status":"unready","reason":"<...>"}    when a required listener is down
//
// Any non-GET method responds 405, mirroring the liveness/health probes.
func ReadyzHandler(state *ReadinessState) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		status, ready, reason := state.Evaluate()
		w.Header().Set("Content-Type", "application/json")

		if ready {
			w.WriteHeader(http.StatusOK)
			writeBody(w, []byte(readyBody))
			return
		}

		w.WriteHeader(http.StatusServiceUnavailable)
		switch status {
		case "starting":
			writeBody(w, []byte(startingBody))
		case "shutting_down":
			writeBody(w, []byte(shuttingDownBody))
		default: // "unready" — reason is dynamic, so encode it safely.
			body, err := json.Marshal(struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			}{Status: "unready", Reason: reason})
			if err != nil {
				// reason is a plain string, so marshalling cannot realistically
				// fail; fall back to a reason-less body rather than emitting
				// malformed JSON.
				body = []byte(`{"status":"unready"}`)
			}
			writeBody(w, body)
		}
	})
}

// writeBody writes the response body best-effort. The caller has already called
// WriteHeader, so the status line and headers are committed: a write error here
// cannot be turned into a different response. Attempting a second http.Error
// would only append a stray body to an already-committed response. We therefore
// log the failure at debug and return — the no-double-write convention for any
// handler that has already committed its status.
func writeBody(w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		slog.Debug("readyz: failed to write response body", slog.String("error", err.Error()))
	}
}
