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
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

// vpn builds a VPN row with the three fields the handler reads. Older tests
// carried msgVpnConnections here — dropped now that zeroConnectionCount comes
// from the real-clients fan-out, not from the connection count.
func vpn(enabled bool, state, name string) map[string]any {
	return map[string]any{
		"enabled":    enabled,
		"state":      state,
		"msgVpnName": name,
	}
}

// clients builds a real-clients fan-out result keyed by VPN name. Each entry
// carries {data: [rows...]} with no paging metadata, i.e. a probe the broker
// ran to exhaustion. Pass an empty slice for "VPN was probed but had no real
// clients" (the zero-conn signal). Omit a VPN entirely for "forEachIf skipped
// this row" (disabled VPNs never enter the map).
func clients(byVpn map[string][]any) map[string]map[string]any {
	byKey := make(map[string]any, len(byVpn))
	for name, rows := range byVpn {
		byKey[name] = map[string]any{"data": rows}
	}
	return map[string]map[string]any{"byKey": {"byKey": byKey}}
}

// indeterminateEntry builds the probe result for a VPN whose scan stopped
// early: no matching rows, but a nextPageUri still pointing onward. This is
// what the broker returns when forceFullPage is not honoured, and it must not
// be read as "no real client".
func indeterminateEntry() map[string]any {
	return map[string]any{
		"data": []any{},
		"meta": map[string]any{
			"paging": map[string]any{
				"nextPageUri": "http://broker/SEMP/v2/__private_monitor__/msgVpns/x/clients?cursor=abc",
			},
		},
	}
}

// withEntry overwrites one VPN's probe entry in an assembled input, for the
// cases the clients() shorthand cannot express.
func withEntry(in map[string]map[string]any, vpnName string, entry map[string]any) map[string]map[string]any {
	in["real-clients"]["byKey"].(map[string]any)[vpnName] = entry
	return in
}

// input assembles both step results into the shape ListVpns receives.
func input(items []any, byVpn map[string][]any) map[string]map[string]any {
	res := clients(byVpn)
	res["vpns"] = map[string]any{"data": items}
	// clients() keyed under "byKey" as a workaround for map return typing;
	// remap to the actual step ID here so ListVpns finds it.
	res["real-clients"] = res["byKey"]
	delete(res, "byKey")
	return res
}

// row is a helper for the "there is at least one real client on this VPN" case.
func clientRow() any { return map[string]any{"clientName": "app-1"} }

func TestListVpns_Counts(t *testing.T) {
	items := []any{
		vpn(true, "up", "healthy"),      // healthy (has real client)
		vpn(true, "up", "empty"),        // zeroConn — no real client
		vpn(true, "up", "also-empty"),   // zeroConn — no real client (belt & braces)
		vpn(true, "down", "downvpn"),    // down
		vpn(true, "standby", "sby"),     // standby (NOT zeroConn)
		vpn(false, "up", "disabled1"),   // disabled (NOT zeroConn)
		vpn(false, "down", "disabled2"), // disabled only (NOT down)
	}
	byVpn := map[string][]any{
		"healthy":    {clientRow()},
		"empty":      {},
		"also-empty": {},
		// downvpn / sby not in byKey since forEachIf on enabled==true still ran;
		// the handler's state switch never checks byKey for those states.
		"downvpn": {},
		"sby":     {clientRow()},
	}
	got, err := ListVpns(input(items, byVpn))
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

// TestListVpns_ZeroConnectionExclusions asserts zeroConnectionCount is gated on
// enabled+up. An HA-standby broker (every VPN standby) must report 0, not a
// fleet-wide false alarm.
func TestListVpns_ZeroConnectionExclusions(t *testing.T) {
	cases := []struct {
		name  string
		items []any
		byVpn map[string][]any
		want  int
	}{
		{
			name:  "all standby",
			items: []any{vpn(true, "standby", "a"), vpn(true, "standby", "b")},
			byVpn: map[string][]any{"a": {clientRow()}, "b": {clientRow()}},
			want:  0,
		},
		{
			name:  "all disabled",
			items: []any{vpn(false, "up", "a"), vpn(false, "up", "b")},
			byVpn: map[string][]any{}, // forEachIf filtered them
			want:  0,
		},
		{
			name:  "all down",
			items: []any{vpn(true, "down", "a"), vpn(true, "down", "b")},
			byVpn: map[string][]any{"a": {}, "b": {}},
			want:  0,
		},
		{
			name:  "mix — only enabled+up+no-real-client counts",
			items: []any{vpn(true, "up", "a"), vpn(true, "up", "b"), vpn(true, "standby", "c"), vpn(false, "up", "d")},
			byVpn: map[string][]any{"a": {}, "b": {clientRow()}, "c": {clientRow()}},
			want:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ListVpns(input(tc.items, tc.byVpn))
			if err != nil {
				t.Fatal(err)
			}
			if got["zeroConnectionCount"] != tc.want {
				t.Errorf("zeroConnectionCount: got %v, want %d", got["zeroConnectionCount"], tc.want)
			}
		})
	}
}

