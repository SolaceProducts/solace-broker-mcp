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

// Step IDs the handler keys into. Declared as consts so the init-time
// RequiredSteps registration and the runtime lookups cannot drift out of sync
// — a YAML rename of either step is caught at boot by ValidatePostProcess.
const (
	rdpStatusBindingsStepID  = "queueBindings"
	rdpStatusConsumersStepID = "restConsumers"
)

func init() {
	postprocess.Register("getRdpStatus", postprocess.Handler{
		Fn:            GetRdpStatus,
		RequiredSteps: []string{rdpStatusBindingsStepID, rdpStatusConsumersStepID},
		// First multi-step handler: fields are declared per step so
		// ValidatePostProcess can check each step's own `select:` covers
		// what the handler reads, rather than the union across steps.
		RequiredFieldsPerStep: map[string][]string{
			rdpStatusBindingsStepID:  {"up", "lastFailureReason"},
			rdpStatusConsumersStepID: {"up", "lastFailureReason"},
		},
	})
}

// GetRdpStatus aggregates the two list steps under a get-rdp-status call so
// an operator (or an LLM answering "is anything in this RDP broken?") gets
// scalar counts instead of having to walk two arrays. The rdpStatus step is a
// single object and is left untouched — the executor merges it into the raw
// result as-is; a single flat object needs no folding.
//
// Per array (queueBindings, restConsumers):
//   - <array>UpCount / <array>DownCount:     count by up == true/false
//   - by<Array>LastFailureReason:            count grouped by lastFailureReason,
//     restricted to currently-down rows (up == false) so the map reflects
//     active failures. SEMP retains stale lastFailureReason on recovered
//     items, so unfiltered counts would fold historical scars in with real
//     current failures (same rationale as listRdps' byLastFailureReason).
//     The empty-string bucket is omitted to keep the map LLM-readable.
//   - <array>ScannedCount:                   items observed, so callers can
//     distinguish "0 down" (nothing broken) from "0 total" (nothing configured).
//   - <array>Truncated / <array>Skipped:     surfaced only when non-zero.
//
// Flat keys (bindingDownCount, consumerDownCount, ...) rather than nested
// (summary.bindings.downCount) — the LLM templates against these strings, and
// a nested path adds a level without saving tokens.
//
// Robustness mirrors listQueues / listRdps: a row missing or wrong-type on a
// required field is tallied into <array>Skipped rather than aborting the call
// — one odd row must not drop the raw result. Structural errors (step
// missing, data not a list, item not an object) still hard-fail since the
// whole array is unusable.
func GetRdpStatus(stepResults map[string]map[string]any) (map[string]any, error) {
	bindings, err := aggregateRdpArray(stepResults, rdpStatusBindingsStepID)
	if err != nil {
		return nil, err
	}
	consumers, err := aggregateRdpArray(stepResults, rdpStatusConsumersStepID)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"bindingUpCount":              bindings.up,
		"bindingDownCount":            bindings.down,
		"bindingScannedCount":         bindings.scanned,
		"byBindingLastFailureReason":  bindings.byReason,
		"consumerUpCount":             consumers.up,
		"consumerDownCount":           consumers.down,
		"consumerScannedCount":        consumers.scanned,
		"byConsumerLastFailureReason": consumers.byReason,
	}
	if bindings.skipped > 0 {
		out["bindingSkipped"] = bindings.skipped
	}
	if bindings.truncated {
		out["bindingTruncated"] = true
	}
	if consumers.skipped > 0 {
		out["consumerSkipped"] = consumers.skipped
	}
	if consumers.truncated {
		out["consumerTruncated"] = true
	}
	return out, nil
}

// rdpArrayCounts collects the per-array aggregation output before the handler
// assembles the flat summary map. Kept internal — the shape of the summary is
// the public contract, not this struct.
type rdpArrayCounts struct {
	up, down, scanned, skipped int
	byReason                   map[string]int
	truncated                  bool
}

// aggregateRdpArray walks one step's data list and returns the counts, the
// active-failure bucket, and the pass-through truncated flag. Bindings and
// consumers share identical field semantics (up + lastFailureReason), so a
// single helper covers both — keeps the two aggregations from drifting.
func aggregateRdpArray(stepResults map[string]map[string]any, stepID string) (rdpArrayCounts, error) {
	var out rdpArrayCounts
	out.byReason = map[string]int{}

	step, ok := stepResults[stepID]
	if !ok {
		return out, fmt.Errorf("step %q not in results", stepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return out, fmt.Errorf("%s.data: want []any, got %T", stepID, step["data"])
	}
	for i, raw := range items {
		r, ok := raw.(map[string]any)
		if !ok {
			return out, fmt.Errorf("%s.data[%d]: want object, got %T", stepID, i, raw)
		}
		up, ok1 := boolField(r, "up")
		reason, ok2 := stringField(r, "lastFailureReason")
		if !ok1 || !ok2 {
			out.skipped++
			continue
		}
		if up {
			out.up++
		} else {
			out.down++
			if reason != "" {
				out.byReason[reason]++
			}
		}
	}
	out.scanned = len(items)
	if t, _ := step["truncated"].(bool); t {
		out.truncated = true
	}
	return out, nil
}
