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

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
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
	if _, has := enabled["handoff"]; has {
		t.Error("enabling a bridge is not a hand-off: the secret is already set")
	}

	// Disabled with no secret needed: just how to enable it later.
	plain := apply(t, "kafkaReceiverUpdated", receiverStep("updateKafkaReceiver", receiverData("none", false)))
	if next, _ := plain["nextStep"].(string); !strings.Contains(next, "is disabled") {
		t.Errorf("disabled nextStep = %q", next)
	}
	if _, has := plain["handoff"]; has {
		t.Error("a disabled bridge with scheme none needs no hand-off")
	}
}

// A disabled authenticated bridge after an update is either a scheme switch (the
// new secret must be set out of band) or a plain disable of a credentialed bridge.
// The handler cannot tell which, so the hand-off lists the per-scheme attributes and
// covers both outcomes, and never claims a secret is required (SOL-156212 review).
func TestKafkaReceiverUpdated_DisabledAuthenticatedBridgeCoversSwitchAndPlainDisable(t *testing.T) {
	for scheme, wantFields := range kafkaSchemeSecretFields {
		t.Run(scheme, func(t *testing.T) {
			out := apply(t, "kafkaReceiverUpdated", receiverStep("updateKafkaReceiver", receiverData(scheme, false)))
			if v, has := out["secretRequired"]; has {
				t.Fatalf("secretRequired = %v: an update cannot know whether a secret is missing, so it must not be asserted", v)
			}
			fields, _ := out["secretFields"].([]string)
			if strings.Join(fields, ",") != strings.Join(wantFields, ",") {
				t.Errorf("secretFields = %v, want %v", fields, wantFields)
			}
			handoff, _ := out["handoff"].(string)
			for _, want := range []string{`"rx"`, `"vpn1"`, `"` + scheme + `"`, "is disabled", "If this call changed the scheme",
				"together", "do not ask for it in this chat", "only disabled the bridge, nothing more is needed", "stored credentials are kept"} {
				if !strings.Contains(handoff, want) {
					t.Errorf("handoff should contain %q:\n%s", want, handoff)
				}
			}
			if strings.Contains(handoff, "was created") {
				t.Errorf("an update's hand-off must not say the bridge was created:\n%s", handoff)
			}
			// The next step is conditional: an intentional disable must not be undone.
			next, _ := out["nextStep"].(string)
			for _, want := range []string{"Only if the bridge should run again", "update-kafka-receiver", `"enabled": true`, "get-kafka-receiver-status"} {
				if !strings.Contains(next, want) {
					t.Errorf("nextStep should contain %q:\n%s", want, next)
				}
			}
		})
	}
}

func TestKafkaBridgeWrite_MalformedResultsAreErrors(t *testing.T) {
	cases := map[string]map[string]map[string]any{
		"missing step":       {},
		"data not an object": {"createKafkaReceiver": {"data": "oops"}},
		"missing name":       {"createKafkaReceiver": {"data": map[string]any{"msgVpnName": "v"}}},
		"missing vpn":        {"createKafkaReceiver": {"data": map[string]any{"kafkaReceiverName": "r"}}},
		// The hand-off depends on the scheme and on enabled: a missing or mistyped
		// one must be an error, not "no secret needed".
		"missing scheme":      {"createKafkaReceiver": {"data": map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r", "enabled": false}}},
		"scheme not a string": {"createKafkaReceiver": {"data": map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r", "authenticationScheme": 5, "enabled": false}}},
		"missing enabled":     {"createKafkaReceiver": {"data": map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r", "authenticationScheme": "basic"}}},
		"enabled not a bool":  {"createKafkaReceiver": {"data": map[string]any{"msgVpnName": "v", "kafkaReceiverName": "r", "authenticationScheme": "basic", "enabled": "false"}}},
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

// The hand-off text names tools and the YAML names handlers and step IDs. All of
// that is free text on one side, so a rename on the other would leave an agent
// pointed at a tool that does not exist. Resolve every name a kind carries
// against the real embedded catalog.
func TestKafkaBridgeKinds_NamesResolveAgainstTheCatalog(t *testing.T) {
	loaded, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	byName := map[string]composite.CompositeTool{}
	for _, tl := range loaded {
		byName[tl.Name] = tl
	}
	readOnly := func(tl composite.CompositeTool) bool {
		return tl.Annotations.ReadOnly != nil && *tl.Annotations.ReadOnly
	}

	for _, k := range kafkaBridgeKinds {
		t.Run(k.object, func(t *testing.T) {
			for _, name := range []string{k.createTool, k.updateTool, k.statusTool} {
				if _, ok := byName[name]; !ok {
					t.Errorf("%q is named in the hand-off text but is not a tool in the catalog", name)
				}
			}
			if tl, ok := byName[k.statusTool]; ok && !readOnly(tl) {
				t.Errorf("%s should be a read-only tool", k.statusTool)
			}
			for _, w := range []struct{ tool, step, handler string }{
				{k.createTool, k.createStepID, k.object + "Created"},
				{k.updateTool, k.updateStepID, k.object + "Updated"},
			} {
				tl, ok := byName[w.tool]
				if !ok {
					continue
				}
				if readOnly(tl) {
					t.Errorf("%s should be a write tool", w.tool)
				}
				if len(tl.Steps) != 1 || tl.Steps[0].ID != w.step {
					t.Errorf("%s: step IDs = %v, the handler reads step %q", w.tool, tl.Steps, w.step)
				}
				if tl.Result.PostProcess != w.handler {
					t.Errorf("%s: postProcess = %q, want %q", w.tool, tl.Result.PostProcess, w.handler)
				}
			}
			update := byName[k.updateTool]
			found := false
			for _, p := range update.Parameters {
				found = found || p.Name == k.configParam
			}
			if !found {
				t.Errorf("%s has no %q parameter, which the hand-off text tells the agent to send", k.updateTool, k.configParam)
			}
		})
	}
}
