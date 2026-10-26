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

// listTopicEndpointsStepID is the step ID this handler keys into, shared by
// the init-time RequiredSteps registration and the runtime lookup so a YAML
// rename of the step fails ValidatePostProcess at boot.
const listTopicEndpointsStepID = "topicEndpoints"

func init() {
	postprocess.Register("listTopicEndpoints", postprocess.Handler{
		Fn:            ListTopicEndpoints,
		RequiredSteps: []string{listTopicEndpointsStepID},
		RequiredFields: []string{
			"bindCount",
			"lowPriorityMsgCongestionState",
			"msgSpoolUsage",
			"maxSpoolUsage",
		},
	})
}

// ListTopicEndpoints summarizes the topic endpoint list the same way
// ListQueues summarizes queues: noConsumerCount (bindCount == 0),
// congestedCount, and nearFullCount (msgSpoolUsage bytes >= 80% of the quota).
// The quota field differs from the queue's: a topic endpoint's is
// maxSpoolUsage (MB), not maxMsgSpoolUsage. An endpoint with maxSpoolUsage 0
// is skipped from nearFullCount.
//
// Truncation, cursor-resume and skipped-row handling match ListQueues: counts
// cover only the rows scanned, a row with a missing or wrong-typed field is
// skipped and tallied, and a structural error fails the call.
func ListTopicEndpoints(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listTopicEndpointsStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listTopicEndpointsStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("topicEndpoints.data: want []any, got %T", step["data"])
	}
	var noConsumer, congested, nearFull, skipped int
	for i, raw := range items {
		te, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("topicEndpoints.data[%d]: want object, got %T", i, raw)
		}
		bindCount, ok1 := numField(te, "bindCount")
		state, ok2 := stringField(te, "lowPriorityMsgCongestionState")
		usage, ok3 := numField(te, "msgSpoolUsage")
		maxUsage, ok4 := numField(te, "maxSpoolUsage")
		if !ok1 || !ok2 || !ok3 || !ok4 {
			skipped++
			continue
		}
		if bindCount == 0 {
			noConsumer++
		}
		if state == "congested" {
			congested++
		}
		if maxUsage > 0 && usage/(maxUsage*bytesPerMB) >= nearFullThreshold {
			nearFull++
		}
	}
	out := map[string]any{
		"noConsumerCount": noConsumer,
		"congestedCount":  congested,
		"nearFullCount":   nearFull,
		"scanned":         len(items),
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	truncated, _ := step["truncated"].(bool)
	resumed, _ := step["resumed"].(bool)
	if truncated {
		out["truncated"] = true
	}
	if truncated || resumed {
		note := fmt.Sprintf("These counts cover only the %d topic endpoints scanned, not the whole VPN.", len(items))
		if resumed {
			note += " They exclude the earlier pages of this scan."
		}
		// Conditional: the response-size cap runs after this handler and
		// removes nextCursor from a page it trims (composite.BoundResponse).
		if _, ok := step["nextCursor"].(string); ok {
			note += " If topicEndpoints.nextCursor is present, pass it as cursor to scan the next page."
		}
		out["scopeNote"] = note
	}
	return out, nil
}
