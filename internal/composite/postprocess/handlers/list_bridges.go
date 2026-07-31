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

// listBridgesStepID is the step ID this handler keys into. Declared as a const
// so the init-time RequiredSteps registration and the runtime lookup cannot
// drift out of sync — and so the boot-time check in ValidatePostProcess
// catches a YAML rename of the step.
const listBridgesStepID = "bridges"

// bridgeInboundUpStates are the inboundState values that mean the inbound
// connection (messages arriving from the remote broker) is up. "not-applicable"
// means the inbound direction doesn't apply to this bridge at all — SEMP
// reports it for e.g. bridges configured one-directional — so it is treated as
// healthy rather than down. Every other value in the enum (init, disabled,
// prepare-*, not-ready-*, stalled, ...) is a down/transitional state.
var bridgeInboundUpStates = map[string]bool{
	"ready-subscribing": true,
	"ready-in-sync":     true,
	"not-applicable":    true,
}

// bridgeOutboundUpStates are the outboundState values that mean the outbound
// connection (messages sent to the remote broker) is up. Unlike inboundState,
// SEMP only defines two values for outboundState: "ready" and "not-applicable".
var bridgeOutboundUpStates = map[string]bool{
	"ready":          true,
	"not-applicable": true,
}

func init() {
	postprocess.Register("listBridges", postprocess.Handler{
		Fn:            ListBridges,
		RequiredSteps: []string{listBridgesStepID},
		RequiredFields: []string{
			"enabled",
			"inboundState",
			"outboundState",
			"inboundFailureReason",
		},
	})
}

// ListBridges aggregates the bridge list into two counts plus a failure-reason
// map. A bridge has two independent connection directions, so — unlike RDPs
// and their single up/down field — "down" here means either direction is
// unhealthy while the bridge itself is enabled:
//   - downCount:               enabled && (inboundState not in
//     bridgeInboundUpStates || outboundState not in bridgeOutboundUpStates).
//     Admin-disabled bridges are excluded, since they're down by design — same
//     shape as list-rdps.downCount / list-vpns.downCount.
//   - disabledCount:           bridges with enabled == false.
//   - byInboundFailureReason:  count grouped by inboundFailureReason,
//     restricted to enabled bridges whose INBOUND direction specifically is
//     unhealthy, with a non-empty reason. Gating on the inbound direction
//     (not just "down") matters because SEMP does not clear
//     inboundFailureReason when inboundState recovers — an outbound-only
//     failure (inbound healthy, outbound not) can coexist with a stale
//     non-empty inboundFailureReason from a since-recovered inbound issue;
//     bucketing on "down" alone would misreport that as an active inbound
//     failure. SEMP has no equivalent outboundFailureReason field, so
//     outbound-only failures land in downCount without a bucketed reason.
//
// Robustness mirrors listRdps: a row missing or wrong-type on any required
// field is tallied into skipped (surfaced when non-zero) rather than aborting
// the call — one odd row must not drop the raw list. Structural errors (step
// missing, data not a list, item not an object) still hard-fail since the
// whole result is unusable.
func ListBridges(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listBridgesStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listBridgesStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("bridges.data: want []any, got %T", step["data"])
	}
	var down, disabled, skipped int
	byReason := map[string]int{}
	for i, raw := range items {
		b, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("bridges.data[%d]: want object, got %T", i, raw)
		}
		enabled, ok1 := boolField(b, "enabled")
		inboundState, ok2 := stringField(b, "inboundState")
		outboundState, ok3 := stringField(b, "outboundState")
		inboundFailureReason, ok4 := stringField(b, "inboundFailureReason")
		if !ok1 || !ok2 || !ok3 || !ok4 {
			skipped++
			continue
		}
		if !enabled {
			disabled++
			continue
		}
		inboundUnhealthy := !bridgeInboundUpStates[inboundState]
		outboundUnhealthy := !bridgeOutboundUpStates[outboundState]
		if inboundUnhealthy || outboundUnhealthy {
			down++
			// Gated on inboundUnhealthy specifically — see byInboundFailureReason above.
			if inboundUnhealthy && inboundFailureReason != "" {
				byReason[inboundFailureReason]++
			}
		}
	}
	out := map[string]any{
		"downCount":              down,
		"disabledCount":          disabled,
		"byInboundFailureReason": byReason,
		"scanned":                len(items),
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}
