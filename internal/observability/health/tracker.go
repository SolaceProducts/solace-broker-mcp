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
	"fmt"
	"sort"
	"sync"
	"time"
)

// BrokerState is the reason label value used by the broker reachability gauges.
type BrokerState string

const (
	StateReachable         BrokerState = "reachable"
	StateUnreachable       BrokerState = "unreachable"
	StateCredentialInvalid BrokerState = "credential_invalid" // #nosec G101 -- metric label value, not a credential
)

// brokerErrorState returns the broker_error_NNN label value for 5xx and 429 responses.
func brokerErrorState(httpStatus int) BrokerState {
	return BrokerState(fmt.Sprintf("broker_error_%d", httpStatus))
}

// Classify maps an HTTP status code to a BrokerState. 0 means no HTTP response
// (transport failure). 4xx other than 401/403 is classified as reachable: the
// broker answered, so connectivity is proven.
func Classify(httpStatus int) BrokerState {
	switch {
	case httpStatus == 0:
		return StateUnreachable
	case httpStatus == 401 || httpStatus == 403:
		return StateCredentialInvalid
	case httpStatus >= 200 && httpStatus < 300:
		return StateReachable
	case httpStatus == 429 || (httpStatus >= 500 && httpStatus < 600):
		return brokerErrorState(httpStatus)
	default:
		// Other 4xx: broker answered, so reachability is proven.
		return StateReachable
	}
}

// brokerEntry holds the current state, all non-reachable states ever seen, and
// the time of the most recent result for a single broker.
type brokerEntry struct {
	current     BrokerState
	seenReasons map[BrokerState]struct{}
	lastResult  time.Time
}

// BrokerSnapshot is returned by SnapshotForMetrics. Current is the state from
// the last call. SeenReasons is every non-reachable state ever recorded for
// this broker, sorted for determinism. LastResult is when the most recent call
// completed.
type BrokerSnapshot struct {
	Current     BrokerState
	SeenReasons []BrokerState
	LastResult  time.Time
}

// BrokerTracker maintains per-broker reachability state updated passively from
// every real SEMP call. Safe for concurrent use.
type BrokerTracker struct {
	mu      sync.RWMutex
	entries map[string]*brokerEntry
}

// NewBrokerTracker returns a BrokerTracker with no recorded state.
func NewBrokerTracker() *BrokerTracker {
	return &BrokerTracker{entries: make(map[string]*brokerEntry)}
}

// RecordBrokerResult updates the tracker. httpStatus is 0 when no HTTP response was received.
func (t *BrokerTracker) RecordBrokerResult(broker string, httpStatus int) {
	state := Classify(httpStatus)
	now := time.Now()
	t.mu.Lock()
	e, ok := t.entries[broker]
	if !ok {
		e = &brokerEntry{seenReasons: make(map[BrokerState]struct{})}
		t.entries[broker] = e
	}
	e.current = state
	e.lastResult = now
	if state != StateReachable {
		e.seenReasons[state] = struct{}{}
	}
	t.mu.Unlock()
}

// Snapshot returns the current state per broker. Used by tests.
func (t *BrokerTracker) Snapshot() map[string]BrokerState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]BrokerState, len(t.entries))
	for k, e := range t.entries {
		out[k] = e.current
	}
	return out
}

// SnapshotForMetrics returns the full picture needed by the gauge callback: current
// state, all non-reachable states ever seen (for the one-hot contract), and the
// timestamp of the most recent result.
func (t *BrokerTracker) SnapshotForMetrics() map[string]BrokerSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]BrokerSnapshot, len(t.entries))
	for broker, e := range t.entries {
		reasons := make([]BrokerState, 0, len(e.seenReasons))
		for r := range e.seenReasons {
			reasons = append(reasons, r)
		}
		sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })
		out[broker] = BrokerSnapshot{
			Current:     e.current,
			SeenReasons: reasons,
			LastResult:  e.lastResult,
		}
	}
	return out
}
