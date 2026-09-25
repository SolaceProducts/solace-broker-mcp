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

package composite

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// legacyRealClientsPredicate is list-vpns' real-clients forEachIf as it stood
// before SOL-154166 — enabled+up with no connection-count term, so every
// enabled+up VPN was probed. The equivalence test below runs the shipped
// predicate and this one over the same simulated broker and requires an
// identical summary, which is the whole claim of the change: skipping the
// probe on a zero-connection VPN cannot alter the answer.
const legacyRealClientsPredicate = `{{and .Item.enabled (eq .Item.state "up")}}`

// vpnFixture is one row of a simulated monitor/getMsgVpns response plus the
// number of real (non-reserved) clients the broker would return when that VPN
// is probed via monitor/getMsgVpnClients.
//
// connections is `any` so a fixture can model the field being absent from the
// row (nil) as well as present. Present values are float64 because that is
// what encoding/json produces for a JSON number decoded into map[string]any,
// which is exactly how sempv2.HTTPClient builds these rows — the forEachIf
// predicate compares against a float literal and a fixture typed int would be
// testing a shape production can never produce.
type vpnFixture struct {
	name        string
	enabled     bool
	state       string
	connections any
	realClients int
	// indeterminate models a broker that does not honour forceFullPage: the
	// real-clients probe for this VPN comes back with zero rows but still
	// reports a further page pending, instead of exhausting the collection.
	// Mutually exclusive with realClients > 0 — a broker that found a match
	// would not also claim it stopped scanning early.
	indeterminate bool
}

func (f vpnFixture) row() map[string]any {
	row := map[string]any{
		"msgVpnName": f.name,
		"enabled":    f.enabled,
		"state":      f.state,
	}
	if f.connections != nil {
		row["msgVpnConnections"] = f.connections
	}
	return row
}

// brokerStub answers the two operations list-vpns issues, modelling the one
// broker invariant the change rides on: a VPN reporting msgVpnConnections == 0
// has no connections at all, so a client probe against it returns nothing. It
// also models a broker that does not honour forceFullPage (vpnFixture.indeterminate),
// so the real-clients probe's pagination envelope reaches the postprocess
// handler exactly as the real executor's fan-out would store it. Probed VPN
// names are recorded so a test can assert which probes were issued.
type brokerStub struct {
	mu     sync.Mutex
	vpns   []vpnFixture
	probes []string
}

func (b *brokerStub) Execute(_ context.Context, op *sempv2.Operation, args map[string]any) (*sempv2.Result, error) {
	switch op.ID {
	case "getMsgVpns":
		rows := make([]any, 0, len(b.vpns))
		for _, f := range b.vpns {
			rows = append(rows, f.row())
		}
		return &sempv2.Result{Data: map[string]any{"data": rows}, StatusCode: 200}, nil

	case "getMsgVpnClients":
		name, ok := args["msgVpnName"].(string)
		if !ok {
			return nil, fmt.Errorf("getMsgVpnClients called without a string msgVpnName arg: %v", args["msgVpnName"])
		}
		b.mu.Lock()
		b.probes = append(b.probes, name)
		b.mu.Unlock()

		rows := []any{}
		indeterminate := false
		for _, f := range b.vpns {
			if f.name != name {
				continue
			}
			indeterminate = f.indeterminate
			for i := 0; i < f.realClients; i++ {
				rows = append(rows, map[string]any{"clientName": fmt.Sprintf("%s-app-%d", name, i)})
			}
		}
		data := map[string]any{"data": rows}
		if indeterminate {
			// Mirrors the real SEMP envelope shape hasNextPage() reads
			// (meta.paging.nextPageUri) — see executor.go's extractNextPageURI.
			data["meta"] = map[string]any{"paging": map[string]any{"nextPageUri": "https://broker/next-page"}}
		}
		return &sempv2.Result{Data: data, StatusCode: 200}, nil

	default:
		return nil, fmt.Errorf("unexpected operation %q", op.ID)
	}
}

func (b *brokerStub) probedVPNs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := append([]string(nil), b.probes...)
	sort.Strings(out)
	return out
}

