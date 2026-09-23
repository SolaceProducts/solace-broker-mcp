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
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

// indeterminateLogSample bounds how many VPN names the indeterminate warning
// names. list-vpns can return up to 500 VPNs, and a degraded broker would make
// every one of them indeterminate at once; the count carries the scale, so the
// names only need to be enough to start an investigation.
const indeterminateLogSample = 5

// Step IDs this handler keys into. Declared as consts so the init-time
// RequiredSteps registration and the runtime lookups cannot drift out of sync,
// and so the boot-time cross-check in ValidatePostProcess catches a YAML
// rename of either step.
const (
	listVpnsStepID    = "vpns"
	listVpnsClientsID = "real-clients"
)

func init() {
	postprocess.Register("listVpns", postprocess.Handler{
		Fn:            ListVpns,
		RequiredSteps: []string{listVpnsStepID, listVpnsClientsID},
		RequiredFieldsPerStep: map[string][]string{
			listVpnsStepID:    {"enabled", "state", "msgVpnName"},
			listVpnsClientsID: {"clientName"},
		},
	})
}

// ListVpns aggregates the VPN list into four mutually-exclusive-per-lens counts:
//   - disabledCount:        VPNs with enabled == false
//   - downCount:            enabled VPNs whose state == "down" (primary
//     operational alarm — "should be serving but isn't")
//   - standbyCount:         enabled VPNs whose state == "standby" (informational;
//     HA mode, not a problem)
//   - zeroConnectionCount:  enabled+up VPNs established to have no real
//     (non-reserved) client connected. Derived from a per-VPN getMsgVpnClients
//     probe filtered server-side by `clientUsername != #*` — the `#*` prefix is
//     Solace's documented reserved-name contract for internal clients, so this is
//     version-independent. Previous implementations (before the real-clients
//     fan-out step) inferred this from `msgVpnConnections <= 1` on the empirical
//     invariant that the reserved `#client` shows up as exactly one connection
//     per enabled+up VPN. That invariant is no longer load-bearing: the count
//     is consulted only to skip the probe at exactly 0 (SOL-154166), where "no
//     connections" implies "no clients" on any broker version, never at 1.
//   - indeterminateConnectionCount: enabled+up VPNs whose probe did not settle
//     the question either way (see probeRealClient). Omitted when zero. These
//     are deliberately excluded from zeroConnectionCount rather than folded
//     into it: reporting an unverified VPN as idle is the exact failure this
//     probe exists to prevent, and an operator acting on it could decommission
//     a VPN carrying live traffic.
//
// down/standby/zeroConnection are all gated on enabled==true so a disabled VPN
// (which typically reports state=="down") lands in disabledCount only, and the
// LLM can read each count as an independent signal without subtracting overlaps.
//
// The counts are over what the paginator actually returned for the vpns step.
// Under truncation (followPages stopped at maxResults / capMax / maxPages), the
// summary also includes scanned and truncated: true so the LLM sees the
// partial-scan reality next to the counts rather than only on the raw data block.
//
// A VPN whose required fields are missing or the wrong type is skipped from
// every counter and tallied into skipped (surfaced when non-zero). One odd row
// must not drop the raw list — that would be a robustness step down from the
// collect strategy. Structural errors (step missing, data not a list, item not
// an object) still hard-fail since the whole result is unusable.
func ListVpns(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listVpnsStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listVpnsStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("vpns.data: want []any, got %T", step["data"])
	}
	// real-clients is a fan-out result: { byKey: { vpnName: {data: [...], ...}, ...} }.
	// When the vpns step returns zero rows, the fan-out has nothing to iterate and
	// executor.fetchFanOut still stores an empty byKey — so we always expect the
	// step to be present. Missing step is a wiring bug; empty byKey is legal.
	clientsStep, ok := stepResults[listVpnsClientsID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listVpnsClientsID)
	}
	byKey, ok := clientsStep["byKey"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("real-clients.byKey: want map[string]any, got %T", clientsStep["byKey"])
	}

	var disabled, down, standby, zeroConn, indeterminate, skipped int
	var indeterminateVpns []string
	for i, raw := range items {
		v, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("vpns.data[%d]: want object, got %T", i, raw)
		}
		enabled, ok1 := boolField(v, "enabled")
		state, ok2 := stringField(v, "state")
		vpnName, ok3 := stringField(v, "msgVpnName")
		if !ok1 || !ok2 || !ok3 {
			skipped++
			continue
		}
		if !enabled {
			disabled++
			continue
		}
		switch state {
		case "down":
			down++
		case "standby":
			standby++
		case "up":
			switch probeRealClientOutcome(byKey, vpnName) {
			case probeNone:
				zeroConn++
			case probeIndeterminate:
				indeterminate++
				indeterminateVpns = append(indeterminateVpns, vpnName)
			case probeRealClient:
			}
		}
	}
	// The probe's raw client rows and paging metadata must not reach the caller:
	// they are internal to how zeroConnectionCount is computed, and stepResults
	// is shared, unbuffered state — the executor's response assembly
	// (collectSteps) copies these same map references after this handler
	// returns, with no filtering of its own. Each entry is replaced by the
	// single fact a consumer needs.
	//
	// Note the shapes are disjoint rather than a bool plus a flag: an
	// indeterminate entry carries no hasRealClient key at all, so a caller
	// reading hasRealClient == false can never silently inherit an unverified
	// VPN as an idle one.
	for vpnName := range byKey {
		switch probeRealClientOutcome(byKey, vpnName) {
		case probeRealClient:
			byKey[vpnName] = map[string]any{"hasRealClient": true}
		case probeNone:
			byKey[vpnName] = map[string]any{"hasRealClient": false}
		case probeIndeterminate:
			byKey[vpnName] = map[string]any{"indeterminate": true}
		}
	}

	out := map[string]any{
		"disabledCount":       disabled,
		"downCount":           down,
		"standbyCount":        standby,
		"zeroConnectionCount": zeroConn,
		"scanned":             len(items),
	}
	if indeterminate > 0 {
		out["indeterminateConnectionCount"] = indeterminate
		// Without this the degradation is effectively invisible: the only other
		// evidence is a summary field in a tool response that an MCP client may
		// never surface to a human. An indeterminate probe means the broker
		// stopped scanning early, which in practice means forceFullPage is no
		// longer being honoured — a silent regression to SOL-153071 territory
		// that an operator needs to hear about from logs, not from a user
		// noticing zeroConnectionCount has quietly pinned to zero. Mirrors the
		// executor's existing "pagination page cap reached" warning for the
		// analogous incomplete-result case.
		sort.Strings(indeterminateVpns)
		sample := indeterminateVpns
		if len(sample) > indeterminateLogSample {
			sample = sample[:indeterminateLogSample]
		}
		slog.Warn("list-vpns real-client probe did not complete for some VPNs; they are excluded from zeroConnectionCount rather than reported as idle",
			slog.Int("indeterminate_vpns", indeterminate),
			slog.Int("scanned_vpns", len(items)),
			slog.String("example_vpns", strings.Join(sample, ",")),
			slog.String("likely_cause", "broker did not honour the forceFullPage query parameter on the real-clients probe"))
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}

