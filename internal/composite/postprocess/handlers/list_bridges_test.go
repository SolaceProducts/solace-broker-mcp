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

// bridge builds a bridge item with the four required fields.
func bridge(enabled bool, inboundState, outboundState, inboundFailureReason string) map[string]any {
	return map[string]any{
		"enabled":              enabled,
		"inboundState":         inboundState,
		"outboundState":        outboundState,
		"inboundFailureReason": inboundFailureReason,
	}
}

func TestListBridges_Counts(t *testing.T) {
	items := []any{
		bridge(true, "ready-in-sync", "ready", ""),                         // healthy
		bridge(true, "not-ready-wait-next", "ready", "connection refused"), // down (inbound) + bucketed
		bridge(false, "disabled", "not-applicable", "Bridge Shutdown"),     // disabled — NOT down, NOT bucketed
		bridge(true, "ready-subscribing", "not-applicable", ""),            // healthy (unidirectional, outbound n/a)
		bridge(true, "not-ready-wait-next", "ready", "connection refused"), // down + same bucket
	}
	got, err := ListBridges(map[string]map[string]any{"bridges": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{
		"downCount":     2,
		"disabledCount": 1,
		"scanned":       5,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
	byReason, ok := got["byInboundFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byInboundFailureReason: wrong type %T", got["byInboundFailureReason"])
	}
	if byReason["connection refused"] != 2 || len(byReason) != 1 {
		t.Errorf("byInboundFailureReason: got %v, want {connection refused:2}", byReason)
	}
	if _, present := byReason["Bridge Shutdown"]; present {
		t.Errorf("admin-disabled bridge's reason must not bucket, got %v", byReason)
	}
}

// TestListBridges_OutboundOnlyFailure pins that an outbound-only failure
// (inbound healthy, outbound not) still counts as down even though SEMP has no
// outboundFailureReason field to bucket it under.
func TestListBridges_OutboundOnlyFailure(t *testing.T) {
	// inbound healthy, outbound neither "ready" nor "not-applicable".
	items := []any{
		bridge(true, "ready-in-sync", "unknown-transitional-state", ""),
	}
	got, err := ListBridges(map[string]map[string]any{"bridges": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["downCount"] != 1 {
		t.Errorf("downCount: got %v, want 1 (outbound-only failure must still count as down)", got["downCount"])
	}
	byReason := got["byInboundFailureReason"].(map[string]int)
	if len(byReason) != 0 {
		t.Errorf("byInboundFailureReason: want empty (no inbound reason field for outbound failures), got %v", byReason)
	}
}

// TestListBridges_OutboundOnlyFailureWithStaleInboundReason is the regression
// test for the bucketing bug flagged in review (aross): SEMP does not clear
// inboundFailureReason when inboundState recovers, so an outbound-only
// failure can coexist with a non-empty, stale inboundFailureReason left over
// from a since-recovered inbound issue. The bridge is down (outbound
// unhealthy), but byInboundFailureReason must stay empty — bucketing here
// would misreport an active inbound failure that isn't actually happening.
func TestListBridges_OutboundOnlyFailureWithStaleInboundReason(t *testing.T) {
	items := []any{
		// inbound healthy but carries a stale reason from a past failure;
		// outbound is the one that's actually down.
		bridge(true, "ready-in-sync", "unknown-transitional-state", "stale connection refused"),
	}
	got, err := ListBridges(map[string]map[string]any{"bridges": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["downCount"] != 1 {
		t.Errorf("downCount: got %v, want 1 (outbound-only failure must still count as down)", got["downCount"])
	}
	byReason := got["byInboundFailureReason"].(map[string]int)
	if len(byReason) != 0 {
		t.Errorf("byInboundFailureReason: want empty (inbound is healthy; the stale reason must not bucket), got %v", byReason)
	}
}

// TestListBridges_UnidirectionalNotApplicableIsHealthy pins that
// "not-applicable" on either direction means that direction doesn't apply to
// this bridge and must not be treated as down.
func TestListBridges_UnidirectionalNotApplicableIsHealthy(t *testing.T) {
	items := []any{
		bridge(true, "not-applicable", "ready", ""),         // inbound n/a, outbound up — healthy
		bridge(true, "ready-in-sync", "not-applicable", ""), // outbound n/a, inbound up — healthy
	}
	got, err := ListBridges(map[string]map[string]any{"bridges": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["downCount"] != 0 {
		t.Errorf("downCount: got %v, want 0", got["downCount"])
	}
}

func TestListBridges_TruncationSurfaced(t *testing.T) {
	items := []any{bridge(true, "ready-in-sync", "ready", ""), bridge(true, "stalled", "ready", "x")}
	t.Run("truncated", func(t *testing.T) {
		got, err := ListBridges(map[string]map[string]any{
			"bridges": {"data": items, "truncated": true},
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
		got, err := ListBridges(map[string]map[string]any{
			"bridges": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

// TestListBridges_Empty pins the AC's shape for an empty bridge list: all
// counts zero, byInboundFailureReason an empty (not absent, not nil) map, no
// skipped or truncated keys.
func TestListBridges_Empty(t *testing.T) {
	got, err := ListBridges(map[string]map[string]any{"bridges": {"data": []any{}}})
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
	byReason, ok := got["byInboundFailureReason"].(map[string]int)
	if !ok {
		t.Fatalf("byInboundFailureReason: want map[string]int, got %T", got["byInboundFailureReason"])
	}
	if len(byReason) != 0 {
		t.Errorf("byInboundFailureReason: want empty map, got %v", byReason)
	}
	for _, k := range []string{"skipped", "truncated"} {
		if _, present := got[k]; present {
			t.Errorf("%s should be omitted on empty input, got %v", k, got[k])
		}
	}
}

func TestListBridges_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "bridges" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"bridges": {"data": "oops"}},
			wantSub: "bridges.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"bridges": {"data": []any{"not-an-object"}}},
			wantSub: "bridges.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListBridges(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestListBridges_MissingField asserts that omitting any one required field
// skips that bridge rather than aborting — mirrors listRdps' robustness, so
// one malformed row doesn't drop the raw list of every other bridge.
func TestListBridges_MissingField(t *testing.T) {
	full := bridge(true, "stalled", "ready", "boom")
	if _, err := ListBridges(map[string]map[string]any{"bridges": {"data": []any{full}}}); err != nil {
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
			// Healthy-but-down row lands in every counter we assert: down
			// (inbound stalled) + bucketed (down && enabled && reason). The
			// skipped row must not steal any of those signals.
			healthy := bridge(true, "stalled", "ready", "other")
			got, err := ListBridges(map[string]map[string]any{"bridges": {"data": []any{item, healthy}}})
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
			byReason := got["byInboundFailureReason"].(map[string]int)
			if byReason["other"] != 1 {
				t.Errorf("healthy row failed to bucket reason: %v", byReason)
			}
		})
	}
}

// TestListBridges_NilField asserts that an explicit nil (what SEMP returns for
// an unset field) is treated as a skip, not a hard fail.
func TestListBridges_NilField(t *testing.T) {
	bad := bridge(true, "stalled", "ready", "boom")
	bad["enabled"] = nil
	healthy := bridge(true, "stalled", "ready", "x")
	got, err := ListBridges(map[string]map[string]any{"bridges": {"data": []any{bad, healthy}}})
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

// TestListBridges_SkippedOmittedWhenZero keeps the summary key set minimal in
// the common case — skipped is noise when nothing was skipped.
func TestListBridges_SkippedOmittedWhenZero(t *testing.T) {
	got, err := ListBridges(map[string]map[string]any{
		"bridges": {"data": []any{bridge(true, "ready-in-sync", "ready", "")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}
