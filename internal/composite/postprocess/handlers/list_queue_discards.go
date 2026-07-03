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
	"sort"

	"github.com/SolaceDev/solace-broker-mcp/internal/composite/postprocess"
)

// listQueueDiscardsStepID is the step ID this handler keys into. Kept const so
// the init-time RequiredSteps registration, the runtime lookup, and the
// boot-time ValidatePostProcess check cannot drift out of sync with a YAML
// rename.
const listQueueDiscardsStepID = "queueDiscards"

// topOffenderLimit caps topOffenderQueues at the worst 10 offenders. Operators
// want "the worst offenders" — not a query knob. Change the constant if this
// proves wrong in practice (per ticket SOL-151316 scoping notes).
const topOffenderLimit = 10

// discardFields is the closed set of per-queue counters this handler sums
// into totalDiscards and scans for dominantCategory. Every entry represents a
// message that was truly lost — never delivered and not routed to a DMQ. The
// set is a subset of the list-queue-discards `select:` in tools.yaml; the
// ValidatePostProcess boot check enforces RequiredFields ⊆ select.
//
// Deliberately EXCLUDED: maxRedeliveryExceededToDmqMsgCount and
// maxTtlExpiredToDmqMsgCount. Both are successful DMQ moves — the message
// left the queue but was delivered to the Dead Message Queue as configured,
// which is expected behavior, not a discard. Counting them would inflate the
// offender score of queues doing exactly what they were designed to do.
// The *ToDmqFailedMsgCount counters ARE included because DMQ resolution
// failed there, so the message actually was lost.
var discardFields = []string{
	"clientProfileDeniedDiscardedMsgCount",
	"destinationGroupErrorDiscardedMsgCount",
	"disabledDiscardedMsgCount",
	"lowPriorityMsgCongestionDiscardedMsgCount",
	"maxMsgSizeExceededDiscardedMsgCount",
	"maxMsgSpoolUsageExceededDiscardedMsgCount",
	"maxRedeliveryExceededDiscardedMsgCount",
	"maxRedeliveryExceededToDmqFailedMsgCount",
	"maxTtlExceededDiscardedMsgCount",
	"maxTtlExpiredDiscardedMsgCount",
	"maxTtlExpiredToDmqFailedMsgCount",
	"noLocalDeliveryDiscardedMsgCount",
	"xaTransactionNotSupportedDiscardedMsgCount",
}

func init() {
	// Sort so the dominantCategory tie-breaker ("first-encountered wins on
	// tie") coincides with "alphabetically earliest wins" regardless of
	// declaration order. Kept as an init-time step rather than a hand-sorted
	// literal so an edit to the slice can't silently break the invariant.
	sort.Strings(discardFields)
	// RequiredFields is the union of discardFields plus the identifiers used
	// to name each offender row. ValidatePostProcess enforces this ⊆ select:
	// at boot so a YAML select-list edit that drops a counter is caught then,
	// not at first invocation.
	required := make([]string, 0, len(discardFields)+2)
	required = append(required, "queueName", "msgVpnName")
	required = append(required, discardFields...)
	postprocess.Register("listQueueDiscards", postprocess.Handler{
		Fn:             ListQueueDiscards,
		RequiredSteps:  []string{listQueueDiscardsStepID},
		RequiredFields: required,
	})
}

// offender is a single row in topOffenderQueues. Kept private — the emitted
// shape is a map[string]any so we don't have to plumb a JSON tag through the
// composite executor's untyped result path.
type offender struct {
	queueName        string
	msgVpnName       string
	totalDiscards    float64
	dominantCategory string
}

// ListQueueDiscards aggregates per-queue discard counters into an offender
// breakdown. Complements get-discard-stats (VPN-wide category totals) with the
// per-queue signal that tool structurally can't produce.
//
// Emits:
//   - topOffenderQueues: up to 10 queues with totalDiscards > 0, sorted
//     descending by totalDiscards, ties broken alphabetically by queueName.
//     Each row: {queueName, msgVpnName, totalDiscards, dominantCategory}.
//     Omitted entirely when no queue has any discards.
//   - discardingQueueCount: count of queues where totalDiscards > 0.
//   - scanned: number of items observed.
//   - truncated: true iff the paginator stopped early (propagated from step).
//   - skipped: count of rows dropped due to a missing/malformed required
//     field, present only when non-zero.
//
// Skip-don't-abort mirrors listQueues: one bad row shouldn't drop the raw
// list. Structural errors (missing step, wrong data type) still hard-fail.
func ListQueueDiscards(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[listQueueDiscardsStepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", listQueueDiscardsStepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("queueDiscards.data: want []any, got %T", step["data"])
	}

	offenders := make([]offender, 0)
	var discarding, skipped int
	for i, raw := range items {
		q, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("queueDiscards.data[%d]: want object, got %T", i, raw)
		}
		queueName, okName := stringField(q, "queueName")
		vpnName, okVpn := stringField(q, "msgVpnName")
		if !okName || !okVpn {
			skipped++
			continue
		}
		total, dominant, ok := sumDiscards(q)
		if !ok {
			skipped++
			continue
		}
		if total > 0 {
			discarding++
			offenders = append(offenders, offender{
				queueName:        queueName,
				msgVpnName:       vpnName,
				totalDiscards:    total,
				dominantCategory: dominant,
			})
		}
	}

	// Descending by totalDiscards; ties broken alphabetically by queueName
	// so the offender list is deterministic across identical broker states.
	sort.Slice(offenders, func(i, j int) bool {
		if offenders[i].totalDiscards != offenders[j].totalDiscards {
			return offenders[i].totalDiscards > offenders[j].totalDiscards
		}
		return offenders[i].queueName < offenders[j].queueName
	})
	if len(offenders) > topOffenderLimit {
		offenders = offenders[:topOffenderLimit]
	}

	out := map[string]any{
		"discardingQueueCount": discarding,
		"scanned":              len(items),
	}
	if len(offenders) > 0 {
		rows := make([]map[string]any, len(offenders))
		for i, o := range offenders {
			rows[i] = map[string]any{
				"queueName":        o.queueName,
				"msgVpnName":       o.msgVpnName,
				"totalDiscards":    o.totalDiscards,
				"dominantCategory": o.dominantCategory,
			}
		}
		out["topOffenderQueues"] = rows
	}
	if skipped > 0 {
		out["skipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["truncated"] = true
	}
	return out, nil
}

// sumDiscards totals every discardFields counter for a queue and identifies
// the single largest contributor. Returns ok=false if any counter is missing
// or the wrong type so the caller can skip the row rather than emit a
// half-counted offender. Ties on the dominant category are broken
// alphabetically on the field name for determinism.
func sumDiscards(q map[string]any) (total float64, dominant string, ok bool) {
	var max float64
	for _, f := range discardFields {
		v, fieldOk := numField(q, f)
		if !fieldOk {
			return 0, "", false
		}
		total += v
		// Strict > picks the first-seen max; equal values fall through so
		// the alphabetically-earliest field wins (discardFields is sorted).
		if v > max {
			max = v
			dominant = f
		}
	}
	return total, dominant, true
}
