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
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

func receiverStep(stepID string, data map[string]any) map[string]map[string]any {
	return map[string]map[string]any{stepID: {"data": data, "meta": map[string]any{"responseCode": float64(200)}}}
}

func receiverData(scheme string, enabled bool) map[string]any {
	return map[string]any{
		"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "authenticationScheme": scheme, "enabled": enabled,
	}
}

func apply(t *testing.T, name string, steps map[string]map[string]any) map[string]any {
	t.Helper()
	out, err := postprocess.Apply(name, steps)
	if err != nil {
		t.Fatalf("Apply(%s): %v", name, err)
	}
	return out
}

func TestKafkaReceiverCreated_NoneEnabled_PointsAtStatusPolling(t *testing.T) {
	out := apply(t, "kafkaReceiverCreated", receiverStep("createKafkaReceiver", receiverData("none", true)))
	if out["secretRequired"] != false {
		t.Errorf("secretRequired = %v, want false for scheme none", out["secretRequired"])
	}
	if _, has := out["handoff"]; has {
		t.Error("no handoff expected when no secret is needed")
	}
	next, _ := out["nextStep"].(string)
	for _, want := range []string{"get-kafka-receiver-status", "10 to 40 seconds", "topicBindingUpCount", "topicBindingCount"} {
		if !strings.Contains(next, want) {
			t.Errorf("nextStep %q should contain %q", next, want)
		}
	}
	if out["object"] != "kafkaReceiver" || out["name"] != "rx" || out["msgVpnName"] != "vpn1" {
		t.Errorf("identity fields wrong: %v", out)
	}
}

func TestKafkaReceiverCreated_NoneDisabled_SaysHowToEnable(t *testing.T) {
	out := apply(t, "kafkaReceiverCreated", receiverStep("createKafkaReceiver", receiverData("none", false)))
	next, _ := out["nextStep"].(string)
	if !strings.Contains(next, "update-kafka-receiver") || !strings.Contains(next, "created disabled") {
		t.Errorf("nextStep = %q", next)
	}
}

func TestKafkaReceiverCreated_AuthenticatedSchemes_HandOffTheSecret(t *testing.T) {
	for scheme, wantFields := range kafkaSchemeSecretFields {
		t.Run(scheme, func(t *testing.T) {
			out := apply(t, "kafkaReceiverCreated", receiverStep("createKafkaReceiver", receiverData(scheme, false)))
			if out["secretRequired"] != true {
				t.Fatalf("secretRequired = %v, want true", out["secretRequired"])
			}
			fields, _ := out["secretFields"].([]string)
			if strings.Join(fields, ",") != strings.Join(wantFields, ",") {
				t.Errorf("secretFields = %v, want %v", fields, wantFields)
			}
			handoff, _ := out["handoff"].(string)
			for _, want := range []string{`"rx"`, `"vpn1"`, `"` + scheme + `"`, "Do not paste the secret into this chat", "together", "Tell me when it is done"} {
				if !strings.Contains(handoff, want) {
					t.Errorf("handoff should contain %q:\n%s", want, handoff)
				}
			}
			next, _ := out["nextStep"].(string)
			for _, want := range []string{"update-kafka-receiver", `"enabled": true`, "kafkaReceiverConfig", "get-kafka-receiver-status"} {
				if !strings.Contains(next, want) {
					t.Errorf("nextStep should contain %q:\n%s", want, next)
				}
			}
		})
	}
}

func TestKafkaReceiverCreated_UnknownSchemeFallsBackToGenericText(t *testing.T) {
	out := apply(t, "kafkaReceiverCreated", receiverStep("createKafkaReceiver", receiverData("future-scheme", false)))
	fields, _ := out["secretFields"].([]string)
	if len(fields) != 1 || !strings.Contains(fields[0], "future-scheme") {
		t.Errorf("secretFields = %v, want one generic entry naming the scheme", fields)
	}
	if h, _ := out["handoff"].(string); !strings.Contains(h, "Do not paste the secret into this chat") {
		t.Errorf("handoff missing for an unknown scheme: %q", h)
	}
}

