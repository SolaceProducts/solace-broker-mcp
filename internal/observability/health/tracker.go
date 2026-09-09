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
	"sync"
)

// BrokerState is the reason label value used by the broker reachability gauges.
type BrokerState string

const (
	StateReachable         BrokerState = "reachable"
	StateUnreachable       BrokerState = "unreachable"
	StateCredentialInvalid BrokerState = "credential_invalid" // #nosec G101 -- metric label value, not a credential
)

// brokerErrorState returns the broker_error_NNN label value for non-2xx, non-401 responses.
func brokerErrorState(httpStatus int) BrokerState {
	return BrokerState(fmt.Sprintf("broker_error_%d", httpStatus))
}

// Classify maps an HTTP status code to a BrokerState. 0 means no HTTP response (transport failure).
func Classify(httpStatus int) BrokerState {
	switch {
	case httpStatus == 0:
		return StateUnreachable
	case httpStatus == 401:
		return StateCredentialInvalid
	case httpStatus >= 200 && httpStatus < 300:
		return StateReachable
	default:
		return brokerErrorState(httpStatus)
	}
}

// BrokerTracker maintains per-broker reachability state updated passively from
// every real SEMP call. Safe for concurrent use.
type BrokerTracker struct {
	mu     sync.RWMutex
	states map[string]BrokerState
}

// NewBrokerTracker returns a BrokerTracker with no recorded state.
func NewBrokerTracker() *BrokerTracker {
	return &BrokerTracker{states: make(map[string]BrokerState)}
}

// RecordBrokerResult updates the tracker. httpStatus is 0 when no HTTP response was received.
func (t *BrokerTracker) RecordBrokerResult(broker string, httpStatus int) {
	state := Classify(httpStatus)
	t.mu.Lock()
	t.states[broker] = state
	t.mu.Unlock()
}

// Snapshot returns a copy of the current per-broker state map.
func (t *BrokerTracker) Snapshot() map[string]BrokerState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]BrokerState, len(t.states))
	for k, v := range t.states {
		out[k] = v
	}
	return out
}

// SnapshotStrings returns the current state as map[broker]reason strings for the metrics snap callback.
func (t *BrokerTracker) SnapshotStrings() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]string, len(t.states))
	for k, v := range t.states {
		out[k] = string(v)
	}
	return out
}
