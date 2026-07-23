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

// listKafkaReceiversStepID is the step ID this handler keys into. Declared as
// a const so the init-time RequiredSteps registration and the runtime lookup
// cannot drift out of sync — and so the boot-time check in ValidatePostProcess
// catches a YAML rename of the step.
const listKafkaReceiversStepID = "kafkaReceivers"

func init() {
	postprocess.Register("listKafkaReceivers", postprocess.Handler{
		Fn:            ListKafkaReceivers,
		RequiredSteps: []string{listKafkaReceiversStepID},
		RequiredFields: []string{
			"up",
			"enabled",
			"failureReason",
		},
	})
}

// ListKafkaReceivers aggregates the Kafka Receiver list into two counts plus
// a failure-reason map. Kafka Receivers use a single up/down field (unlike
// bridges' two independent directions), so this mirrors listRdps exactly:
//   - downCount:       enabled && !up — the "unexpectedly down" set
//     (admin-disabled receivers are excluded, since they're down by design —
//     same shape as list-rdps.downCount / list-vpns.downCount / list-bridges.downCount).
//   - disabledCount:    Kafka Receivers with enabled == false.
//   - byFailureReason:  count grouped by failureReason, restricted to down,
//     enabled receivers with a non-empty reason, so the map reflects
//     UNEXPECTED active failures — not a stale reason left over on a receiver
//     that has since recovered. Whether this field reliably populates on a
//     real broker (vs. staying empty like bridges' inboundFailureReason did)
//     is not yet lab-verified — see SOL-152328.
//
// Robustness mirrors listRdps: a row missing or wrong-type on any required
// field is tallied into skipped (surfaced when non-zero) rather than aborting
// the call — one odd row must not drop the raw list. Structural errors (step
// missing, data not a list, item not an object) still hard-fail since the
// whole result is unusable.
func ListKafkaReceivers(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listKafkaReceiversStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listKafkaReceiversStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("kafkaReceivers.data: want []any, got %T", step["data"])
	}
	var down, disabled, skipped int
	byReason := map[string]int{}
	for i, raw := range items {
		r, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("kafkaReceivers.data[%d]: want object, got %T", i, raw)
		}
		up, ok1 := boolField(r, "up")
		enabled, ok2 := boolField(r, "enabled")
		reason, ok3 := stringField(r, "failureReason")
		if !ok1 || !ok2 || !ok3 {
			skipped++
			continue
		}
		if !enabled {
			disabled++
			continue
		}
		if !up {
			down++
			if reason != "" {
				byReason[reason]++
			}
		}
	}
	out := map[string]any{
		"downCount":       down,
		"disabledCount":   disabled,
		"byFailureReason": byReason,
		"scanned":         len(items),
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}