// probeOutcome is what the per-VPN real-clients probe actually established.
type probeOutcome int

const (
	probeNone          probeOutcome = iota // definitively no real client
	probeRealClient                        // definitively at least one real client
	probeIndeterminate                     // the probe did not settle the question
)

// probeRealClient classifies the per-VPN probe result.
//
// A non-empty data[] means the broker returned a client whose clientUsername
// did not match the reserved `#*` prefix, so the VPN has a real client.
//
// An empty data[] is only "no real client" when the broker also reports no
// further page. The step runs with count=1 and forceFullPage=true, which makes
// the broker scan internally until the page holds a match or the collection is
// exhausted; on exhaustion it drops nextPageUri. So empty-and-no-next means
// "there are none", while empty-with-a-next-page means the scan stopped early
// and nothing was established — the signature of forceFullPage not being
// honoured. That case must not be read as an idle VPN: SEMP applies `where`
// after the count cut, so a single unscanned page is exactly how SOL-153071
// produced false positives across every VPN at once.
//
// A missing key is "no real client", not indeterminate, and is expected:
// forEachIf filters out disabled/down/standby rows (whose branches never reach
// here), and since SOL-154166 it also filters out an enabled+up VPN whose
// msgVpnConnections is 0. That last case does reach here, and probeNone is
// correct — zero connections means no clients of any kind, so the probe that
// was skipped would have come back empty anyway.
func probeRealClientOutcome(byKey map[string]any, vpnName string) probeOutcome {
	entry, ok := byKey[vpnName].(map[string]any)
	if !ok {
		return probeNone
	}
	if data, ok := entry["data"].([]any); ok && len(data) > 0 {
		return probeRealClient
	}
	if hasNextPage(entry) {
		return probeIndeterminate
	}
	return probeNone
}

// hasNextPage reports whether a SEMP response envelope carries pagination
// metadata pointing at a further page.
func hasNextPage(entry map[string]any) bool {
	meta, ok := entry["meta"].(map[string]any)
	if !ok {
		return false
	}
	paging, ok := meta["paging"].(map[string]any)
	if !ok {
		return false
	}
	uri, _ := paging["nextPageUri"].(string)
	return uri != ""
}
