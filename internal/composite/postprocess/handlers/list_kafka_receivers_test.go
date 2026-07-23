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

// kafkaReceiver builds a Kafka Receiver item with the three required fields.
func kafkaReceiver(up, enabled bool, reason string) map[string]any {
	return map[string]any{
		"up":            up,
		"enabled":       enabled,
		"failureReason": reason,
	}
}

func TestListKafkaReceivers_Counts(t *testing.T) {
	items := []any{
		kafkaReceiver(true, true, ""),                          // healthy
		kafkaReceiver(false, true, "connection refused"),       // down (enabled && !up) + bucketed
		kafkaReceiver(false, false, "Kafka Receiver Shutdown"), // disabled only — NOT down (admin-disabled excluded), NOT bucketed
		kafkaReceiver(true, false, ""),                         // disabled only
		kafkaReceiver(false, true, "connection refused"),       // down (enabled && !up) + same bucket
	}
	got, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{
		"downCount":     2,
		"disabledCount": 2,
		"scanned":       5,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
	byReason, ok := got["byFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byFailureReason: wrong type %T", got["byFailureReason"])
	}
	if byReason["connection refused"] != 2 || len(byReason) != 1 {
		t.Errorf("byFailureReason: got %v, want {connection refused:2}", byReason)
	}
	if _, present := byReason["Kafka Receiver Shutdown"]; present {
		t.Errorf("admin-disabled receiver's reason must not bucket, got %v", byReason)
	}
}

// TestListKafkaReceivers_UnexpectedFailureFilter is the load-bearing test for
// the byFailureReason design choice: the map reflects UNEXPECTED active
// failures (down && enabled && non-empty reason). Mirrors the same three
// exclusion classes as list-rdps' byLastFailureReason.
func TestListKafkaReceivers_UnexpectedFailureFilter(t *testing.T) {
	items := []any{
		kafkaReceiver(false, true, "boom"),                     // down + enabled + reason → bucketed
		kafkaReceiver(false, false, "Kafka Receiver Shutdown"), // admin-disabled → MUST NOT bucket
		kafkaReceiver(true, true, "old boom"),                  // recovered with historical reason → MUST NOT bucket
		kafkaReceiver(false, true, ""),                         // enabled && !up with empty reason → counted in downCount, NOT bucketed
	}
	got, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["downCount"] != 2 {
		t.Errorf("downCount: got %v, want 2", got["downCount"])
	}
	byReason := got["byFailureReason"].(map[string]int)
	if len(byReason) != 1 || byReason["boom"] != 1 {
		t.Errorf("byFailureReason should contain only {boom:1}, got %v", byReason)
	}
	for _, leaked := range []string{"", "old boom", "Kafka Receiver Shutdown"} {
		if _, present := byReason[leaked]; present {
			t.Errorf("%q leaked into byFailureReason: %v", leaked, byReason)
		}
	}
}

func TestListKafkaReceivers_TruncationSurfaced(t *testing.T) {
	items := []any{kafkaReceiver(true, true, ""), kafkaReceiver(false, true, "x")}
	t.Run("truncated", func(t *testing.T) {
		got, err := ListKafkaReceivers(map[string]map[string]any{
			"kafkaReceivers": {"data": items, "truncated": true},
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
		got, err := ListKafkaReceivers(map[string]map[string]any{
			"kafkaReceivers": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

// TestListKafkaReceivers_Empty pins the AC's shape for an empty receiver
// list: all counts zero, byFailureReason an empty (not absent, not nil) map,
// no skipped or truncated keys.
func TestListKafkaReceivers_Empty(t *testing.T) {
	got, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"downCount", "disabledCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
	if got["scanned"] != 0 {
		t.Errorf("scanned: got %v, want 0", got["scanned"])
	}
	byReason, ok := got["byFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byFailureReason: want map[string]int, got %T", got["byFailureReason"])
	}
	if len(byReason) != 0 {
		t.Errorf("byFailureReason: want empty map, got %v", byReason)
	}
	for _, k := range []string{"skipped", "truncated"} {
		if _, present := got[k]; present {
			t.Errorf("%s should be omitted on empty input, got %v", k, got[k])
		}
	}
}

func TestListKafkaReceivers_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "kafkaReceivers" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"kafkaReceivers": {"data": "oops"}},
			wantSub: "kafkaReceivers.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"kafkaReceivers": {"data": []any{"not-an-object"}}},
			wantSub: "kafkaReceivers.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListKafkaReceivers(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestListKafkaReceivers_MissingField asserts that omitting any one required
// field skips that receiver rather than aborting — mirrors listRdps'
// robustness, so one malformed row doesn't drop the raw list of every other
// receiver. Table-driven over each field so reordering the handler's reads
// doesn't break the test for the wrong reason.
func TestListKafkaReceivers_MissingField(t *testing.T) {
	full := kafkaReceiver(false, true, "boom")
	if _, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": []any{full}}}); err != nil {
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
			// Healthy row lands in every counter we assert: down (enabled &&
			// !up) + bucketed (down && enabled && reason). The skipped row
			// must not steal any of those signals.
			healthy := kafkaReceiver(false, true, "other")
			got, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": []any{item, healthy}}})
			if err != nil {
				t.Fatalf("expected skip on missing %q, got error %v", field, err)
			}
			if got["skipped"] != 1 {
				t.Errorf("skipped: got %v, want 1", got["skipped"])
			}
			if got["scanned"] != 2 {
				t.Errorf("scanned: got %v, want 2", got["scanned"])
			}
			if got["downCount"] != 1 || got["disabledCount"] != 0 {
				t.Errorf("healthy row signals lost or wrong: %+v", got)
			}
			byReason := got["byFailureReason"].(map[string]int)
			if byReason["other"] != 1 {
				t.Errorf("healthy row failed to bucket reason: %v", byReason)
			}
		})
	}
}

// TestListKafkaReceivers_NilField asserts that an explicit nil (what SEMP
// returns for an unset field) is treated as a skip, not a hard fail.
func TestListKafkaReceivers_NilField(t *testing.T) {
	bad := kafkaReceiver(false, true, "boom")
	bad["up"] = nil
	healthy := kafkaReceiver(false, true, "x")
	got, err := ListKafkaReceivers(map[string]map[string]any{"kafkaReceivers": {"data": []any{bad, healthy}}})
	if err != nil {
		t.Fatalf("nil field should skip, got %v", err)
	}
	if got["skipped"] != 1 {
		t.Errorf("skipped: got %v, want 1", got["skipped"])
	}
	if got["downCount"] != 1 {
		t.Errorf("healthy row signals lost: %+v", got)
	}
}

// TestListKafkaReceivers_SkippedOmittedWhenZero keeps the summary key set
// minimal in the common case — skipped is noise when nothing was skipped.
func TestListKafkaReceivers_SkippedOmittedWhenZero(t *testing.T) {
	got, err := ListKafkaReceivers(map[string]map[string]any{
		"kafkaReceivers": {"data": []any{kafkaReceiver(true, true, "")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}
