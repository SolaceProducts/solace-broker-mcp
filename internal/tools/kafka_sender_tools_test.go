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

package tools

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SOL-156213: the Kafka Sender tools, driven through the real manager exactly as
// the receiver tools are in kafka_receiver_tools_test.go (same fixture).

func senderResponse(scheme string, enabled bool) *sempv2.Result {
	return &sempv2.Result{StatusCode: 200, Data: map[string]any{
		"data": map[string]any{
			"msgVpnName": "vpn1", "kafkaSenderName": "tx", "authenticationScheme": scheme,
			"bootstrapAddressList": "kafka:9092", "transportTlsEnabled": false, "enabled": enabled,
		},
		"meta": map[string]any{"responseCode": float64(200)},
	}}
}

func TestCreateKafkaSender_NoAuthMode_CreatesEnabledAndPointsAtStatus(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, false)
	client.responses["createMsgVpnKafkaSender"] = senderResponse("none", true)

	r := callKafka(t, mgr, "create-kafka-sender", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx",
		"kafkaSenderConfig": map[string]any{"bootstrapAddressList": "kafka:9092", "authenticationScheme": "none", "enabled": true},
	})
	s := summaryOf(t, r)
	if s["object"] != "kafkaSender" || s["name"] != "tx" || s["secretRequired"] != false {
		t.Errorf("summary = %v", s)
	}
	next, _ := s["nextStep"].(string)
	for _, want := range []string{"get-kafka-sender-status", "queueBindingUpCount", "queueBindingCount"} {
		if !strings.Contains(next, want) {
			t.Errorf("nextStep %q should contain %q", next, want)
		}
	}
	body, _ := client.callArgs[0]["body"].(map[string]any)
	if body["kafkaSenderName"] != "tx" || body["enabled"] != true {
		t.Errorf("body = %v", body)
	}
	if _, inArgs := client.callArgs[0]["kafkaSenderName"]; inArgs {
		t.Error("kafkaSenderName must travel in the body on create, not as a path arg")
	}
}

func TestCreateKafkaSender_AuthenticatedMode_CreatedDisabledWithHandoff(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, false)
	client.responses["createMsgVpnKafkaSender"] = senderResponse("scram", false)

	r := callKafka(t, mgr, "create-kafka-sender", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx",
		"kafkaSenderConfig": map[string]any{"authenticationScheme": "scram", "authenticationScramUsername": "kuser"},
	})
	s := summaryOf(t, r)
	if s["secretRequired"] != true {
		t.Fatalf("secretRequired = %v", s["secretRequired"])
	}
	handoff, _ := s["handoff"].(string)
	for _, want := range []string{"Kafka Sender", "authenticationScramUsername", "authenticationScramPassword", "Do not paste the secret into this chat"} {
		if !strings.Contains(handoff, want) {
			t.Errorf("handoff should contain %q:\n%s", want, handoff)
		}
	}
	if next, _ := s["nextStep"].(string); !strings.Contains(next, "update-kafka-sender") || !strings.Contains(next, "kafkaSenderConfig") {
		t.Errorf("nextStep = %q", next)
	}
}

// Every write-only secret, on both bridge kinds, on create and update, in the
// config object and as a top-level param: rejected before any broker call, the
// value never in the reply, the logs or the audit stream.
func TestKafkaBridgeTools_RejectEverySecretOnEveryTool(t *testing.T) {
	secrets := []string{
		"authenticationAwsMskIamSecretAccessKey", "authenticationBasicPassword", "authenticationClientCertContent",
		"authenticationClientCertPassword", "authenticationKerberosKeytabContent", "authenticationOauthClientSecret",
		"authenticationScramPassword",
	}
	kinds := []struct{ verbNoun, nameParam, configParam string }{
		{"kafka-receiver", "kafkaReceiverName", "kafkaReceiverConfig"},
		{"kafka-sender", "kafkaSenderName", "kafkaSenderConfig"},
	}
	for _, kind := range kinds {
		for _, verb := range []string{"create", "update"} {
			for _, secret := range secrets {
				for _, shape := range []string{"config object", "top-level"} {
					t.Run(verb+"-"+kind.verbNoun+"/"+shape+"/"+secret, func(t *testing.T) {
						mgr, client := kafkaBridgeManager(t, true)
						params := map[string]any{"msgVpnName": "vpn1", kind.nameParam: "x"}
						if shape == "config object" {
							params[kind.configParam] = map[string]any{secret: kafkaSecretSentinel}
						} else {
							params[kind.configParam] = map[string]any{}
							params[secret] = kafkaSecretSentinel
						}
						var r *mcp.CallToolResult
						records := captureAudit(t, slog.LevelDebug, func() { r = callKafka(t, mgr, verb+"-"+kind.verbNoun, params) })
						if !r.IsError {
							t.Fatal("expected the call to be rejected")
						}
						reply := callToolResultText(t, r, nil)
						if !strings.Contains(reply, `"`+secret+`"`) {
							t.Errorf("reply %q should name %s", reply, secret)
						}
						all, _ := json.Marshal(records)
						if strings.Contains(reply, kafkaSecretSentinel) || strings.Contains(string(all), kafkaSecretSentinel) {
							t.Errorf("secret value leaked into the reply or the logs/audit stream")
						}
						if len(client.calls) != 0 {
							t.Errorf("SEMP calls = %v, want none", client.calls)
						}
					})
				}
			}
		}
	}
}

