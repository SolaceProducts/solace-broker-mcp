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

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
)

// listVpnsStepID is the step ID this handler keys into. Declared as a const
// so the init-time RequiredSteps registration and the runtime lookup cannot
// drift out of sync — and so the boot-time check in ValidatePostProcess
// catches a YAML rename of the step.
const listVpnsStepID = "vpns"

func init() {
	postprocess.Register("listVpns", postprocess.Handler{
		Fn:            ListVpns,
		RequiredSteps: []string{listVpnsStepID},
		RequiredFields: []string{
			"enabled",
			"state",
			"msgVpnConnections",
		},
	})
}

// ListVpns aggregates the VPN list into four counts:
//   - disabledCount:        VPNs with enabled == false
//   - downCount:            VPNs whose state == "down" (primary operational alarm)
//   - standbyCount:         VPNs whose state == "standby" (informational; HA mode,
//     not a problem)
//   - zeroConnectionCount:  VPNs with enabled == true && state == "up" &&
//     msgVpnConnections == 0 (configured to serve but nobody's connecting).
//     Standby and disabled VPNs are excluded so HA standby brokers don't
//     false-alarm.
//
// The counts are over what the paginator actually returned. Under truncation
// (followPages stopped at maxResults / capMax / maxPages), the summary also
// includes scanned and truncated: true so the LLM sees the partial-scan reality
// next to the counts rather than only on the raw data block.
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
	var disabled, down, standby, zeroConn, skipped int
	for i, raw := range items {
		v, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("vpns.data[%d]: want object, got %T", i, raw)
		}
		enabled, ok1 := boolField(v, "enabled")
		state, ok2 := stringField(v, "state")
		conns, ok3 := numField(v, "msgVpnConnections")
		if !ok1 || !ok2 || !ok3 {
			skipped++
			continue
		}
		if !enabled {
			disabled++
		}
		switch state {
		case "down":
			down++
		case "standby":
			standby++
		}
		if enabled && state == "up" && conns == 0 {
			zeroConn++
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

// boolField returns the named bool from item. Returns ok=false for missing,
// nil, or unexpected types so the caller can skip the row rather than abort.
func boolField(item map[string]any, name string) (bool, bool) {
	v, ok := item[name].(bool)
	return v, ok
}
