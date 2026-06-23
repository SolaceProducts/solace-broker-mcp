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

// queue is a tiny helper to build a queue item with the five required fields.
func queue(bindCount, spooled, usage, maxUsage float64, state string) map[string]any {
	return map[string]any{
		"bindCount":                     bindCount,
		"spooledMsgCount":               spooled,
		"msgSpoolUsage":                 usage,
		"maxMsgSpoolUsage":              maxUsage,
		"lowPriorityMsgCongestionState": state,
	}
}

func TestListQueues_Counts(t *testing.T) {
	items := []any{
		queue(0, 0, 0, 100, "idle"),     // noConsumer
		queue(0, 50, 80, 100, "active"), // noConsumer + congested + backlogged + nearFull (0.8)
		queue(2, 10, 99, 100, "idle"),   // backlogged + nearFull (0.99)
		queue(2, 0, 50, 100, "idle"),    // nothing
		queue(1, 5, 0, 0, "idle"),       // backlogged only (unbounded — skip nearFull)
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
		"backloggedCount": 3,
		"nearFullCount":   2,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
}

// TestListQueues_TruncationSurfaced asserts that when the paginator marks the
// step result truncated, the summary exposes scanned (item count) and
// truncated: true alongside the counts. Without this the LLM sees authoritative
// looking counts and a truncation flag buried in a sibling object, and tends to
// treat the counts as global rather than over the visible page.
func TestListQueues_TruncationSurfaced(t *testing.T) {
	items := []any{
		queue(0, 0, 0, 100, "idle"),
		queue(2, 10, 50, 100, "idle"),
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
	for _, k := range []string{"noConsumerCount", "congestedCount", "backloggedCount", "nearFullCount"} {
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
// produces an error naming that specific field. Table-driven over each field
// rather than relying on the handler's read-order, so reordering the loop
// doesn't break the test for the wrong reason.
func TestListQueues_MissingField(t *testing.T) {
	full := queue(0, 0, 0, 100, "idle")
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
			_, err := ListQueues(map[string]map[string]any{"queues": {"data": []any{item}}})
			if err == nil {
				t.Fatalf("expected error when %q is missing, got nil", field)
			}
			if !strings.Contains(err.Error(), field) {
				t.Fatalf("error %q does not name the omitted field %q", err, field)
			}
		})
	}
}