func TestKafkaReceiverUpdated(t *testing.T) {
	enabled := apply(t, "kafkaReceiverUpdated", receiverStep("updateKafkaReceiver", receiverData("basic", true)))
	if next, _ := enabled["nextStep"].(string); !strings.Contains(next, "get-kafka-receiver-status") || !strings.Contains(next, "Username or password missing") {
		t.Errorf("enabled nextStep = %q", next)
	}
	disabled := apply(t, "kafkaReceiverUpdated", receiverStep("updateKafkaReceiver", receiverData("basic", false)))
	if next, _ := disabled["nextStep"].(string); !strings.Contains(next, "is disabled") || !strings.Contains(next, "out of band first") {
		t.Errorf("disabled nextStep = %q", next)
	}
	for _, out := range []map[string]any{enabled, disabled} {
		if _, has := out["handoff"]; has {
			t.Error("an update result must not carry a hand-off block")
		}
	}
}

func TestKafkaBridgeWrite_MalformedResultsAreErrors(t *testing.T) {
	cases := map[string]map[string]map[string]any{
		"missing step":       {},
		"data not an object": {"createKafkaReceiver": {"data": "oops"}},
		"missing name":       {"createKafkaReceiver": {"data": map[string]any{"msgVpnName": "v"}}},
		"missing vpn":        {"createKafkaReceiver": {"data": map[string]any{"kafkaReceiverName": "r"}}},
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := postprocess.Apply("kafkaReceiverCreated", steps); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

// ValidateTool is what the loader runs at startup: the registered handler must
// declare the step and the select fields the YAML provides.
func TestKafkaBridgeWrite_RegistrationMatchesTheToolShape(t *testing.T) {
	selects := map[string][]string{"createKafkaReceiver": {"msgVpnName", "kafkaReceiverName", "authenticationScheme", "enabled"}}
	if err := postprocess.ValidateTool("create-kafka-receiver", "kafkaReceiverCreated", []string{"createKafkaReceiver"}, selects); err != nil {
		t.Errorf("created: %v", err)
	}
	// Dropping a field the handler reads from the select must be caught at boot.
	bad := map[string][]string{"createKafkaReceiver": {"msgVpnName", "kafkaReceiverName", "enabled"}}
	if err := postprocess.ValidateTool("create-kafka-receiver", "kafkaReceiverCreated", []string{"createKafkaReceiver"}, bad); err == nil {
		t.Error("a select missing authenticationScheme must fail validation")
	}
	if err := postprocess.ValidateTool("update-kafka-receiver", "kafkaReceiverUpdated", []string{"someOtherStep"}, selects); err == nil {
		t.Error("a renamed step must fail validation")
	}
}

func senderStep(stepID string, data map[string]any) map[string]map[string]any {
	return map[string]map[string]any{stepID: {"data": data}}
}

func TestKafkaSenderHandlers_UseSenderNamesAndBindingFields(t *testing.T) {
	data := map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx", "authenticationScheme": "basic", "enabled": false}
	created := apply(t, "kafkaSenderCreated", senderStep("createKafkaSender", data))
	if created["object"] != "kafkaSender" || created["name"] != "tx" {
		t.Errorf("identity wrong: %v", created)
	}
	handoff, _ := created["handoff"].(string)
	if !strings.Contains(handoff, "Kafka Sender") || strings.Contains(handoff, "Receiver") {
		t.Errorf("handoff should speak of the sender only:\n%s", handoff)
	}
	next, _ := created["nextStep"].(string)
	if !strings.Contains(next, "update-kafka-sender") || !strings.Contains(next, "kafkaSenderConfig") || strings.Contains(next, "kafkaReceiverConfig") {
		t.Errorf("nextStep = %q", next)
	}

	data["enabled"] = true
	updated := apply(t, "kafkaSenderUpdated", senderStep("updateKafkaSender", data))
	if n, _ := updated["nextStep"].(string); !strings.Contains(n, "get-kafka-sender-status") || !strings.Contains(n, "queueBindingUpCount") || strings.Contains(n, "topicBinding") {
		t.Errorf("updated nextStep = %q", n)
	}
}

// Every registered kind must be fully described: a missing field would produce
// text that points at a tool or field that does not exist.
func TestKafkaBridgeKinds_AreFullyDescribed(t *testing.T) {
	for _, k := range kafkaBridgeKinds {
		for name, v := range map[string]string{
			"object": k.object, "label": k.label, "nameField": k.nameField, "configParam": k.configParam,
			"createTool": k.createTool, "updateTool": k.updateTool, "statusTool": k.statusTool,
			"createStepID": k.createStepID, "updateStepID": k.updateStepID,
			"bindingCountField": k.bindingCountField, "bindingUpField": k.bindingUpField,
		} {
			if v == "" {
				t.Errorf("kind %q: %s is empty", k.object, name)
			}
		}
	}
}
