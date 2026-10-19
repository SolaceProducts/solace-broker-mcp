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
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

func bindingRow(nameField, name string, enabled, up bool, reason string) any {
	return map[string]any{nameField: name, "enabled": enabled, "up": up, "failureReason": reason}
}

func bindingSteps(stepID string, items []any, truncated bool) map[string]map[string]any {
	step := map[string]any{"data": items}
	if truncated {
		step["truncated"] = true
	}
	return map[string]map[string]any{stepID: step}
}

func summarizeBindings(t *testing.T, handler, stepID string, items []any, truncated bool) map[string]any {
	t.Helper()
	out, err := postprocess.Apply(handler, bindingSteps(stepID, items, truncated))
	if err != nil {
		t.Fatalf("Apply(%s): %v", handler, err)
	}
	return out
}

func TestGetKafkaReceiverStatus_NamesTheFailedBindingAndItsReason(t *testing.T) {
	// The lab case: the bridge reads fine, one binding is down.
	items := []any{
		bindingRow("topicName", "orders", true, true, ""),
		map[string]any{"topicName": "payments", "enabled": true, "up": false,
			"failureReason": "Local failure: spool over quota", "lastNotice": "Enabled"},
		bindingRow("topicName", "audit", false, false, "Shutdown"),
	}
	out := summarizeBindings(t, "getKafkaReceiverStatus", "topicBindings", items, false)

	if out["bindingScannedCount"] != 3 || out["bindingUpCount"] != 1 || out["bindingDownCount"] != 1 || out["bindingDisabledCount"] != 1 {
		t.Errorf("counts = %v", out)
	}
	byReason, _ := out["byBindingFailureReason"].(map[string]int)
	if len(byReason) != 1 || byReason["Local failure: spool over quota"] != 1 {
		t.Errorf("byBindingFailureReason = %v, want only the active failure (the disabled binding's Shutdown must not count)", byReason)
	}
	failed, _ := out["failedBindings"].([]map[string]any)
	if len(failed) != 1 || failed[0]["name"] != "payments" || failed[0]["failureReason"] != "Local failure: spool over quota" || failed[0]["lastNotice"] != "Enabled" {
		t.Errorf("failedBindings = %v", failed)
	}
	for _, absent := range []string{"bindingSkipped", "bindingTruncated", "failedBindingsTruncated"} {
		if _, has := out[absent]; has {
			t.Errorf("%s must be absent when zero/false", absent)
		}
	}
}

func TestGetKafkaSenderStatus_UsesQueueNames(t *testing.T) {
	items := []any{
		bindingRow("queueName", "q-out", true, false, "Remote topic has no leader on remote-broker"),
		bindingRow("queueName", "q-ok", true, true, ""),
	}
	out := summarizeBindings(t, "getKafkaSenderStatus", "queueBindings", items, false)
	failed, _ := out["failedBindings"].([]map[string]any)
	if len(failed) != 1 || failed[0]["name"] != "q-out" || failed[0]["failureReason"] != "Remote topic has no leader on remote-broker" {
		t.Errorf("failedBindings = %v", failed)
	}
	if _, has := failed[0]["lastNotice"]; has {
		t.Error("lastNotice must be omitted when the row has none")
	}
}

func TestGetKafkaBindingStatus_StaleReasonOnRecoveredBindingIsNotAFailure(t *testing.T) {
	// SEMP keeps the last failureReason on a binding that has since come up.
	out := summarizeBindings(t, "getKafkaReceiverStatus", "topicBindings",
		[]any{bindingRow("topicName", "orders", true, true, "Remote topic has no leader on remote-broker")}, false)
	if out["bindingDownCount"] != 0 || len(out["byBindingFailureReason"].(map[string]int)) != 0 || len(out["failedBindings"].([]map[string]any)) != 0 {
		t.Errorf("a recovered binding must not read as failing: %v", out)
	}
}

func TestGetKafkaBindingStatus_EmptyIsAllZeroNotAnError(t *testing.T) {
	out := summarizeBindings(t, "getKafkaSenderStatus", "queueBindings", []any{}, false)
	if out["bindingScannedCount"] != 0 || out["bindingDownCount"] != 0 {
		t.Errorf("empty list summary = %v", out)
	}
}

func TestGetKafkaBindingStatus_DownWithoutReasonIsCountedButNotGrouped(t *testing.T) {
	out := summarizeBindings(t, "getKafkaReceiverStatus", "topicBindings",
		[]any{bindingRow("topicName", "orders", true, false, "")}, false)
	if out["bindingDownCount"] != 1 || len(out["byBindingFailureReason"].(map[string]int)) != 0 {
		t.Errorf("summary = %v", out)
	}
	if failed := out["failedBindings"].([]map[string]any); len(failed) != 1 || failed[0]["name"] != "orders" {
		t.Errorf("a down binding is still listed even with no reason: %v", failed)
	}
}