func TestKafkaSenderTools_EnabledWithAuthenticationRules(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, false)
	r := callKafka(t, mgr, "create-kafka-sender", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx",
		"kafkaSenderConfig": map[string]any{"authenticationScheme": "scram", "enabled": true}})
	if !r.IsError || !strings.Contains(callToolResultText(t, r, nil), "must be created with enabled:false") {
		t.Errorf("create: result = %v", r.StructuredContent)
	}
	r = callKafka(t, mgr, "update-kafka-sender", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx",
		"kafkaSenderConfig": map[string]any{"authenticationScheme": "scram"}})
	if !r.IsError || !strings.Contains(callToolResultText(t, r, nil), "must also set enabled:false") {
		t.Errorf("update: result = %v", r.StructuredContent)
	}
	if len(client.calls) != 0 {
		t.Errorf("SEMP calls = %v, want none", client.calls)
	}

	client.responses["updateMsgVpnKafkaSender"] = senderResponse("scram", true)
	r = callKafka(t, mgr, "update-kafka-sender", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx", "kafkaSenderConfig": map[string]any{"enabled": true}})
	s := summaryOf(t, r)
	if next, _ := s["nextStep"].(string); !strings.Contains(next, "get-kafka-sender-status") {
		t.Errorf("enable nextStep = %q", next)
	}
	if client.callArgs[0]["kafkaSenderName"] != "tx" {
		t.Errorf("update must address the sender by path: args = %v", client.callArgs[0])
	}
}

func TestCreateKafkaSender_AlreadyExists_CarriesTheSchemeHint(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, false)
	client.errors["createMsgVpnKafkaSender"] = &sempv2.SEMPError{
		Operation: "createMsgVpnKafkaSender", StatusCode: 400, SEMPCode: 10, SEMPStatus: "ALREADY_EXISTS",
		Description: "Problem with POST: Kafka Sender tx already exists.",
	}
	r := callKafka(t, mgr, "create-kafka-sender", map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx"})
	m := structured(t, r)
	if r.IsError || m["outcome"] != "already_exists" {
		t.Fatalf("result = %v", r.StructuredContent)
	}
	msg, _ := m["message"].(string)
	for _, want := range []string{"already exists", "get-kafka-sender-status", "authenticationScheme", "not modified"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "get-kafka-receiver-status") {
		t.Errorf("a sender's hint must not point at the receiver tool: %q", msg)
	}
}

