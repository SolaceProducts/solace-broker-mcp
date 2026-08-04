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
	"fmt"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

// nearFullThreshold is the spool-usage fill ratio at or above which a queue is
// counted as "near full". 0.8 is the conventional warning threshold for
// guaranteed-message storage; queues at this fill level are at risk of taking
// SPOOL_OVER_QUOTA discards if their consumers don't catch up.
const nearFullThreshold = 0.8

// bytesPerMB converts maxMsgSpoolUsage (megabytes, per the SEMPv2 monitor spec)
// to bytes so it can be compared against msgSpoolUsage (bytes). Without this
// conversion the fill ratio is off by a factor of ~1e6 and flags essentially
// every non-empty queue as near full (SOL-150260).
const bytesPerMB = 1024 * 1024

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
			"lowPriorityMsgCongestionState",
			"msgSpoolUsage",
			"maxMsgSpoolUsage",
		},
	})
}

// ListQueues aggregates the queue list into three counts:
//   - noConsumerCount: queues with bindCount == 0 (nothing reading)
//   - congestedCount:  queues whose lowPriorityMsgCongestionState == "congested"
//   - nearFullCount:   queues whose spool usage is >= 80% of their quota, i.e.
//     msgSpoolUsage (bytes) / (maxMsgSpoolUsage MB * bytesPerMB) >= 0.8
//     (skips queues with maxMsgSpoolUsage == 0 — unbounded or unset)
//
// Note there is intentionally no "backlog"/"depth" count here: spooledMsgCount
// is a cumulative lifetime counter (messages ever spooled), not the current
// number of messages in a queue, so counting queues with spooledMsgCount > 0
// would report every queue that has ever received a message — not queues with a
// live backlog (SOL-150260). A current-depth-based count requires either
// deriving currentMsgCount = spooledMsgCount - deletedMsgCount or an
// authoritative SEMPv1 num-messages-spooled read; both are deferred to the
// follow-up capability work.
//
// The counts are over what the paginator actually returned. Under truncation
// (followPages stopped at maxResults / capMax / maxPages), the summary also
// includes scanned (the count of items observed) and truncated: true so the
// LLM sees the partial-scan reality next to the counts rather than only on
// the raw data block.
//
// A queue whose required fields are missing or the wrong type is skipped from
// every counter and tallied into skipped (surfaced when non-zero). One odd row
// must not drop the raw list — that would be a robustness step down from the
// collect strategy. Structural errors (step missing, data not a list, item not
// an object) still hard-fail since the whole result is unusable.
func ListQueues(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listQueuesStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listQueuesStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("queues.data: want []any, got %T", step["data"])
	}
	var noConsumer, congested, nearFull, skipped int
	for i, raw := range items {
		q, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("queues.data[%d]: want object, got %T", i, raw)
		}
		bindCount, ok1 := numField(q, "bindCount")
		state, ok2 := stringField(q, "lowPriorityMsgCongestionState")
		usage, ok3 := numField(q, "msgSpoolUsage")
		maxUsage, ok4 := numField(q, "maxMsgSpoolUsage")
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
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}