// loadListVPNs returns the shipped list-vpns definition. Called once per run so
// each caller gets its own Steps backing array and can retune a step without
// mutating another caller's copy.
func loadListVPNs(t *testing.T) CompositeTool {
	t.Helper()
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	tool := findTool(tools, "list-vpns")
	if tool == nil {
		t.Fatal("tool list-vpns not found in embedded definitions")
	}
	return *tool
}

// runListVPNs executes the given list-vpns definition against a stub broker
// built from fixtures, returning the postprocessed summary and the VPN names
// that were probed for real clients. It also asserts the verdict shape of every
// real-clients.byKey entry in the assembled response, so each caller checks the
// scrub the MCP caller depends on without repeating the assertion.
func runListVPNs(t *testing.T, tool CompositeTool, fixtures []vpnFixture) (map[string]any, []string) {
	t.Helper()
	broker := &brokerStub{vpns: fixtures}
	out, err := NewCompositeExecutor(testOperations()).Execute(context.Background(), tool, broker, map[string]any{})
	if err != nil {
		t.Fatalf("Execute(list-vpns): %v", err)
	}
	summary, ok := out["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary missing or wrong type: %T", out["summary"])
	}
	assertVerdictShapes(t, out)
	return summary, broker.probedVPNs()
}

// assertVerdictShapes fails unless every real-clients.byKey entry in the
// executor's assembled output is exactly one of the three verdict shapes
// ListVpns documents — {hasRealClient: true}, {hasRealClient: false},
// {indeterminate: true} — with no other keys. This is what the MCP caller
// receives: collectSteps copies the step map references verbatim after the
// handler returns, so a raw "data" or "meta" key left beside a verdict, or an
// indeterminate entry the scrub missed, would ship — and the summary
// assertions alone would never notice.
func assertVerdictShapes(t *testing.T, out map[string]any) {
	t.Helper()
	step, ok := out["real-clients"].(map[string]any)
	if !ok {
		t.Fatalf("real-clients step missing or wrong type in output: %T", out["real-clients"])
	}
	byKey, ok := step["byKey"].(map[string]any)
	if !ok {
		t.Fatalf("real-clients.byKey missing or wrong type: %T", step["byKey"])
	}
	verdicts := []map[string]any{
		{"hasRealClient": true},
		{"hasRealClient": false},
		{"indeterminate": true},
	}
	for vpn, raw := range byKey {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("byKey[%q]: want map[string]any, got %#v", vpn, raw)
			continue
		}
		matched := false
		for _, v := range verdicts {
			if reflect.DeepEqual(entry, v) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("byKey[%q] = %#v; want exactly one of %v", vpn, entry, verdicts)
		}
	}
}

// TestListVPNs_RealClientsProbeArgsMatchHandlerAssumption pins the args the
// postprocess handler's correctness silently depends on, plus the one tuning
// choice that is easy to "fix" back to the wrong value.
//
// ListVpns (internal/composite/postprocess/handlers/list_vpns.go) reads "zero
// rows but a nextPageUri is present" as indeterminate rather than as "no real
// client". That reading is only correct while this step asks the broker for an
// exhaustive search: forceFullPage=true makes the broker scan internally until
// the page holds `count` matches or the collection is exhausted, so an empty
// result without a next page genuinely means "there are none".
//
// Change either of these and the handler keeps its interpretation while the
// premise moves under it, with no error anywhere:
//   - drop forceFullPage → every probe returns a scan-window page with a next
//     page, so every VPN becomes indeterminate and zeroConnectionCount silently
//     goes to 0 forever;
//   - set followPages → per-step paging couples this probe's depth to the
//     tool-wide maxResults (resolveMaxResults reads it once), which is the
//     coupling forceFullPage exists to avoid.
//
// count is pinned for a different reason. With forceFullPage set, correctness
// does not depend on count at all — any value scans exhaustively — so count is
// a performance choice, not a correctness one. forceFullPage pages internally
// at `count`, so a small count costs one internal round trip per object
// scanned: measured on 10.26.6, a 19-client VPN with no real client took
// 55.5ms at count=1 versus 28.5ms at count=100, roughly 2ms per object, i.e.
// ~2s of avoidable latency on a 1,000-client VPN (see the step's comment in
// tools.yaml). The handler only needs "is there at least one?", which makes
// count=1 look like the obvious value; this pin exists so that change is made
// on purpose, with the measurement in hand.
//
// The repo enforces the handler's *field* dependency at boot (ValidateTool
// cross-checks RequiredFieldsPerStep against each step's select:), but there is
// no equivalent for arg dependencies, so this test is the enforcement.
func TestListVPNs_RealClientsProbeArgsMatchHandlerAssumption(t *testing.T) {
	tool := loadListVPNs(t)
	step := findStep(&tool, "real-clients")
	if step == nil {
		t.Fatal("step 'real-clients' not found in list-vpns")
	}
	if got := step.Args["forceFullPage"]; got != "true" {
		t.Errorf("real-clients args[forceFullPage] = %v, want \"true\" — the ListVpns indeterminate guard depends on this; see this test's doc comment before changing it", got)
	}
	if got := step.Args["count"]; got != "100" {
		t.Errorf("real-clients args[count] = %v, want \"100\" — count is a measured performance choice, not a correctness one; see this test's doc comment and the step's tools.yaml comment before changing it", got)
	}
	if step.FollowPages {
		t.Error("real-clients must not set followPages: forceFullPage moves paging broker-side precisely so this probe's depth is not coupled to the tool-wide maxResults")
	}
}

