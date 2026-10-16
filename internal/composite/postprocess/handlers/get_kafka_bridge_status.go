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

// maxFailedBindingsListed bounds the per-binding detail in the summary. The raw
// step still carries up to its page size; this list is what an agent reads, so it
// must stay small however many bindings fail (see SOL-155426: an unbounded
// per-item list is how a summary overflows a host's token budget).
const maxFailedBindingsListed = 20

// kafkaBindingStatusKind describes the bindings of one kind of Kafka bridge: a
// receiver's topic bindings or a sender's queue bindings. They expose the same
// up / enabled / failureReason fields, so one implementation serves both.
type kafkaBindingStatusKind struct {
	handlerName string // registry name referenced from tools.yaml
	stepID      string // the YAML step listing the bindings
	nameField   string // attribute naming a binding: topicName or queueName
}

var kafkaBindingStatusKinds = []kafkaBindingStatusKind{
	{handlerName: "getKafkaReceiverStatus", stepID: "topicBindings", nameField: "topicName"},
	{handlerName: "getKafkaSenderStatus", stepID: "queueBindings", nameField: "queueName"},
}

func init() {
	for _, k := range kafkaBindingStatusKinds {
		postprocess.Register(k.handlerName, postprocess.Handler{
			Fn:             k.summarize,
			RequiredSteps:  []string{k.stepID},
			RequiredFields: []string{k.nameField, "enabled", "up", "failureReason"},
		})
	}
}

// summarize turns a bridge's binding list into the answer to "which binding
// failed, and why?". The bridge's own up/failureReason and the binding counts
// come from the status step and are left untouched in the raw result; they do
// not say which binding is down. Observed in the lab: a Kafka Sender read
// up=true with an empty failureReason and queueBindingUpCount=0 while its only
// binding was down with "Remote topic has no leader on remote-broker".
//
// Flat keys, like get-rdp-status:
//   - bindingScannedCount / bindingUpCount / bindingDownCount / bindingDisabledCount.
//     "Down" is enabled && !up, the unexpectedly-down set, matching
//     list-kafka-receivers' downCount; an administratively disabled binding is
//     counted separately, never as a failure.
//   - byBindingFailureReason: counts of unexpectedly-down bindings by reason. Only
//     currently-failing bindings are counted: the broker keeps a stale reason on
//     a recovered one, which would otherwise read as a live fault. An empty
//     reason is omitted.
//   - failedBindings: for each unexpectedly-down binding, its name, failureReason
//     and lastNotice, capped at maxFailedBindingsListed (failedBindingsTruncated
//     says so).
//   - bindingSkipped / bindingTruncated: surfaced only when non-zero / true.
//
// A row missing or wrong-typed on a required field is tallied into bindingSkipped
// rather than aborting the call. Structural errors (step missing, data not a
// list, item not an object) still fail, since the whole list is then unusable.
func (k kafkaBindingStatusKind) summarize(stepResults map[string]map[string]any) (map[string]any, error) {
	step, ok := stepResults[k.stepID]
	if !ok {
		return nil, fmt.Errorf("step %q not in results", k.stepID)
	}
	items, ok := step["data"].([]any)
	if !ok {
		return nil, fmt.Errorf("%s.data: want []any, got %T", k.stepID, step["data"])
	}

	var up, down, disabled, skipped int
	byReason := map[string]int{}
	failed := []map[string]any{}
	failedOverflow := false

	for i, raw := range items {
		r, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.data[%d]: want object, got %T", k.stepID, i, raw)
		}
		name, ok1 := stringField(r, k.nameField)
		enabled, ok2 := boolField(r, "enabled")
		isUp, ok3 := boolField(r, "up")
		reason, ok4 := stringField(r, "failureReason")
		if !ok1 || !ok2 || !ok3 || !ok4 {
			skipped++
			continue
		}
		switch {
		case !enabled:
			disabled++
		case isUp:
			up++
		default:
			down++
			if reason != "" {
				byReason[reason]++
			}
			if len(failed) >= maxFailedBindingsListed {
				failedOverflow = true
				continue
			}
			entry := map[string]any{"name": name, "failureReason": reason}
			if notice, ok := stringField(r, "lastNotice"); ok && notice != "" {
				entry["lastNotice"] = notice
			}
			failed = append(failed, entry)
		}
	}

	out := map[string]any{
		"bindingScannedCount":    len(items),
		"bindingUpCount":         up,
		"bindingDownCount":       down,
		"bindingDisabledCount":   disabled,
		"byBindingFailureReason": byReason,
		"failedBindings":         failed,
	}
	if failedOverflow {
		out["failedBindingsTruncated"] = true
	}
	if skipped > 0 {
		out["bindingSkipped"] = skipped
	}
	if t, _ := step["truncated"].(bool); t {
		out["bindingTruncated"] = true
	}
	return out, nil
}
