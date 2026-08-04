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

// listSlowSubscribersStepID is the step ID this handler keys into. Declared as
// a const so the init-time RequiredSteps registration and the runtime lookup
// cannot drift out of sync — and so the boot-time check in ValidateTool
// catches a YAML rename of the step.
const listSlowSubscribersStepID = "slowSubscribers"

func init() {
	postprocess.Register("listSlowSubscribers", postprocess.Handler{
		Fn:            ListSlowSubscribers,
		RequiredSteps: []string{listSlowSubscribersStepID},
		RequiredFields: []string{
			"clientProfileName",
			"platform",
			"txDiscardedMsgCount",
		},
	})
}

// ListSlowSubscribers aggregates the slow-subscriber list into triage signals:
//   - byClientProfile: count grouped by clientProfileName — surfaces whether
//     offenders share a QoS/policy bucket.
//   - byPlatform:      count grouped by platform — surfaces SDK/version
//     regressions.
//   - totalTxDiscardedMsgCount: sum of txDiscardedMsgCount — severity signal;
//     distinguishes minor TCP blips from a flood of dropped messages.
//
// Aggregations are over what the paginator actually returned. Under truncation
// the summary also includes scanned (items observed) and truncated: true so
// the LLM sees the partial-scan reality next to the counts.
//
// A row whose required fields are missing or the wrong type is skipped from
// every aggregation and tallied into skipped (surfaced only when non-zero).
// One odd row must not drop the raw list — that would be a robustness step
// down from the collect strategy. Structural errors (step missing, data not a
// list, item not an object) still hard-fail since the whole result is unusable.
func ListSlowSubscribers(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listSlowSubscribersStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listSlowSubscribersStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("slowSubscribers.data: want []any, got %T", step["data"])
	}
	byClientProfile := map[string]int{}
	byPlatform := map[string]int{}
	var totalDiscarded float64
	var skipped int
	for i, raw := range items {
		c, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("slowSubscribers.data[%d]: want object, got %T", i, raw)
		}
		profile, ok1 := stringField(c, "clientProfileName")
		platform, ok2 := stringField(c, "platform")
		discarded, ok3 := numField(c, "txDiscardedMsgCount")
		if !ok1 || !ok2 || !ok3 {
			skipped++
			continue
		}
		byClientProfile[profile]++
		byPlatform[platform]++
		totalDiscarded += discarded
	}
	out := map[string]any{
		"byClientProfile":          byClientProfile,
		"byPlatform":               byPlatform,
		"totalTxDiscardedMsgCount": totalDiscarded,
		"scanned":                  len(items),
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}