// TestExecute_ListVPNs_SkipsRealClientProbeOnZeroConnectionVPNs covers
// SOL-154166. The real-clients fan-out exists only to answer "does this VPN
// have a non-reserved client connected?", and the vpns step has already
// fetched msgVpnConnections for every row. A VPN reporting zero connections
// has no clients of any kind, so the probe's answer is known before it is
// issued: the postprocess handler reads an absent byKey entry as "no real
// client", which is the same verdict the probe would have produced.
//
// The test pins both halves of that claim on the shipped definition:
//   - which VPNs get probed: the zero-connection one no longer does, while
//     every VPN whose count is nonzero or absent still does; and
//   - the summary the handler produces is identical to the one the
//     pre-SOL-154166 predicate produced over the same stub.
//
// The second assertion is a regression fence over the predicate/handler pair,
// not evidence about real brokers: it holds because the stub models the broker
// invariant the change rests on (a VPN reporting zero connections returns no
// clients). That invariant is the thing to verify against a broker, not here.
func TestExecute_ListVPNs_SkipsRealClientProbeOnZeroConnectionVPNs(t *testing.T) {
	fixtures := []vpnFixture{
		// The optimization target: enabled, up, provably empty.
		{name: "idle", enabled: true, state: "up", connections: float64(0)},
		// One connection with no real client is the reserved #client case; one
		// connection with a real client is the other reading of the same count.
		// The pair is load-bearing: they disagree in the summary, so a
		// predicate that widened the skip to <= 1 would count "one-real" as
		// zero-connection and break the summary assertions below, not merely
		// the probe list. This is why the fan-out replaced the old
		// msgVpnConnections <= 1 heuristic in the first place.
		{name: "reserved-only", enabled: true, state: "up", connections: float64(1)},
		{name: "one-real", enabled: true, state: "up", connections: float64(1), realClients: 1},
		{name: "busy", enabled: true, state: "up", connections: float64(4), realClients: 2},
		// Broker omitted msgVpnConnections. The predicate must fail open and
		// probe rather than error out under missingkey=error or assume zero.
		{name: "count-absent", enabled: true, state: "up", connections: nil},
		{name: "down-vpn", enabled: true, state: "down", connections: float64(0)},
		{name: "standby-vpn", enabled: true, state: "standby", connections: float64(0)},
		{name: "disabled-vpn", enabled: false, state: "up", connections: float64(9), realClients: 1},
	}
	// Guard the fixture set against the physically impossible row that would
	// make this test lie: real clients on a VPN reporting zero connections.
	for _, f := range fixtures {
		if conns, isNum := f.connections.(float64); isNum && conns == 0 && f.realClients > 0 {
			t.Fatalf("fixture %q claims %d real clients with msgVpnConnections == 0; a broker cannot report that",
				f.name, f.realClients)
		}
	}

	shippedSummary, shippedProbes := runListVPNs(t, loadListVPNs(t), fixtures)

	legacy := loadListVPNs(t)
	legacyStep := findStep(&legacy, "real-clients")
	if legacyStep == nil {
		t.Fatal("step 'real-clients' not found in list-vpns")
	}
	legacyStep.ForEachIf = legacyRealClientsPredicate
	if err := compileStepTemplates(legacyStep); err != nil {
		t.Fatalf("compileStepTemplates(legacy predicate): %v", err)
	}
	legacySummary, legacyProbes := runListVPNs(t, legacy, fixtures)

	wantSummary := map[string]any{
		"disabledCount":       1, // disabled-vpn
		"downCount":           1, // down-vpn
		"standbyCount":        1, // standby-vpn
		"zeroConnectionCount": 3, // idle, reserved-only, count-absent
		"scanned":             len(fixtures),
	}
	if !reflect.DeepEqual(shippedSummary, wantSummary) {
		t.Errorf("summary = %v, want %v", shippedSummary, wantSummary)
	}
	if !reflect.DeepEqual(shippedSummary, legacySummary) {
		t.Errorf("summary differs from the pre-SOL-154166 predicate's: got %v, was %v — skipping the "+
			"probe on a zero-connection VPN must not change the answer", shippedSummary, legacySummary)
	}

	wantProbes := []string{"busy", "count-absent", "one-real", "reserved-only"}
	if !reflect.DeepEqual(shippedProbes, wantProbes) {
		t.Errorf("probed VPNs = %v, want %v", shippedProbes, wantProbes)
	}
	wantLegacyProbes := []string{"busy", "count-absent", "idle", "one-real", "reserved-only"}
	if !reflect.DeepEqual(legacyProbes, wantLegacyProbes) {
		t.Errorf("legacy probed VPNs = %v, want %v — the fixture no longer reproduces the "+
			"pre-fix behavior this test claims to compare against", legacyProbes, wantLegacyProbes)
	}
}

