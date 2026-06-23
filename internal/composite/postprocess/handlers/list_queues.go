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

// Package handlers registers the production postprocess handlers. Each handler
// lives in its own file and self-registers from init(). main.go pulls them in
// via a blank import so the registry is populated before startup validation.
package handlers

import (
	"encoding/json"
	"fmt"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
)

// nearFullThreshold is the msgSpoolUsage / maxMsgSpoolUsage ratio at or above
// which a queue is counted as "near full". 0.8 is the conventional warning
// threshold for guaranteed-message storage; queues at this fill level are at
// risk of taking SPOOL_OVER_QUOTA discards if their consumers don't catch up.
const nearFullThreshold = 0.8

// listQueuesStepID is the step ID this handler keys into. Declared as a const
// so the init-time RequiredSteps registration and the runtime lookup cannot
// drift out of sync — and so the boot-time check in ValidatePostProcess
// catches a YAML rename of the step.
const listQueuesStepID = "queues"

func init() {
	postprocess.Register("listQueues", postprocess.Handler{
		Fn:            ListQueues,
		RequiredSteps: []string{listQueuesStepID},
		RequiredFields: []string{
			"bindCount",
			"spooledMsgCount",
			"lowPriorityMsgCongestionState",
			"msgSpoolUsage",
			"maxMsgSpoolUsage",
		},
	})
}

// ListQueues aggregates the queue list into four counts:
//   - noConsumerCount: queues with bindCount == 0 (nothing reading)
//   - congestedCount:  queues whose lowPriorityMsgCongestionState == "active"
//   - backloggedCount: queues with spooledMsgCount > 0 (any backlog at all)
//   - nearFullCount:   queues with msgSpoolUsage/maxMsgSpoolUsage >= 0.8
//     (skips queues with maxMsgSpoolUsage == 0 — unbounded or unset)
//
// The counts are over what the paginator actually returned. Under truncation
// (followPages stopped at maxResults / capMax / maxPages), the summary also
// includes scanned (the count of items observed) and truncated: true so the
// LLM sees the partial-scan reality next to the counts rather than only on
// the raw data block.
func ListQueues(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listQueuesStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listQueuesStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("queues.data: want []any, got %T", step["data"])
	}
	var noConsumer, congested, backlogged, nearFull int
	for i, raw := range items {
		q, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("queues.data[%d]: want object, got %T", i, raw)
		}
		bindCount, err := numField(q, "bindCount", i)
		if err != nil {
			return nil, err
		}
		state, err := stringField(q, "lowPriorityMsgCongestionState", i)
		if err != nil {
			return nil, err
		}
		spooled, err := numField(q, "spooledMsgCount", i)
		if err != nil {
			return nil, err
		}
		usage, err := numField(q, "msgSpoolUsage", i)
		if err != nil {
			return nil, err
		}
		maxUsage, err := numField(q, "maxMsgSpoolUsage", i)
		if err != nil {
			return nil, err
		}
		if bindCount == 0 {
			noConsumer++
		}
		if state == "active" {
			congested++
		}
		if spooled > 0 {
			backlogged++
		}
		if maxUsage > 0 && usage/maxUsage >= nearFullThreshold {
			nearFull++
		}
	}
	out := map[string]any{
		"noConsumerCount": noConsumer,
		"congestedCount":  congested,
		"backloggedCount": backlogged,
		"nearFullCount":   nearFull,
		"scanned":         len(items),
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}

// numField accepts any of the numeric shapes JSON decoders produce: float64
// (encoding/json default for map[string]any), json.Number (decoder with
// UseNumber), int / int64 (custom unmarshalers). Keeps the handler insulated
// from future SEMP-client decode-mode changes.
func numField(item map[string]any, name string, i int) (float64, error) {
	switch v := item[name].(type) {
	case float64:
		return v, nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("queues.data[%d].%s: parse json.Number: %w", i, name, err)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("queues.data[%d].%s: want number, got %T", i, name, item[name])
	}
}

func stringField(item map[string]any, name string, i int) (string, error) {
	v, ok := item[name].(string)
	if !ok {
		return "", fmt.Errorf("queues.data[%d].%s: want string, got %T", i, name, item[name])
	}
	return v, nil
}
