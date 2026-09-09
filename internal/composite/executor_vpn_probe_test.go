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
// has no connections at all, so a client probe against it returns nothing.
// Probed VPN names are recorded so a test can assert which probes were issued.
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
		for _, f := range b.vpns {
			if f.name != name {
				continue
			}
			for i := 0; i < f.realClients; i++ {
				rows = append(rows, map[string]any{"clientName": fmt.Sprintf("%s-app-%d", name, i)})
			}
		}
		return &sempv2.Result{Data: map[string]any{"data": rows}, StatusCode: 200}, nil

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
// that were probed for real clients.
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
	return summary, broker.probedVPNs()
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
