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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		httpStatus int
		want       BrokerState
	}{
		{"zero is unreachable (transport failure)", 0, StateUnreachable},
		{"200 is reachable", 200, StateReachable},
		{"201 is reachable", 201, StateReachable},
		{"299 is reachable", 299, StateReachable},
		{"401 is credential_invalid", 401, StateCredentialInvalid},
		{"403 is credential_invalid", 403, StateCredentialInvalid},
		{"404 is reachable (broker answered)", 404, StateReachable},
		{"409 is reachable (broker answered)", 409, StateReachable},
		{"429 is broker_error", 429, "broker_error_429"},
		{"500 is broker_error", 500, "broker_error_500"},
		{"503 is broker_error", 503, "broker_error_503"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Classify(tc.httpStatus)
			if got != tc.want {
				t.Errorf("Classify(%d) = %q, want %q", tc.httpStatus, got, tc.want)
			}
		})
	}
}

func TestBrokerTracker_AbsentUntilFirstCall(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()
	snap := tr.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot before any call, got %v", snap)
	}
}

func TestBrokerTracker_RecordBrokerResult(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		httpStatus int
		want       BrokerState
	}{
		{"success records reachable", 200, StateReachable},
		{"401 records credential_invalid", 401, StateCredentialInvalid},
		{"transport failure records unreachable", 0, StateUnreachable},
		{"503 records broker_error_503", 503, "broker_error_503"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := NewBrokerTracker()
			tr.RecordBrokerResult("prod", tc.httpStatus)
			snap := tr.Snapshot()
			got, ok := snap["prod"]
			if !ok {
				t.Fatal("expected broker 'prod' in snapshot")
			}
			if got != tc.want {
				t.Errorf("got state %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBrokerTracker_StateTransitions(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()

	tr.RecordBrokerResult("prod", 0) // unreachable
	if got := tr.Snapshot()["prod"]; got != StateUnreachable {
		t.Fatalf("after transport failure: got %q, want %q", got, StateUnreachable)
	}

	tr.RecordBrokerResult("prod", 401) // credential_invalid
	if got := tr.Snapshot()["prod"]; got != StateCredentialInvalid {
		t.Fatalf("after 401: got %q, want %q", got, StateCredentialInvalid)
	}

	tr.RecordBrokerResult("prod", 200) // recovered
	if got := tr.Snapshot()["prod"]; got != StateReachable {
		t.Fatalf("after recovery: got %q, want %q", got, StateReachable)
	}
}

func TestBrokerTracker_SnapshotReturnsCopy(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()
	tr.RecordBrokerResult("prod", 200)

	snap := tr.Snapshot()
	snap["prod"] = StateUnreachable // mutate the copy

	// Tracker's internal state must be unaffected.
	if got := tr.Snapshot()["prod"]; got != StateReachable {
		t.Errorf("mutation of returned snapshot leaked into tracker: got %q", got)
	}
}

func TestBrokerTracker_MultipleBrokers(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()
	tr.RecordBrokerResult("prod", 200)
	tr.RecordBrokerResult("dev", 0)
	tr.RecordBrokerResult("staging", 401)

	snap := tr.Snapshot()
	if snap["prod"] != StateReachable {
		t.Errorf("prod: got %q, want %q", snap["prod"], StateReachable)
	}
	if snap["dev"] != StateUnreachable {
		t.Errorf("dev: got %q, want %q", snap["dev"], StateUnreachable)
	}
	if snap["staging"] != StateCredentialInvalid {
		t.Errorf("staging: got %q, want %q", snap["staging"], StateCredentialInvalid)
	}
}

func TestBrokerTracker_TimestampRecorded(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()
	before := time.Now()
	tr.RecordBrokerResult("prod", 200)
	after := time.Now()

	snap := tr.SnapshotForMetrics()
	entry, ok := snap["prod"]
	if !ok {
		t.Fatal("expected broker 'prod' in snapshot")
	}
	if entry.LastResult.Before(before) || entry.LastResult.After(after) {
		t.Errorf("LastResult %v not in [%v, %v]", entry.LastResult, before, after)
	}
}

func TestBrokerTracker_OneHotSeenReasons(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()
	tr.RecordBrokerResult("prod", 0)   // unreachable
	tr.RecordBrokerResult("prod", 401) // credential_invalid
	tr.RecordBrokerResult("prod", 200) // recovered

	snap := tr.SnapshotForMetrics()
	entry, ok := snap["prod"]
	if !ok {
		t.Fatal("expected broker 'prod' in snapshot")
	}
	if entry.Current != StateReachable {
		t.Errorf("current: got %q, want %q", entry.Current, StateReachable)
	}
	seen := make(map[BrokerState]bool)
	for _, r := range entry.SeenReasons {
		seen[r] = true
	}
	if !seen[StateUnreachable] {
		t.Error("unreachable missing from SeenReasons after recovery")
	}
	if !seen[StateCredentialInvalid] {
		t.Error("credential_invalid missing from SeenReasons after recovery")
	}
}

// TestReadyzDecoupling asserts that recording a broker error does not affect
// the /readyz handler. ReadinessState holds no reference to BrokerTracker.
func TestReadyzDecoupling(t *testing.T) {
	t.Parallel()

	tr := NewBrokerTracker()
	tr.RecordBrokerResult("prod", 0) // unreachable

	readiness := NewReadinessState()
	readiness.SetInitialized()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/readyz", nil)
	rr := httptest.NewRecorder()
	ReadyzHandler(readiness).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("readyz returned %d after broker error, want 200", rr.Code)
	}
}
