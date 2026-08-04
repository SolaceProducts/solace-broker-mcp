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

// listKafkaSendersStepID is the step ID this handler keys into. Declared as a
// const so the init-time RequiredSteps registration and the runtime lookup
// cannot drift out of sync — and so the boot-time check in ValidatePostProcess
// catches a YAML rename of the step.
const listKafkaSendersStepID = "kafkaSenders"

func init() {
	postprocess.Register("listKafkaSenders", postprocess.Handler{
		Fn:            ListKafkaSenders,
		RequiredSteps: []string{listKafkaSendersStepID},
		RequiredFields: []string{
			"up",
			"enabled",
			"failureReason",
		},
	})
}

// ListKafkaSenders aggregates the Kafka Sender list into two counts plus a
// failure-reason map. Identical shape to ListKafkaReceivers (Kafka Senders
// use the same single up/down field) — see that function's doc comment for
// the full rationale.
func ListKafkaSenders(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listKafkaSendersStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listKafkaSendersStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("kafkaSenders.data: want []any, got %T", step["data"])
	}
	var down, disabled, skipped int
	byReason := map[string]int{}
	for i, raw := range items {
		r, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("kafkaSenders.data[%d]: want object, got %T", i, raw)
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
