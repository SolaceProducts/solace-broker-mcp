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

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

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
//   - zeroConnectionCount:  enabled+up VPNs with no real (non-reserved) client
//     connected. Derived directly from a per-VPN getMsgVpnClients probe filtered
//     server-side by `clientUsername != #*` — the `#*` prefix is Solace's
//     documented reserved-name contract for internal clients, so this is
//     version-independent. Previous implementations (before the real-clients
//     fan-out step) inferred this from `msgVpnConnections <= 1` on the empirical
//     invariant that the reserved `#client` shows up as exactly one connection
//     per enabled+up VPN. That invariant is no longer load-bearing.
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

	var disabled, down, standby, zeroConn, skipped int
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
			if !hasRealClient(byKey, vpnName) {
				zeroConn++
			}
		}
	}
	out := map[string]any{
		"disabledCount":       disabled,
		"downCount":           down,
		"standbyCount":        standby,
		"zeroConnectionCount": zeroConn,
		"scanned":             len(items),
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}

// hasRealClient returns true when the per-VPN probe returned at least one row.
// A row present in byKey with a non-empty data[] means the broker had a client
// whose clientUsername did not match the reserved `#*` prefix. Missing key or
// empty data[] means "no real client" — the VPN is enabled+up but no user
// clients are connected. A key can be missing legitimately when forEachIf
// filtered the row out (disabled/down VPNs), but those branches never call
// this function; when this fires and finds nothing, that IS the signal.
func hasRealClient(byKey map[string]any, vpnName string) bool {
	entry, ok := byKey[vpnName].(map[string]any)
	if !ok {
		return false
	}
	data, ok := entry["data"].([]any)
	if !ok {
		return false
	}
	return len(data) > 0
}
