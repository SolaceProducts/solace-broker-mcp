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

package handlers

import (
	"strings"
	"testing"
)

// queue is a tiny helper to build a queue item with the four required fields.
// usageBytes is msgSpoolUsage in bytes; maxUsageMB is maxMsgSpoolUsage in
// megabytes — the same unit split the broker uses, so tests exercise the
// MB→bytes conversion in the near-full computation rather than papering over it.
func queue(bindCount, usageBytes, maxUsageMB float64, state string) map[string]any {
	return map[string]any{
		"bindCount":                     bindCount,
		"msgSpoolUsage":                 usageBytes,
		"maxMsgSpoolUsage":              maxUsageMB,
		"lowPriorityMsgCongestionState": state,
	}
}

func TestListQueues_Counts(t *testing.T) {
	items := []any{
		queue(0, 0, 100, "not-congested"),             // noConsumer
		queue(0, 90*bytesPerMB, 100, "congested"),     // noConsumer + congested + nearFull (0.90)
		queue(2, 99*bytesPerMB, 100, "not-congested"), // nearFull (0.99)
		queue(2, 50*bytesPerMB, 100, "not-congested"), // nothing (0.50)
		queue(1, 0, 0, "not-congested"),               // unbounded — skip nearFull
	}
	got, err := ListQueues(map[string]map[string]any{
		"queues": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{
		"noConsumerCount": 2,
		"congestedCount":  1,
		"nearFullCount":   2,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
	// backloggedCount was removed (it counted queues with cumulative
	// spooledMsgCount > 0, which is not a live backlog — SOL-150260).
	if _, present := got["backloggedCount"]; present {
		t.Errorf("backloggedCount should no longer be emitted, got %v", got["backloggedCount"])
	}
}

// TestListQueues_NearFullUnits is the regression guard for the bytes-vs-MB unit
// bug (SOL-150260). msgSpoolUsage is bytes; maxMsgSpoolUsage is MB. A queue
// using 0.8 MB (838860 bytes) of a 100 MB quota is at 0.8% — NOT near full. The
// pre-fix code compared 838860 / 100 = 8388 >= 0.8 and wrongly counted it.
func TestListQueues_NearFullUnits(t *testing.T) {
	items := []any{
		queue(1, 0.8*bytesPerMB, 100, "not-congested"), // 0.8% full — not near full
		queue(1, 80*bytesPerMB, 100, "not-congested"),  // 80% full — near full
	}
	got, err := ListQueues(map[string]map[string]any{"queues": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["nearFullCount"] != 1 {
		t.Errorf("nearFullCount: got %v, want 1 (only the 80%% queue is near full)", got["nearFullCount"])
	}
}

// TestListQueues_TruncationSurfaced asserts that when the paginator marks the
// step result truncated, the summary exposes scanned (item count) and
// truncated: true alongside the counts. Without this the LLM sees authoritative
// looking counts and a truncation flag buried in a sibling object, and tends to
// treat the counts as global rather than over the visible page.
func TestListQueues_TruncationSurfaced(t *testing.T) {
	items := []any{
		queue(0, 0, 100, "not-congested"),
		queue(2, 50*bytesPerMB, 100, "not-congested"),
	}
	t.Run("truncated", func(t *testing.T) {
		got, err := ListQueues(map[string]map[string]any{
			"queues": {"data": items, "truncated": true},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got["scanned"] != 2 {
			t.Errorf("scanned: got %v, want 2", got["scanned"])
		}
		if got["truncated"] != true {
			t.Errorf("truncated: got %v, want true", got["truncated"])
		}
	})
	t.Run("not truncated omits flag", func(t *testing.T) {
		got, err := ListQueues(map[string]map[string]any{
			"queues": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got["scanned"] != 2 {
			t.Errorf("scanned: got %v, want 2", got["scanned"])
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

func TestListQueues_Empty(t *testing.T) {
	got, err := ListQueues(map[string]map[string]any{
		"queues": {"data": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"noConsumerCount", "congestedCount", "nearFullCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
}

func TestListQueues_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "queues" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"queues": {"data": "oops"}},
			wantSub: "queues.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"queues": {"data": []any{"not-an-object"}}},
			wantSub: "queues.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListQueues(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestListQueues_MissingField asserts that omitting any one required field
// skips that queue rather than aborting the call. Table-driven over each
// field so reordering the handler's reads doesn't break the test for the
// wrong reason. Hard-failing on one bad row would drop the raw list too —
// a step down from the collect strategy.
func TestListQueues_MissingField(t *testing.T) {
	full := queue(0, 0, 100, "not-congested")
	// Sanity: full input must succeed, otherwise the omission cases prove nothing.
	if _, err := ListQueues(map[string]map[string]any{"queues": {"data": []any{full}}}); err != nil {
		t.Fatalf("baseline full input should pass, got %v", err)
	}
	for field := range full {
		t.Run(field, func(t *testing.T) {
			item := map[string]any{}
			for k, v := range full {
				if k != field {
					item[k] = v
				}
			}
			// Pair the broken row with a healthy one so we can also assert the
			// healthy row's signal still lands (the broken row in the baseline
			// is a noConsumer, so use a row that ISN'T to disambiguate).
			healthy := queue(2, 90*bytesPerMB, 100, "congested") // congested + nearFull
			got, err := ListQueues(map[string]map[string]any{"queues": {"data": []any{item, healthy}}})
			if err != nil {
				t.Fatalf("expected skip on missing %q, got error %v", field, err)
			}
			if got["skipped"] != 1 {
				t.Errorf("skipped: got %v, want 1", got["skipped"])
			}
			if got["scanned"] != 2 {
				t.Errorf("scanned: got %v, want 2", got["scanned"])
			}
			// Healthy row should still contribute.
			for _, k := range []string{"congestedCount", "nearFullCount"} {
				if got[k] != 1 {
					t.Errorf("%s: got %v, want 1 (healthy row should still count)", k, got[k])
				}
			}
		})
	}
}

// TestListQueues_NilField asserts that an explicit nil (what SEMP returns for
// an unset numeric field) is treated as a skip, not a hard fail. This is the
// motivating case for the skip-don't-abort behavior: one queue with a nil
// msgSpoolUsage shouldn't lose the raw list of every other queue.
func TestListQueues_NilField(t *testing.T) {
	bad := queue(0, 0, 100, "not-congested")
	bad["msgSpoolUsage"] = nil
	healthy := queue(2, 90*bytesPerMB, 100, "not-congested")
	got, err := ListQueues(map[string]map[string]any{"queues": {"data": []any{bad, healthy}}})
	if err != nil {
		t.Fatalf("nil field should skip, got %v", err)
	}
	if got["skipped"] != 1 {
		t.Errorf("skipped: got %v, want 1", got["skipped"])
	}
	if got["nearFullCount"] != 1 {
		t.Errorf("healthy row signals lost: %+v", got)
	}
}

// TestListQueues_SkippedOmittedWhenZero keeps the summary key set minimal in
// the common case — skipped is noise when nothing was skipped.
func TestListQueues_SkippedOmittedWhenZero(t *testing.T) {
	got, err := ListQueues(map[string]map[string]any{
		"queues": {"data": []any{queue(0, 0, 100, "not-congested")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}