func TestGetKafkaBindingStatus_FailedListIsCappedAndSaysSo(t *testing.T) {
	var items []any
	for i := 0; i < maxFailedBindingsListed+5; i++ {
		items = append(items, bindingRow("topicName", fmt.Sprintf("t%02d", i), true, false, "boom"))
	}
	out := summarizeBindings(t, "getKafkaReceiverStatus", "topicBindings", items, false)
	if got := len(out["failedBindings"].([]map[string]any)); got != maxFailedBindingsListed {
		t.Errorf("failedBindings has %d entries, want the cap %d", got, maxFailedBindingsListed)
	}
	if out["failedBindingsTruncated"] != true {
		t.Error("failedBindingsTruncated must be set when the list was cut")
	}
	// Counts still cover every binding, not just the listed ones.
	if out["bindingDownCount"] != maxFailedBindingsListed+5 || out["byBindingFailureReason"].(map[string]int)["boom"] != maxFailedBindingsListed+5 {
		t.Errorf("counts must not be capped: %v", out)
	}
}

func TestGetKafkaBindingStatus_SkipsMalformedRowsAndPassesTruncationThrough(t *testing.T) {
	items := []any{
		bindingRow("topicName", "ok", true, true, ""),
		map[string]any{"topicName": "no-up-field", "enabled": true, "failureReason": ""},
		map[string]any{"enabled": true, "up": true, "failureReason": ""}, // no name
	}
	out := summarizeBindings(t, "getKafkaReceiverStatus", "topicBindings", items, true)
	if out["bindingSkipped"] != 2 || out["bindingTruncated"] != true || out["bindingScannedCount"] != 3 || out["bindingUpCount"] != 1 {
		t.Errorf("summary = %v", out)
	}
}

func TestGetKafkaBindingStatus_StructuralErrorsFail(t *testing.T) {
	cases := map[string]map[string]map[string]any{
		"missing step":       {},
		"data not a list":    {"topicBindings": {"data": "oops"}},
		"item not an object": {"topicBindings": {"data": []any{"oops"}}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := postprocess.Apply("getKafkaReceiverStatus", steps); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// The loader runs ValidateTool at startup: the handler must declare the step and
// the select fields the YAML provides, per kind. The bridge step's real select is
// included in every case so that a missing binding field cannot be masked by it.
func TestGetKafkaBindingStatus_RegistrationMatchesTheToolShape(t *testing.T) {
	bridge := []string{"enabled", "up", "failureReason", "kafkaReceiverName", "topicBindingCount", "topicBindingUpCount"}
	steps := []string{"kafkaReceiverStatus", "topicBindings"}
	good := map[string][]string{"kafkaReceiverStatus": bridge, "topicBindings": {"topicName", "enabled", "up", "failureReason", "lastNotice"}}
	if err := postprocess.ValidateTool("get-kafka-receiver-status", "getKafkaReceiverStatus", steps, good); err != nil {
		t.Errorf("receiver: %v", err)
	}
	bad := map[string][]string{"kafkaReceiverStatus": bridge, "topicBindings": {"topicName", "enabled", "up"}}
	if err := postprocess.ValidateTool("get-kafka-receiver-status", "getKafkaReceiverStatus", steps, bad); err == nil {
		t.Error("a bindings select missing failureReason must fail validation, even though the bridge step selects it")
	}
	if err := postprocess.ValidateTool("get-kafka-sender-status", "getKafkaSenderStatus", []string{"kafkaSenderStatus"}, map[string][]string{}); err == nil {
		t.Error("a missing bindings step must fail validation")
	}
}

// Required fields are checked per step. The bridge-status step also selects
// enabled, up and failureReason, so a union-of-all-steps check would let the
// bindings step drop them unnoticed; the summary would then skip every binding as
// malformed and report zero down.
func TestGetKafkaBindingStatus_RequiredFieldsAreCheckedPerStep(t *testing.T) {
	bridgeSelect := []string{"enabled", "up", "failureReason", "kafkaReceiverName", "topicBindingCount"}
	for _, field := range []string{"enabled", "up", "failureReason", "topicName"} {
		t.Run("bindings step without "+field, func(t *testing.T) {
			var bindings []string
			for _, f := range []string{"topicName", "enabled", "up", "failureReason", "lastNotice"} {
				if f != field {
					bindings = append(bindings, f)
				}
			}
			selects := map[string][]string{"kafkaReceiverStatus": bridgeSelect, "topicBindings": bindings}
			err := postprocess.ValidateTool("get-kafka-receiver-status", "getKafkaReceiverStatus", []string{"kafkaReceiverStatus", "topicBindings"}, selects)
			if err == nil {
				t.Errorf("a bindings select without %q must fail validation even though the bridge step selects it", field)
			}
		})
	}
	good := map[string][]string{"kafkaReceiverStatus": {"kafkaReceiverName"}, "topicBindings": {"topicName", "enabled", "up", "failureReason"}}
	if err := postprocess.ValidateTool("get-kafka-receiver-status", "getKafkaReceiverStatus", []string{"kafkaReceiverStatus", "topicBindings"}, good); err != nil {
		t.Errorf("a complete bindings select must pass even when the bridge step selects none of the fields: %v", err)
	}
}