// The queue travels in the body on create and in the path on delete; the sender
// is in the path both times.
func TestKafkaSenderQueueBinding_CreateAndDelete(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, false)
	client.responses["createMsgVpnKafkaSenderQueueBinding"] = &sempv2.Result{StatusCode: 200, Data: map[string]any{
		"data": map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx", "queueName": "q1", "remoteTopic": "out-topic", "enabled": true},
		"meta": map[string]any{"responseCode": float64(200)},
	}}
	r := callKafka(t, mgr, "create-kafka-sender-queue-binding", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx", "queueName": "q1",
		"queueBindingConfig": map[string]any{"remoteTopic": "out-topic", "ackMode": "all", "enabled": true},
	})
	if r.IsError {
		t.Fatalf("create binding failed: %v", r.StructuredContent)
	}
	create := client.callArgs[0]
	body, _ := create["body"].(map[string]any)
	if body["queueName"] != "q1" || body["remoteTopic"] != "out-topic" || body["ackMode"] != "all" || body["enabled"] != true {
		t.Errorf("create body = %v", body)
	}
	if create["kafkaSenderName"] != "tx" {
		t.Errorf("the sender is addressed by path on a binding create: args = %v", create)
	}
	if _, inArgs := create["queueName"]; inArgs {
		t.Error("queueName must travel in the body on create")
	}

	r = callKafka(t, mgr, "delete-kafka-sender-queue-binding", map[string]any{
		"msgVpnName": "vpn1", "kafkaSenderName": "tx", "queueName": "q1"})
	if r.IsError {
		t.Fatalf("delete binding failed: %v", r.StructuredContent)
	}
	del := client.callArgs[1]
	if del["queueName"] != "q1" || del["kafkaSenderName"] != "tx" {
		t.Errorf("delete addresses the binding by path: args = %v", del)
	}
}

func TestDeleteKafkaSender_BindingsPresent_BrokerRefusalReachesTheAgent(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, false)
	client.errors["deleteMsgVpnKafkaSender"] = &sempv2.SEMPError{
		Operation: "deleteMsgVpnKafkaSender", StatusCode: 400, SEMPCode: 2, SEMPStatus: "FAIL",
		Description: "Problem with DELETE: Cannot delete Kafka Sender when there are Kafka Sender Queue Bindings present.",
	}
	r := callKafka(t, mgr, "delete-kafka-sender", map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx"})
	if !r.IsError {
		t.Fatal("expected an error")
	}
	if reply := callToolResultText(t, r, nil); !strings.Contains(reply, "Cannot delete Kafka Sender when there are Kafka Sender Queue Bindings present") {
		t.Errorf("reply = %q, want the broker's refusal verbatim", reply)
	}
}

func TestKafkaSenderTools_AnnotationsAndAudit(t *testing.T) {
	mgr, client := kafkaBridgeManager(t, true)
	want := map[string]bool{
		"create-kafka-sender":               true,
		"update-kafka-sender":               true,
		"delete-kafka-sender":               true,
		"create-kafka-sender-queue-binding": false,
		"delete-kafka-sender-queue-binding": true,
	}
	for name, destructive := range want {
		h, err := mgr.Route(name)
		if err != nil {
			t.Fatalf("Route(%s): %v", name, err)
		}
		if a := h.Metadata().Annotations; a.ReadOnly || a.Destructive == nil || *a.Destructive != destructive {
			t.Errorf("%s: annotations = %+v, want destructive=%v and not read-only", name, a, destructive)
		}
	}

	client.responses["createMsgVpnKafkaSender"] = senderResponse("none", false)
	client.responses["createMsgVpnKafkaSenderQueueBinding"] = &sempv2.Result{StatusCode: 200, Data: map[string]any{
		"data": map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx", "queueName": "q1"},
		"meta": map[string]any{"responseCode": float64(200)},
	}}
	records := captureAudit(t, slog.LevelDebug, func() {
		callKafka(t, mgr, "create-kafka-sender", map[string]any{"msgVpnName": "vpn1", "kafkaSenderName": "tx"})
		callKafka(t, mgr, "create-kafka-sender-queue-binding", map[string]any{
			"msgVpnName": "vpn1", "kafkaSenderName": "tx", "queueName": "q1"})
	})
	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 || ops[0]["tool"] != "create-kafka-sender" {
		t.Fatalf("operation records = %v, want exactly one, for create-kafka-sender", ops)
	}
}

func TestKafkaSenderOperations_DeclareTheSelectQueryParameter(t *testing.T) {
	operations, _ := kafkaBridgeFixture(t)
	for _, id := range []string{"config/createMsgVpnKafkaSender", "config/updateMsgVpnKafkaSender"} {
		op, ok := operations[id]
		if !ok {
			t.Fatalf("%s missing from the embedded spec", id)
		}
		found := false
		for _, p := range op.Parameters {
			found = found || (p.Name == "select" && p.In == "query")
		}
		if !found {
			t.Errorf("%s does not declare a `select` query parameter", id)
		}
	}
}
