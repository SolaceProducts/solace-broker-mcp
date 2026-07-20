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

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
)

// vpn is a tiny helper to build a VPN item with the three required fields.
func vpn(enabled bool, state string, conns float64) map[string]any {
	return map[string]any{
		"enabled":           enabled,
		"state":             state,
		"msgVpnConnections": conns,
	}
}

func TestListVpns_Counts(t *testing.T) {
	items := []any{
		vpn(true, "up", 5),      // healthy
		vpn(true, "up", 1),      // zeroConn — only #client attached
		vpn(true, "up", 0),      // zeroConn — underflow safety (no #client)
		vpn(true, "down", 0),    // down
		vpn(true, "standby", 0), // standby (NOT zeroConn — standby excluded)
		vpn(false, "up", 0),     // disabled (NOT zeroConn — disabled excluded)
		vpn(false, "down", 0),   // disabled only (NOT down — down excludes disabled)
	}
	got, err := ListVpns(map[string]map[string]any{
		"vpns": {"data": items},
	})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int{
		"disabledCount":       2,
		"downCount":           1,
		"standbyCount":        1,
		"zeroConnectionCount": 2,
	}
	for k, want := range checks {
		if got[k] != want {
			t.Errorf("%s: got %v, want %d", k, got[k], want)
		}
	}
	if got["scanned"] != len(items) {
		t.Errorf("scanned: got %v, want %d", got["scanned"], len(items))
	}
}

// TestListVpns_ZeroConnectionExclusions asserts that zeroConnectionCount
// counts ONLY enabled+up VPNs — an HA-standby broker (every VPN standby)
// should report zeroConnectionCount: 0, not a fleet-wide false alarm.
// A bare enabled+up VPN reports msgVpnConnections==1 (reserved `#client`
// broker invariant), so the mix case uses conns==1 as the zero-conn marker.
func TestListVpns_ZeroConnectionExclusions(t *testing.T) {
	cases := []struct {
		name  string
		items []any
		want  int
	}{
		{
			name:  "all standby (HA standby broker)",
			items: []any{vpn(true, "standby", 1), vpn(true, "standby", 1)},
			want:  0,
		},
		{
			name:  "all disabled",
			items: []any{vpn(false, "up", 0), vpn(false, "up", 0)},
			want:  0,
		},
		{
			name:  "all down",
			items: []any{vpn(true, "down", 0), vpn(true, "down", 0)},
			want:  0,
		},
		{
			name:  "mix — only enabled+up+(<=1 conn) counts",
			items: []any{vpn(true, "up", 1), vpn(true, "up", 2), vpn(true, "standby", 1), vpn(false, "up", 0)},
			want:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ListVpns(map[string]map[string]any{"vpns": {"data": tc.items}})
			if err != nil {
				t.Fatal(err)
			}
			if got["zeroConnectionCount"] != tc.want {
				t.Errorf("zeroConnectionCount: got %v, want %d", got["zeroConnectionCount"], tc.want)
			}
		})
	}
}

// TestListVpns_DisabledExcludedFromStateCounts asserts down/standby/zeroConn
// are all gated on enabled==true, so a disabled VPN (SEMP typically reports
// state=="down" for these) lands in disabledCount only. This keeps the four
// counts independently readable — the LLM shouldn't have to subtract overlaps
// to answer "how many VPNs are actually broken?".
func TestListVpns_DisabledExcludedFromStateCounts(t *testing.T) {
	items := []any{
		vpn(false, "down", 0),
		vpn(false, "standby", 0),
		vpn(false, "up", 0),
	}
	got, err := ListVpns(map[string]map[string]any{"vpns": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["disabledCount"] != 3 {
		t.Errorf("disabledCount: got %v, want 3", got["disabledCount"])
	}
	for _, k := range []string{"downCount", "standbyCount", "zeroConnectionCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0 (disabled must not contribute)", k, got[k])
		}
	}
}

