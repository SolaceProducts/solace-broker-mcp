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
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// sub builds a slow-subscriber item with the three required fields.
func sub(profile, platform string, discarded any) map[string]any {
	return map[string]any{
		"clientProfileName":   profile,
		"platform":            platform,
		"txDiscardedMsgCount": discarded,
	}
}

func TestListSlowSubscribers_Empty(t *testing.T) {
	got, err := ListSlowSubscribers(map[string]map[string]any{
		"slowSubscribers": {"data": []any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["scanned"] != 0 {
		t.Errorf("scanned: got %v, want 0", got["scanned"])
	}
	if got["totalTxDiscardedMsgCount"] != float64(0) {
		t.Errorf("totalTxDiscardedMsgCount: got %v, want 0", got["totalTxDiscardedMsgCount"])
	}
	profiles, ok := got["byClientProfile"].(map[string]int)
	if !ok || len(profiles) != 0 {
		t.Errorf("byClientProfile: got %v, want empty map", got["byClientProfile"])
	}
	platforms, ok := got["byPlatform"].(map[string]int)
	if !ok || len(platforms) != 0 {
		t.Errorf("byPlatform: got %v, want empty map", got["byPlatform"])
	}
	for _, k := range []string{"skipped", "truncated"} {
		if _, present := got[k]; present {
			t.Errorf("%s should be omitted, got %v", k, got[k])
		}
	}
}

func TestListSlowSubscribers_AllSameProfile(t *testing.T) {
	items := []any{
		sub("default", "linux", float64(5)),
		sub("default", "linux", float64(10)),
		sub("default", "windows", float64(0)),
	}
	got, err := ListSlowSubscribers(map[string]map[string]any{
		"slowSubscribers": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := got["byClientProfile"].(map[string]int)
	if len(profiles) != 1 || profiles["default"] != 3 {
		t.Errorf("byClientProfile: got %v, want {default:3}", profiles)
	}
	platforms := got["byPlatform"].(map[string]int)
	if platforms["linux"] != 2 || platforms["windows"] != 1 {
		t.Errorf("byPlatform: got %v, want {linux:2, windows:1}", platforms)
	}
	if got["totalTxDiscardedMsgCount"] != float64(15) {
		t.Errorf("totalTxDiscardedMsgCount: got %v, want 15", got["totalTxDiscardedMsgCount"])
	}
	if got["scanned"] != 3 {
		t.Errorf("scanned: got %v, want 3", got["scanned"])
	}
}

func TestListSlowSubscribers_MixedProfiles(t *testing.T) {
	items := []any{
		sub("default", "linux", float64(1)),
		sub("high-throughput", "linux", float64(2)),
		sub("default", "macos", float64(4)),
		sub("high-throughput", "windows", float64(8)),
	}
	got, err := ListSlowSubscribers(map[string]map[string]any{
		"slowSubscribers": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := got["byClientProfile"].(map[string]int)
	if profiles["default"] != 2 || profiles["high-throughput"] != 2 {
		t.Errorf("byClientProfile: got %v", profiles)
	}
	platforms := got["byPlatform"].(map[string]int)
	if platforms["linux"] != 2 || platforms["macos"] != 1 || platforms["windows"] != 1 {
		t.Errorf("byPlatform: got %v", platforms)
	}
	if got["totalTxDiscardedMsgCount"] != float64(15) {
		t.Errorf("totalTxDiscardedMsgCount: got %v, want 15", got["totalTxDiscardedMsgCount"])
	}
}

// TestListSlowSubscribers_NumericShapes asserts that totalTxDiscardedMsgCount
// sums across the numeric shapes JSON decoders produce — float64 (default),
// json.Number (UseNumber decoder), int/int64 (custom unmarshalers). This is
// the same insulation numField provides for listQueues; a future SEMP-client
// decode-mode change must not silently drop discard counts.
func TestListSlowSubscribers_NumericShapes(t *testing.T) {
	items := []any{
		sub("p", "linux", float64(1.5)),
		sub("p", "linux", json.Number("2")),
		sub("p", "linux", int(3)),
		sub("p", "linux", int64(4)),
	}
	got, err := ListSlowSubscribers(map[string]map[string]any{
		"slowSubscribers": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["totalTxDiscardedMsgCount"] != float64(10.5) {
		t.Errorf("totalTxDiscardedMsgCount: got %v, want 10.5", got["totalTxDiscardedMsgCount"])
	}
	if got["scanned"] != 4 {
		t.Errorf("scanned: got %v, want 4", got["scanned"])
	}
}

// TestListSlowSubscribers_LargeSum guards against silent overflow at counts a
// production broker can plausibly report: txDiscardedMsgCount is a 64-bit
// counter, and 100 slow clients each with 2^53 discards would already exceed
// float64's exact-integer range. The handler documents float64 accumulation;
// this test freezes that behavior so a switch to int64 accumulation later is
// a deliberate change, not an accidental one.
func TestListSlowSubscribers_LargeSum(t *testing.T) {
	big := float64(1 << 53) // largest exact-integer float64
	items := []any{
		sub("p", "linux", big),
		sub("p", "linux", big),
	}
	got, err := ListSlowSubscribers(map[string]map[string]any{
		"slowSubscribers": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := big * 2
	if got["totalTxDiscardedMsgCount"] != want {
		t.Errorf("totalTxDiscardedMsgCount: got %v, want %v", got["totalTxDiscardedMsgCount"], want)
	}
	if math.IsInf(want, 0) {
		t.Fatal("test assumption failed: 2*2^53 should be finite")
	}
}

// TestListSlowSubscribers_MissingField asserts that omitting any one required
// field skips that row rather than aborting. Table-driven over each field so
// reordering the handler's reads doesn't break the test for the wrong reason.
func TestListSlowSubscribers_MissingField(t *testing.T) {
	full := sub("default", "linux", float64(7))
	if _, err := ListSlowSubscribers(map[string]map[string]any{"slowSubscribers": {"data": []any{full}}}); err != nil {
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
			// healthy row's signal still lands.
			healthy := sub("high-throughput", "windows", float64(3))
			got, err := ListSlowSubscribers(map[string]map[string]any{"slowSubscribers": {"data": []any{item, healthy}}})
			if err != nil {
				t.Fatalf("expected skip on missing %q, got error %v", field, err)
			}
			if got["skipped"] != 1 {
				t.Errorf("skipped: got %v, want 1", got["skipped"])
			}
			if got["scanned"] != 2 {
				t.Errorf("scanned: got %v, want 2", got["scanned"])
			}
			profiles := got["byClientProfile"].(map[string]int)
			if profiles["high-throughput"] != 1 {
				t.Errorf("healthy row lost from byClientProfile: %v", profiles)
			}
			platforms := got["byPlatform"].(map[string]int)
			if platforms["windows"] != 1 {
				t.Errorf("healthy row lost from byPlatform: %v", platforms)
			}
			if got["totalTxDiscardedMsgCount"] != float64(3) {
				t.Errorf("totalTxDiscardedMsgCount: got %v, want 3", got["totalTxDiscardedMsgCount"])
			}
		})
	}
}

// TestListSlowSubscribers_WrongType asserts a wrong-typed field skips the row
// rather than aborting — same rationale as MissingField.
func TestListSlowSubscribers_WrongType(t *testing.T) {
	bad := sub("default", "linux", "not-a-number")
	healthy := sub("high-throughput", "windows", float64(9))
	got, err := ListSlowSubscribers(map[string]map[string]any{"slowSubscribers": {"data": []any{bad, healthy}}})
	if err != nil {
		t.Fatalf("wrong-type field should skip, got %v", err)
	}
	if got["skipped"] != 1 {
		t.Errorf("skipped: got %v, want 1", got["skipped"])
	}
	if got["totalTxDiscardedMsgCount"] != float64(9) {
		t.Errorf("totalTxDiscardedMsgCount: got %v, want 9", got["totalTxDiscardedMsgCount"])
	}
}

// TestListSlowSubscribers_TruncationSurfaced asserts that when the paginator
// marks the step truncated, the summary exposes scanned and truncated: true.
func TestListSlowSubscribers_TruncationSurfaced(t *testing.T) {
	items := []any{sub("default", "linux", float64(1)), sub("default", "linux", float64(2))}
	t.Run("truncated", func(t *testing.T) {
		got, err := ListSlowSubscribers(map[string]map[string]any{
			"slowSubscribers": {"data": items, "truncated": true},
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
		got, err := ListSlowSubscribers(map[string]map[string]any{
			"slowSubscribers": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

// TestListSlowSubscribers_SkippedOmittedWhenZero keeps the summary key set
// minimal in the common case — skipped is noise when nothing was skipped.
func TestListSlowSubscribers_SkippedOmittedWhenZero(t *testing.T) {
	got, err := ListSlowSubscribers(map[string]map[string]any{
		"slowSubscribers": {"data": []any{sub("default", "linux", float64(1))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}

func TestListSlowSubscribers_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "slowSubscribers" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"slowSubscribers": {"data": "oops"}},
			wantSub: "slowSubscribers.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"slowSubscribers": {"data": []any{"not-an-object"}}},
			wantSub: "slowSubscribers.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListSlowSubscribers(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}
