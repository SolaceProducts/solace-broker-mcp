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

// rdp builds an RDP item with the three required fields.
func rdp(up, enabled bool, reason string) map[string]any {
	return map[string]any{
		"up":                up,
		"enabled":           enabled,
		"lastFailureReason": reason,
	}
}

func TestListRdps_Counts(t *testing.T) {
	items := []any{
		rdp(true, true, ""),                    // healthy
		rdp(false, true, "connection refused"), // down (enabled && !up) + bucketed
		rdp(false, false, "RDP Shutdown"),      // disabled only — NOT down (admin-disabled excluded), NOT bucketed
		rdp(true, false, ""),                   // disabled only
		rdp(false, true, "connection refused"), // down (enabled && !up) + same bucket
	}
	got, err := ListRdps(map[string]map[string]any{"rdps": {"data": items}})
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
	byReason, ok := got["byLastFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byLastFailureReason: wrong type %T", got["byLastFailureReason"])
	}
	if byReason["connection refused"] != 2 || len(byReason) != 1 {
		t.Errorf("byLastFailureReason: got %v, want {connection refused:2}", byReason)
	}
	if _, present := byReason["RDP Shutdown"]; present {
		t.Errorf("admin-disabled RDP's reason must not bucket, got %v", byReason)
	}
}

// TestListRdps_UnexpectedFailureFilter is the load-bearing test for the
// byLastFailureReason design choice: the map reflects UNEXPECTED active
// failures (down && enabled && non-empty reason). Three classes MUST NOT
// appear in the bucket:
//   - admin-disabled RDPs (broker populates "RDP Shutdown" but it's not a
//     failure — the operator turned it off);
//   - recovered RDPs whose lastFailureReason is a stale historical scar;
//   - currently-down RDPs with no reason at all (would otherwise produce a
//     noisy "" key).
func TestListRdps_UnexpectedFailureFilter(t *testing.T) {
	items := []any{
		rdp(false, true, "boom"),          // down + enabled + reason → bucketed
		rdp(false, false, "RDP Shutdown"), // admin-disabled → MUST NOT bucket
		rdp(true, true, "old boom"),       // recovered with historical reason → MUST NOT bucket
		rdp(false, true, ""),              // enabled && !up with empty reason → counted in downCount, NOT bucketed
	}
	got, err := ListRdps(map[string]map[string]any{"rdps": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	// enabled && !up: items 0 and 3. Admin-disabled row (item 1) is excluded
	// from downCount under the unified rule shared with list-vpns.
	if got["downCount"] != 2 {
		t.Errorf("downCount: got %v, want 2", got["downCount"])
	}
	byReason := got["byLastFailureReason"].(map[string]int)
	if len(byReason) != 1 || byReason["boom"] != 1 {
		t.Errorf("byLastFailureReason should contain only {boom:1}, got %v", byReason)
	}
	for _, leaked := range []string{"", "old boom", "RDP Shutdown"} {
		if _, present := byReason[leaked]; present {
			t.Errorf("%q leaked into byLastFailureReason: %v", leaked, byReason)
		}
	}
}

// TestListRdps_DownCountUnifiedWithVpns pins the semantics agreed in
// SOL-151552: downCount is enabled && !up, matching list-vpns.downCount's
// enabled-and-in-the-down-state shape. If the rule drifts, cross-tool
// reasoning breaks silently — hence a dedicated test.
func TestListRdps_DownCountUnifiedWithVpns(t *testing.T) {
	items := []any{
		rdp(false, false, "RDP Shutdown"), // admin-disabled — MUST NOT count as down
		rdp(false, true, "boom"),          // unexpected failure — counts
		rdp(true, true, ""),               // healthy — does not count
	}
	got, err := ListRdps(map[string]map[string]any{"rdps": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["downCount"] != 1 {
		t.Errorf("downCount (enabled && !up): got %v, want 1", got["downCount"])
	}
}

func TestListRdps_TruncationSurfaced(t *testing.T) {
	items := []any{rdp(true, true, ""), rdp(false, true, "x")}
	t.Run("truncated", func(t *testing.T) {
		got, err := ListRdps(map[string]map[string]any{
			"rdps": {"data": items, "truncated": true},
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
		got, err := ListRdps(map[string]map[string]any{
			"rdps": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

// TestListRdps_Empty pins the AC's shape for an empty RDP list: all counts
// zero, byLastFailureReason an empty (not absent, not nil) map, no skipped or
// truncated keys.
func TestListRdps_Empty(t *testing.T) {
	got, err := ListRdps(map[string]map[string]any{"rdps": {"data": []any{}}})
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
	byReason, ok := got["byLastFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byLastFailureReason: want map[string]int, got %T", got["byLastFailureReason"])
	}
	if len(byReason) != 0 {
		t.Errorf("byLastFailureReason: want empty map, got %v", byReason)
	}
	for _, k := range []string{"skipped", "truncated"} {
		if _, present := got[k]; present {
			t.Errorf("%s should be omitted on empty input, got %v", k, got[k])
		}
	}
}

func TestListRdps_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "rdps" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"rdps": {"data": "oops"}},
			wantSub: "rdps.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"rdps": {"data": []any{"not-an-object"}}},
			wantSub: "rdps.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListRdps(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestListRdps_MissingField asserts that omitting any one required field
// skips that RDP rather than aborting — mirrors listQueues' robustness, so one
// malformed row doesn't drop the raw list of every other RDP. Table-driven
// over each field so reordering the handler's reads doesn't break the test
// for the wrong reason.
func TestListRdps_MissingField(t *testing.T) {
	full := rdp(false, true, "boom")
	if _, err := ListRdps(map[string]map[string]any{"rdps": {"data": []any{full}}}); err != nil {
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
			// Healthy row lands in every counter we assert: down (enabled && !up)
			// + bucketed (down && enabled && reason). The skipped row must not
			// steal any of those signals.
			healthy := rdp(false, true, "other")
			got, err := ListRdps(map[string]map[string]any{"rdps": {"data": []any{item, healthy}}})
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
			byReason := got["byLastFailureReason"].(map[string]int)
			if byReason["other"] != 1 {
				t.Errorf("healthy row failed to bucket reason: %v", byReason)
			}
		})
	}
}

// TestListRdps_NilField asserts that an explicit nil (what SEMP returns for an
// unset field) is treated as a skip, not a hard fail.
func TestListRdps_NilField(t *testing.T) {
	bad := rdp(false, true, "boom")
	bad["up"] = nil
	healthy := rdp(false, true, "x")
	got, err := ListRdps(map[string]map[string]any{"rdps": {"data": []any{bad, healthy}}})
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

// TestListRdps_SkippedOmittedWhenZero keeps the summary key set minimal in the
// common case — skipped is noise when nothing was skipped.
func TestListRdps_SkippedOmittedWhenZero(t *testing.T) {
	got, err := ListRdps(map[string]map[string]any{
		"rdps": {"data": []any{rdp(true, true, "")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}