// TestListVpns_MissingByKeyEntryIsZeroConn: forEachIf never filters
// enabled+up rows, but if for any reason byKey has no entry for a VPN we
// should treat it as "no real client" — not crash and not silently miss
// the signal. Belt-and-braces against future YAML tweaks.
func TestListVpns_MissingByKeyEntryIsZeroConn(t *testing.T) {
	items := []any{vpn(true, "up", "orphan")}
	got, err := ListVpns(input(items, map[string][]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if got["zeroConnectionCount"] != 1 {
		t.Errorf("missing byKey entry should count as zero-conn, got %v", got["zeroConnectionCount"])
	}
}

func TestListVpns_DisabledExcludedFromStateCounts(t *testing.T) {
	items := []any{
		vpn(false, "down", "a"),
		vpn(false, "standby", "b"),
		vpn(false, "up", "c"),
	}
	got, err := ListVpns(input(items, map[string][]any{}))
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
	items := []any{vpn(true, "up", "a"), vpn(true, "up", "b")}
	byVpn := map[string][]any{"a": {clientRow()}, "b": {clientRow()}}
	got, err := ListVpns(input(items, byVpn))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"disabledCount", "downCount", "standbyCount", "zeroConnectionCount"} {
		if got[k] != 0 {
			t.Errorf("%s: got %v, want 0", k, got[k])
		}
	}
}

func TestListVpns_TruncationSurfaced(t *testing.T) {
	items := []any{vpn(true, "up", "a"), vpn(true, "down", "b")}
	byVpn := map[string][]any{"a": {clientRow()}, "b": {}}
	t.Run("truncated", func(t *testing.T) {
		in := input(items, byVpn)
		in["vpns"]["truncated"] = true
		got, err := ListVpns(in)
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
		got, err := ListVpns(input(items, byVpn))
		if err != nil {
			t.Fatal(err)
		}
		if _, present := got["truncated"]; present {
			t.Errorf("truncated key should be omitted when false, got %v", got["truncated"])
		}
	})
}

func TestListVpns_Empty(t *testing.T) {
	got, err := ListVpns(input([]any{}, map[string][]any{}))
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
			name:    "missing vpns step",
			input:   map[string]map[string]any{"real-clients": {"byKey": map[string]any{}}},
			wantSub: `step "vpns" not in results`,
		},
		{
			name:    "vpns data wrong type",
			input:   map[string]map[string]any{"vpns": {"data": "oops"}, "real-clients": {"byKey": map[string]any{}}},
			wantSub: "vpns.data: want []any",
		},
		{
			name:    "vpns item wrong type",
			input:   map[string]map[string]any{"vpns": {"data": []any{"not-an-object"}}, "real-clients": {"byKey": map[string]any{}}},
			wantSub: "vpns.data[0]: want object",
		},
		{
			name:    "missing real-clients step",
			input:   map[string]map[string]any{"vpns": {"data": []any{}}},
			wantSub: `step "real-clients" not in results`,
		},
		{
			name:    "real-clients byKey wrong type",
			input:   map[string]map[string]any{"vpns": {"data": []any{}}, "real-clients": {"byKey": "oops"}},
			wantSub: "real-clients.byKey: want map",
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

// TestListVpns_MissingField asserts that omitting any one required field on a
// VPN row skips that VPN rather than aborting the call.
func TestListVpns_MissingField(t *testing.T) {
	full := vpn(true, "down", "target")
	if _, err := ListVpns(input([]any{full}, map[string][]any{})); err != nil {
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
			healthy := vpn(true, "standby", "healthy")
			got, err := ListVpns(input([]any{item, healthy}, map[string][]any{"healthy": {clientRow()}}))
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

// TestListVpns_NilField mirrors TestListVpns_MissingField for the case SEMP
// returns an explicit nil rather than omitting the field.
func TestListVpns_NilField(t *testing.T) {
	bad := vpn(true, "up", "bad")
	bad["state"] = nil
	healthy := vpn(true, "down", "healthy")
	got, err := ListVpns(input([]any{bad, healthy}, map[string][]any{}))
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

func TestListVpns_SkippedOmittedWhenZero(t *testing.T) {
	items := []any{vpn(true, "up", "a")}
	got, err := ListVpns(input(items, map[string][]any{"a": {clientRow()}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, present := got["skipped"]; present {
		t.Errorf("skipped key should be omitted when 0, got %v", got["skipped"])
	}
}

// TestListVpns_IndeterminateProbeIsNotZeroConn is the guard that keeps an
// unhonoured forceFullPage from silently reproducing SOL-153071. A probe that
// returned no rows but still advertises a next page established nothing, so the
// VPN must land in indeterminateConnectionCount — never in zeroConnectionCount,
// which an operator may act on by decommissioning the VPN.
func TestListVpns_IndeterminateProbeIsNotZeroConn(t *testing.T) {
	items := []any{
		vpn(true, "up", "unverified"),
		vpn(true, "up", "genuinely-empty"),
		vpn(true, "up", "has-client"),
	}
	in := input(items, map[string][]any{
		"genuinely-empty": {},
		"has-client":      {clientRow()},
	})
	in = withEntry(in, "unverified", indeterminateEntry())

	got, err := ListVpns(in)
	if err != nil {
		t.Fatal(err)
	}
	if got["zeroConnectionCount"] != 1 {
		t.Errorf("zeroConnectionCount: got %v, want 1 (only genuinely-empty)", got["zeroConnectionCount"])
	}
	if got["indeterminateConnectionCount"] != 1 {
		t.Errorf("indeterminateConnectionCount: got %v, want 1", got["indeterminateConnectionCount"])
	}
	byKey := in["real-clients"]["byKey"].(map[string]any)
	// The indeterminate entry must not expose hasRealClient at all: a consumer
	// testing `hasRealClient == false` would otherwise treat it as idle.
	entry := byKey["unverified"].(map[string]any)
	if _, present := entry["hasRealClient"]; present {
		t.Errorf("indeterminate entry must not carry hasRealClient, got %#v", entry)
	}
	if entry["indeterminate"] != true {
		t.Errorf("byKey[unverified]: want {indeterminate:true}, got %#v", entry)
	}
}

// TestListVpns_IndeterminateProbeWarns asserts the degradation is not silent.
// The summary field alone is insufficient — an MCP client may never surface it
// to a human — so an indeterminate probe must also reach the operator's logs.
// A test that only checked the count would leave that guarantee unverified,
// which is the same gap this whole change set exists to close.
func TestListVpns_IndeterminateProbeWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	in := input([]any{vpn(true, "up", "unverified")}, map[string][]any{})
	in = withEntry(in, "unverified", indeterminateEntry())
	if _, err := ListVpns(in); err != nil {
		t.Fatal(err)
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("indeterminate probe produced no log output; the degradation would be silent")
	}
	for _, want := range []string{
		"level=WARN",
		"indeterminate_vpns=1",
		"unverified",
		"forceFullPage",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("warning missing %q\ngot: %s", want, logged)
		}
	}
}

// TestListVpns_NoWarningWhenAllProbesComplete is the companion: a healthy run
// must stay quiet, or the warning becomes noise operators learn to ignore.
func TestListVpns_NoWarningWhenAllProbesComplete(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	items := []any{vpn(true, "up", "has-client"), vpn(true, "up", "empty")}
	if _, err := ListVpns(input(items, map[string][]any{
		"has-client": {clientRow()},
		"empty":      {},
	})); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "" {
		t.Errorf("healthy run must not warn, got: %s", got)
	}
}

// TestListVpns_ExhaustedEmptyProbeIsZeroConn is the other half of the pair: an
// empty probe with NO next page means the broker scanned the whole collection
// and found nothing, which is a definitive zero-connection answer.
func TestListVpns_ExhaustedEmptyProbeIsZeroConn(t *testing.T) {
	got, err := ListVpns(input([]any{vpn(true, "up", "empty")}, map[string][]any{"empty": {}}))
	if err != nil {
		t.Fatal(err)
	}
	if got["zeroConnectionCount"] != 1 {
		t.Errorf("zeroConnectionCount: got %v, want 1", got["zeroConnectionCount"])
	}
	if _, present := got["indeterminateConnectionCount"]; present {
		t.Errorf("indeterminateConnectionCount must be omitted when zero, got %v", got["indeterminateConnectionCount"])
	}
}

// TestListVpns_IndeterminateIgnoredWhenRowsPresent: a probe that returned a
// match is definitive even if a next page remains — the broker stops as soon
// as the page holds `count` matches, so a trailing cursor is normal and must
// not downgrade a positive result.
func TestListVpns_IndeterminateIgnoredWhenRowsPresent(t *testing.T) {
	entry := indeterminateEntry()
	entry["data"] = []any{clientRow()}
	in := withEntry(input([]any{vpn(true, "up", "has-client")}, map[string][]any{}), "has-client", entry)
	got, err := ListVpns(in)
	if err != nil {
		t.Fatal(err)
	}
	if got["zeroConnectionCount"] != 0 {
		t.Errorf("zeroConnectionCount: got %v, want 0", got["zeroConnectionCount"])
	}
	if _, present := got["indeterminateConnectionCount"]; present {
		t.Errorf("a matched probe must not be indeterminate, got %v", got["indeterminateConnectionCount"])
	}
	entryOut := in["real-clients"]["byKey"].(map[string]any)["has-client"].(map[string]any)
	if entryOut["hasRealClient"] != true {
		t.Errorf("byKey[has-client]: want {hasRealClient:true}, got %#v", entryOut)
	}
}

// TestListVpns_ScrubsRawClientRows locks in the payload-leak fix: after
// ListVpns returns, real-clients.byKey must hold exactly one verdict shape for
// every VPN it probed — {hasRealClient: true}, {hasRealClient: false}, or
// {indeterminate: true} — never the raw client rows or paging metadata the
// probe fetched. stepResults is shared with the executor's response assembly
// (no copy is made after this handler returns), so anything left in byKey
// here is returned to the MCP caller verbatim.
func TestListVpns_ScrubsRawClientRows(t *testing.T) {
	items := []any{
		vpn(true, "up", "healthy"),
		vpn(true, "up", "empty"),
		vpn(true, "up", "degraded"),
	}
	byVpn := map[string][]any{
		"healthy":  {clientRow(), clientRow()},
		"empty":    {},
		"degraded": {},
	}
	in := withEntry(input(items, byVpn), "degraded", indeterminateEntry())
	if _, err := ListVpns(in); err != nil {
		t.Fatal(err)
	}
	byKey, ok := in["real-clients"]["byKey"].(map[string]any)
	if !ok {
		t.Fatalf("real-clients.byKey missing or wrong type after ListVpns: %#v", in["real-clients"])
	}
	cases := map[string]map[string]any{
		"healthy":  {"hasRealClient": true},
		"empty":    {"hasRealClient": false},
		"degraded": {"indeterminate": true},
	}
	for vpnName, want := range cases {
		entry, ok := byKey[vpnName].(map[string]any)
		if !ok {
			t.Fatalf("byKey[%q]: want map[string]any, got %#v", vpnName, byKey[vpnName])
		}
		for _, raw := range []string{"data", "meta"} {
			if _, leaked := entry[raw]; leaked {
				t.Errorf("byKey[%q] still has raw %q after ListVpns: %#v", vpnName, raw, entry)
			}
		}
		if len(entry) != len(want) {
			t.Errorf("byKey[%q]: want exactly %#v, got %#v", vpnName, want, entry)
		}
		for k, v := range want {
			if entry[k] != v {
				t.Errorf("byKey[%q][%q]: got %v, want %v", vpnName, k, entry[k], v)
			}
		}
	}
}

// TestListVpns_ValidateTool exercises the boot-time cross-check for the
// listVpns handler. Ensures RequiredFieldsPerStep matches the YAML selects and
// that renaming either step (vpns / real-clients) is caught at boot.
func TestListVpns_ValidateTool(t *testing.T) {
	t.Run("selects cover required fields", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"vpns", "real-clients"},
			map[string][]string{
				"vpns":         {"enabled", "state", "msgVpnName"},
				"real-clients": {"clientName"},
			})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})
	t.Run("vpns select missing state", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"vpns", "real-clients"},
			map[string][]string{
				"vpns":         {"enabled", "msgVpnName"},
				"real-clients": {"clientName"},
			})
		if err == nil || !strings.Contains(err.Error(), `"state"`) {
			t.Fatalf("expected error mentioning state, got %v", err)
		}
	})
	t.Run("real-clients select missing clientName", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"vpns", "real-clients"},
			map[string][]string{
				"vpns":         {"enabled", "state", "msgVpnName"},
				"real-clients": {},
			})
		if err == nil || !strings.Contains(err.Error(), `"clientName"`) {
			t.Fatalf("expected error mentioning clientName, got %v", err)
		}
	})
	t.Run("vpns step renamed", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"messageVpns", "real-clients"},
			map[string][]string{
				"messageVpns":  {"enabled", "state", "msgVpnName"},
				"real-clients": {"clientName"},
			})
		if err == nil || !strings.Contains(err.Error(), `"vpns"`) {
			t.Fatalf("expected error mentioning vpns step, got %v", err)
		}
	})
	t.Run("real-clients step renamed", func(t *testing.T) {
		err := postprocess.ValidateTool("list-vpns", "listVpns",
			[]string{"vpns", "clients"},
			map[string][]string{
				"vpns":    {"enabled", "state", "msgVpnName"},
				"clients": {"clientName"},
			})
		if err == nil || !strings.Contains(err.Error(), `"real-clients"`) {
			t.Fatalf("expected error mentioning real-clients step, got %v", err)
		}
	})
}