func TestListVpns_AllUp(t *testing.T) {
	items := []any{vpn(true, "up", 3), vpn(true, "up", 7)}
	got, err := ListVpns(map[string]map[string]any{"vpns": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"disabledCount", "downCount", "standbyCount", "zeroConnectionCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
}

func TestListVpns_AllDown(t *testing.T) {
	items := []any{vpn(true, "down", 0), vpn(true, "down", 0), vpn(true, "down", 0)}
	got, err := ListVpns(map[string]map[string]any{"vpns": {"data": items}})
	if err != nil {
		t.Fatal(err)
	}
	if got["downCount"] != 3 {
		t.Errorf("downCount: got %v, want 3", got["downCount"])
	}
}

// TestListVpns_TruncationSurfaced mirrors the listQueues test: when the
// paginator marks the step truncated, the summary must expose scanned and
// truncated: true alongside the counts. Otherwise the LLM sees authoritative
// looking counts and the truncation flag buried on a sibling object, and
// tends to treat the counts as global rather than over the visible page.
func TestListVpns_TruncationSurfaced(t *testing.T) {
	items := []any{vpn(true, "up", 5), vpn(true, "down", 0)}
	t.Run("truncated", func(t *testing.T) {
		got, err := ListVpns(map[string]map[string]any{
			"vpns": {"data": items, "truncated": true},
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
		got, err := ListVpns(map[string]map[string]any{
			"vpns": {"data": items, "truncated": false},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

func TestListVpns_Empty(t *testing.T) {
	got, err := ListVpns(map[string]map[string]any{"vpns": {"data": []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"disabledCount", "downCount", "standbyCount", "zeroConnectionCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
	if got["scanned"] != 0 {
		t.Errorf("scanned: got %v, want 0", got["scanned"])
	}
}

func TestListVpns_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   map[string]map[string]any
		wantSub string
	}{
		{
			name:    "missing step",
			input:   map[string]map[string]any{},
			wantSub: `step "vpns" not in results`,
		},
		{
			name:    "data wrong type",
			input:   map[string]map[string]any{"vpns": {"data": "oops"}},
			wantSub: "vpns.data: want []any",
		},
		{
			name:    "item wrong type",
			input:   map[string]map[string]any{"vpns": {"data": []any{"not-an-object"}}},
			wantSub: "vpns.data[0]: want object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ListVpns(tc.input)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("got %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// TestListVpns_MissingField asserts that omitting any one required field
// skips that VPN rather than aborting the call. Table-driven over each
// field so reordering the handler's reads doesn't break the test for the
// wrong reason. Hard-failing on one bad row would drop the raw list too —
// a step down from the collect strategy.
func TestListVpns_MissingField(t *testing.T) {
	full := vpn(true, "down", 0)
	if _, err := ListVpns(map[string]map[string]any{"vpns": {"data": []any{full}}}); err != nil {
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
			// Pair the broken row with a healthy signal-emitting row (standby)
			// distinct from the baseline's down signal, so we can also assert
			// the healthy row's signal still lands.
			healthy := vpn(true, "standby", 0)
			got, err := ListVpns(map[string]map[string]any{"vpns": {"data": []any{item, healthy}}})
			if err != nil {
				t.Fatalf("expected skip on missing %q, got error %v", field, err)
			}
			if got["skipped"] != 1 {
				t.Errorf("skipped: got %v, want 1", got["skipped"])
			}
			if got["scanned"] != 2 {
				t.Errorf("scanned: got %v, want 2", got["scanned"])
			}
			if got["standbyCount"] != 1 {
				t.Errorf("standbyCount: got %v, want 1 (healthy row should still count)", got["standbyCount"])
			}
		})
	}
}

// TestListVpns_NilField asserts that an explicit nil (what SEMP returns for
// an unset field) is treated as a skip, not a hard fail. One VPN with a nil
// msgVpnConnections shouldn't lose the raw list of every other VPN.
func TestListVpns_NilField(t *testing.T) {
	bad := vpn(true, "up", 0)
	bad["msgVpnConnections"] = nil
	healthy := vpn(true, "down", 0)
	got, err := ListVpns(map[string]map[string]any{"vpns": {"data": []any{bad, healthy}}})
	if err != nil {
		t.Fatalf("nil field should skip, got %v", err)
	}
	if got["skipped"] != 1 {
		t.Errorf("skipped: got %v, want 1", got["skipped"])
	}
	if got["downCount"] != 1 {
		t.Errorf("healthy row signal lost: %+v", got)
	}
}

// TestListVpns_SkippedOmittedWhenZero keeps the summary key set minimal in
// the common case — skipped is noise when nothing was skipped.
func TestListVpns_SkippedOmittedWhenZero(t *testing.T) {
	got, err := ListVpns(map[string]map[string]any{
		"vpns": {"data": []any{vpn(true, "up", 1)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}

// TestListVpns_ValidateTool exercises the boot-time cross-check for the
// listVpns handler specifically. Ensures the registered RequiredFields match
// what's needed so the server refuses to start if a future YAML edit drops
// enabled/state/msgVpnConnections from the vpns step's select.
func TestListVpns_ValidateTool(t *testing.T) {
	t.Run("select covers required fields", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"vpns"},
			map[string][]string{"vpns": {"enabled", "state", "msgVpnConnections", "msgVpnName"}})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})
	t.Run("select missing state", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"vpns"},
			map[string][]string{"vpns": {"enabled", "msgVpnConnections"}})
		if err == nil || !strings.Contains(err.Error(), `"state"`) {
			t.Fatalf("expected error mentioning state, got %v", err)
		}
	})
	t.Run("step id renamed", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"messageVpns"},
			map[string][]string{"messageVpns": {"enabled", "state", "msgVpnConnections"}})
		if err == nil || !strings.Contains(err.Error(), `"vpns"`) {
			t.Fatalf("expected error mentioning vpns step, got %v", err)
		}
	})
}
