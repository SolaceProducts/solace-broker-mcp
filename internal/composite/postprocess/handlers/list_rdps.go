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

// listRdpsStepID is the step ID this handler keys into. Declared as a const
// so the init-time RequiredSteps registration and the runtime lookup cannot
// drift out of sync — and so the boot-time check in ValidatePostProcess
// catches a YAML rename of the step.
const listRdpsStepID = "rdps"

func init() {
	postprocess.Register("listRdps", postprocess.Handler{
		Fn:            ListRdps,
		RequiredSteps: []string{listRdpsStepID},
		RequiredFields: []string{
			"up",
			"enabled",
			"lastFailureReason",
		},
	})
}

// ListRdps aggregates the RDP list into three counts plus a failure-reason map:
//   - downCount:           RDPs with up == false
//   - disabledCount:       RDPs with enabled == false
//   - downButEnabledCount: enabled && !up — the "unexpectedly down" /
//     page-worthy count (admin-disabled RDPs are excluded, since they're down
//     by design)
//   - byLastFailureReason: count grouped by lastFailureReason, restricted to
//     currently-down RDPs (up == false) so the map reflects ACTIVE failures,
//     not historical scars on RDPs that have since recovered. The empty-string
//     bucket is omitted to keep the map LLM-readable.
//
// Robustness mirrors listQueues: a row missing or wrong-type on any required
// field is tallied into skipped (surfaced when non-zero) rather than aborting
// the call — one odd row must not drop the raw list. Structural errors (step
// missing, data not a list, item not an object) still hard-fail since the
// whole result is unusable.
func ListRdps(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listRdpsStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listRdpsStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("rdps.data: want []any, got %T", step["data"])
	}
	var down, disabled, downButEnabled, skipped int
	byReason := map[string]int{}
	for i, raw := range items {
		r, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("rdps.data[%d]: want object, got %T", i, raw)
		}
		up, ok1 := boolField(r, "up")
		enabled, ok2 := boolField(r, "enabled")
		reason, ok3 := stringField(r, "lastFailureReason")
		if !ok1 || !ok2 || !ok3 {
			skipped++
			continue
		}
		if !up {
			down++
		}
		if !enabled {
			disabled++
		}
		if !up && enabled {
			downButEnabled++
		}
		if !up && reason != "" {
			byReason[reason]++
		}
	}
	out := map[string]any{
		"downCount":           down,
		"disabledCount":       disabled,
		"downButEnabledCount": downButEnabled,
		"byLastFailureReason": byReason,
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

// boolField returns the bool at name and ok=true if present and bool-typed.
// Missing, nil, or any other type returns ok=false so the caller can skip the
// row rather than abort. Mirrors numField / stringField in list_queues.go.
func boolField(item map[string]any, name string) (bool, bool) {
	v, ok := item[name].(bool)
	return v, ok
}