// TestExecute_ListVPNs_IndeterminateWhenBrokerDoesNotHonourForceFullPage covers
// the safety mechanism the SOL-153071 rework added: when the real-clients probe
// comes back empty but the broker reports a further page pending, the VPN must
// be reported as indeterminate rather than folded into zeroConnectionCount.
//
// Before this test, brokerStub had no way to produce that response shape at
// all — its getMsgVpnClients case never set meta.paging, so every probe result
// was either "has a real client" or "exhausted, no real client". The
// probeIndeterminate branch in probeRealClientOutcome
// (postprocess/handlers/list_vpns.go) was therefore only ever exercised by
// hand-built maps in list_vpns_test.go, never through the real
// executor/fan-out path this tool actually runs in production. This test
// closes that gap.
func TestExecute_ListVPNs_IndeterminateWhenBrokerDoesNotHonourForceFullPage(t *testing.T) {
	fixtures := []vpnFixture{
		{name: "idle", enabled: true, state: "up", connections: float64(1)},
		{name: "busy", enabled: true, state: "up", connections: float64(2), realClients: 1},
		{name: "degraded", enabled: true, state: "up", connections: float64(1), indeterminate: true},
	}
	for _, f := range fixtures {
		if f.indeterminate && f.realClients > 0 {
			t.Fatalf("fixture %q sets both indeterminate and realClients; a broker cannot claim it "+
				"stopped scanning early while also returning a match", f.name)
		}
	}

	summary, probes := runListVPNs(t, loadListVPNs(t), fixtures)

	wantSummary := map[string]any{
		"disabledCount":                0,
		"downCount":                    0,
		"standbyCount":                 0,
		"zeroConnectionCount":          1, // idle
		"indeterminateConnectionCount": 1, // degraded
		"scanned":                      len(fixtures),
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Errorf("summary = %v, want %v", summary, wantSummary)
	}

	wantProbes := []string{"busy", "degraded", "idle"}
	if !reflect.DeepEqual(probes, wantProbes) {
		t.Errorf("probed VPNs = %v, want %v", probes, wantProbes)
	}
}
