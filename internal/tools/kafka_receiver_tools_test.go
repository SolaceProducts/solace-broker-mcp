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
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	_ "github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess/handlers" // register the kafka bridge postProcess handlers, as cmd/server does
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/audit"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests drive the real Kafka Receiver tool definitions (SOL-156212) through
// ToolManager.CallTool against the real embedded SEMP spec, with a mock SEMP
// client behind them. The input-schema validation, body checks, postProcess
// handler, strict output-schema validation, desired-state outcomes and audit
// all run for real; only the broker is faked.

const kafkaSecretSentinel = "SECRET-SENTINEL-a91f"

// fixedClientHandler runs a real tool handler but with a fixed SEMP client, so
// the manager's own annotations, schemas and error mapping are the production ones.
type fixedClientHandler struct {
	ToolHandler
	client sempv2.Client
}

func (h fixedClientHandler) Handle(ctx context.Context, tc *ToolContext, params map[string]any) (*ToolResult, error) {
	return h.ToolHandler.Handle(ctx, &ToolContext{SEMPv1Client: tc.SEMPv1Client, SEMPv2Client: h.client}, params)
}

func kafkaReceiverManager(t *testing.T, auditOn bool) (*ToolManager, *mockClient) {
	t.Helper()
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	catalog, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	executor := composite.NewCompositeExecutor(operations)
	client := newMockClient()
	mgr := NewToolManager(newTestPool(t), WithAuditLog(auditOn))
	registered := 0
	for _, tl := range catalog {
		if strings.HasSuffix(tl.Name, "-kafka-receiver") || strings.HasSuffix(tl.Name, "-kafka-receiver-topic-binding") {
			mgr.Register(fixedClientHandler{NewCompositeToolHandler(tl, executor), client})
			registered++
		}
	}
	if registered != 5 {
		t.Fatalf("registered %d Kafka Receiver tools, want 5", registered)
	}
	return mgr, client
}

func structured(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	m, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %T, want a map", r.StructuredContent)
	}
	return m
}

func summaryOf(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	if r.IsError {
		t.Fatalf("call failed: %v", r.StructuredContent)
	}
	s, ok := structured(t, r)["summary"].(map[string]any)
	if !ok {
		t.Fatalf("no summary in %v", r.StructuredContent)
	}
	return s
}

func receiverResponse(scheme string, enabled bool) *sempv2.Result {
	return &sempv2.Result{StatusCode: 200, Data: map[string]any{
		"data": map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "authenticationScheme": scheme,
			"bootstrapAddressList": "kafka:9092", "transportTlsEnabled": false, "enabled": enabled,
		},
		"meta": map[string]any{"responseCode": float64(200)},
	}}
}

func callKafka(t *testing.T, mgr *ToolManager, tool string, params map[string]any) *mcp.CallToolResult {
	t.Helper()
	args := map[string]any{"broker": "dev"}
	for k, v := range params {
		args[k] = v
	}
	result, err := mgr.CallTool(auditCtx(t), tool, args, idFixture())
	if err != nil {
		t.Fatalf("CallTool(%s): protocol error: %v", tool, err)
	}
	return result
}

func TestCreateKafkaReceiver_NoAuthMode_CreatesEnabledAndPointsAtStatus(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.responses["createMsgVpnKafkaReceiver"] = receiverResponse("none", true)

	r := callKafka(t, mgr, "create-kafka-receiver", map[string]any{
		"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
		"kafkaReceiverConfig": map[string]any{"bootstrapAddressList": "kafka:9092", "authenticationScheme": "none", "enabled": true},
	})
	s := summaryOf(t, r)
	if s["secretRequired"] != false || s["enabled"] != true || s["name"] != "rx" {
		t.Errorf("summary = %v", s)
	}
	if next, _ := s["nextStep"].(string); !strings.Contains(next, "get-kafka-receiver-status") {
		t.Errorf("nextStep = %q, want it to point at the status tool", next)
	}

	// The SEMP call carries the name in the body (create) and asks for a small response.
	if len(client.callArgs) != 1 {
		t.Fatalf("SEMP calls = %d, want 1", len(client.callArgs))
	}
	args := client.callArgs[0]
	body, _ := args["body"].(map[string]any)
	if body["kafkaReceiverName"] != "rx" || body["enabled"] != true || body["bootstrapAddressList"] != "kafka:9092" {
		t.Errorf("body = %v", body)
	}
	if _, inArgs := args["kafkaReceiverName"]; inArgs {
		t.Error("kafkaReceiverName must travel in the body on create, not as a path arg")
	}
	if sel, _ := args["select"].(string); !strings.Contains(sel, "authenticationScheme") || !strings.Contains(sel, "kafkaReceiverName") {
		t.Errorf("select = %q, want the identifier and handler fields", sel)
	}
}

func TestCreateKafkaReceiver_AuthenticatedMode_CreatedDisabledWithHandoff(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.responses["createMsgVpnKafkaReceiver"] = receiverResponse("basic", false)

	r := callKafka(t, mgr, "create-kafka-receiver", map[string]any{
		"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
		"kafkaReceiverConfig": map[string]any{"authenticationScheme": "basic", "authenticationBasicUsername": "kuser", "bootstrapAddressList": "kafka:9094"},
	})
	s := summaryOf(t, r)
	if s["secretRequired"] != true {
		t.Fatalf("secretRequired = %v, want true", s["secretRequired"])
	}
	handoff, _ := s["handoff"].(string)
	for _, want := range []string{"authenticationBasicUsername", "authenticationBasicPassword", "Do not paste the secret into this chat", `"rx"`} {
		if !strings.Contains(handoff, want) {
			t.Errorf("handoff should contain %q:\n%s", want, handoff)
		}
	}
	body, _ := client.callArgs[0]["body"].(map[string]any)
	if body["authenticationBasicUsername"] != "kuser" {
		t.Errorf("the non-secret username should reach the broker: body = %v", body)
	}
}

func TestKafkaReceiverTools_RejectSecretsBeforeAnyBrokerCall(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		params map[string]any
	}{
		{"create, config object", "create-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
			"kafkaReceiverConfig": map[string]any{"authenticationBasicPassword": kafkaSecretSentinel}}},
		{"create, top-level param", "create-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "authenticationScramPassword": kafkaSecretSentinel}},
		{"update, config object", "update-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
			"kafkaReceiverConfig": map[string]any{"authenticationOauthClientSecret": kafkaSecretSentinel}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr, client := kafkaReceiverManager(t, false)
			var r *mcp.CallToolResult
			records := captureAudit(t, slog.LevelDebug, func() { r = callKafka(t, mgr, tc.tool, tc.params) })
			if !r.IsError {
				t.Fatal("expected the call to be rejected")
			}
			reply := callToolResultText(t, r, nil)
			if !strings.Contains(reply, "is a secret and cannot be sent through this tool") || !strings.Contains(reply, "rotate") {
				t.Errorf("reply = %q", reply)
			}
			all, _ := json.Marshal(records)
			if strings.Contains(reply, kafkaSecretSentinel) || strings.Contains(string(all), kafkaSecretSentinel) {
				t.Errorf("secret value leaked into the reply or logs:\n%s\n%s", reply, all)
			}
			if len(client.calls) != 0 {
				t.Errorf("SEMP calls = %v, want none", client.calls)
			}
		})
	}
}

func TestKafkaReceiverTools_EnabledWithAuthenticationRules(t *testing.T) {
	t.Run("create enabled with a scheme is rejected", func(t *testing.T) {
		mgr, client := kafkaReceiverManager(t, false)
		r := callKafka(t, mgr, "create-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
			"kafkaReceiverConfig": map[string]any{"authenticationScheme": "basic", "enabled": true}})
		if !r.IsError || !strings.Contains(callToolResultText(t, r, nil), "must be created with enabled:false") {
			t.Errorf("result = %v", r.StructuredContent)
		}
		if len(client.calls) != 0 {
			t.Errorf("SEMP calls = %v, want none", client.calls)
		}
	})
	t.Run("update that switches scheme without disabling is rejected", func(t *testing.T) {
		mgr, client := kafkaReceiverManager(t, false)
		r := callKafka(t, mgr, "update-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
			"kafkaReceiverConfig": map[string]any{"authenticationScheme": "scram"}})
		if !r.IsError || !strings.Contains(callToolResultText(t, r, nil), "must also set enabled:false") {
			t.Errorf("result = %v", r.StructuredContent)
		}
		if len(client.calls) != 0 {
			t.Errorf("SEMP calls = %v, want none", client.calls)
		}
	})
	t.Run("update that switches scheme and disables is accepted", func(t *testing.T) {
		mgr, client := kafkaReceiverManager(t, false)
		client.responses["updateMsgVpnKafkaReceiver"] = receiverResponse("scram", false)
		r := callKafka(t, mgr, "update-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
			"kafkaReceiverConfig": map[string]any{"authenticationScheme": "scram", "enabled": false}})
		_ = summaryOf(t, r)
		if len(client.calls) != 1 {
			t.Errorf("SEMP calls = %v, want 1", client.calls)
		}
	})
	t.Run("the plain enable step is accepted and tells the agent how to verify", func(t *testing.T) {
		mgr, client := kafkaReceiverManager(t, false)
		client.responses["updateMsgVpnKafkaReceiver"] = receiverResponse("basic", true)
		r := callKafka(t, mgr, "update-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
			"kafkaReceiverConfig": map[string]any{"enabled": true}})
		s := summaryOf(t, r)
		if next, _ := s["nextStep"].(string); !strings.Contains(next, "get-kafka-receiver-status") || !strings.Contains(next, "10 to 40 seconds") {
			t.Errorf("nextStep = %q", next)
		}
		args := client.callArgs[0]
		if args["kafkaReceiverName"] != "rx" || args["msgVpnName"] != "vpn1" {
			t.Errorf("update must address the receiver by path: args = %v", args)
		}
	})
}

// A duplicate create skips the postProcess handler, so the already_exists outcome
// itself must carry the "check the scheme before enabling" guidance (D3).
func TestCreateKafkaReceiver_AlreadyExists_CarriesTheSchemeHint(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.errors["createMsgVpnKafkaReceiver"] = &sempv2.SEMPError{
		Operation: "createMsgVpnKafkaReceiver", StatusCode: 400, SEMPCode: 10, SEMPStatus: "ALREADY_EXISTS",
		Description: "Problem with POST: Kafka Receiver rx already exists.",
	}
	r := callKafka(t, mgr, "create-kafka-receiver", map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx"})
	if r.IsError {
		t.Fatalf("already_exists must not be an error: %v", r.StructuredContent)
	}
	m := structured(t, r)
	if m["outcome"] != "already_exists" || m["changed"] != false {
		t.Fatalf("result = %v", m)
	}
	msg, _ := m["message"].(string)
	for _, want := range []string{"already exists", "get-kafka-receiver-status", "authenticationScheme", "not modified"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q should contain %q", msg, want)
		}
	}
}

func TestAlreadyExistsHint_OnlyForToolsThatHaveOne(t *testing.T) {
	absent := &desiredStateOutcome{Outcome: DesiredStateAlreadyAbsent, Message: "gone"}
	applyAlreadyExistsHint("create-kafka-receiver", absent)
	if absent.Message != "gone" {
		t.Errorf("already_absent must never get a hint, got %q", absent.Message)
	}
	other := &desiredStateOutcome{Outcome: DesiredStateAlreadyExists, Message: "exists"}
	applyAlreadyExistsHint("create-queue", other)
	if other.Message != "exists" {
		t.Errorf("a tool with no hint must be unchanged, got %q", other.Message)
	}
}

func TestKafkaReceiverBinding_CreateAndDelete(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.responses["createMsgVpnKafkaReceiverTopicBinding"] = &sempv2.Result{StatusCode: 200, Data: map[string]any{
		"data": map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "topicName": "^orders.*", "localTopic": "kafka/orders", "enabled": true},
		"meta": map[string]any{"responseCode": float64(200)},
	}}
	r := callKafka(t, mgr, "create-kafka-receiver-topic-binding", map[string]any{
		"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "topicName": "^orders.*",
		"topicBindingConfig": map[string]any{"localTopic": "kafka/orders", "enabled": true, "initialOffset": "beginning"},
	})
	if r.IsError {
		t.Fatalf("create binding failed: %v", r.StructuredContent)
	}
	create := client.callArgs[0]
	body, _ := create["body"].(map[string]any)
	if body["topicName"] != "^orders.*" || body["localTopic"] != "kafka/orders" || body["enabled"] != true {
		t.Errorf("create body = %v", body)
	}
	if create["kafkaReceiverName"] != "rx" {
		t.Errorf("the receiver is addressed by path on a binding create: args = %v", create)
	}

	r = callKafka(t, mgr, "delete-kafka-receiver-topic-binding", map[string]any{
		"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "topicName": "^orders.*"})
	if r.IsError {
		t.Fatalf("delete binding failed: %v", r.StructuredContent)
	}
	del := client.callArgs[1]
	if del["topicName"] != "^orders.*" {
		t.Errorf("delete passes the regex name unescaped to the client (which encodes the path): args = %v", del)
	}
}

func TestDeleteKafkaReceiver_BindingsPresent_BrokerRefusalReachesTheAgent(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.errors["deleteMsgVpnKafkaReceiver"] = &sempv2.SEMPError{
		Operation: "deleteMsgVpnKafkaReceiver", StatusCode: 400, SEMPCode: 2, SEMPStatus: "FAIL",
		Description: "Problem with DELETE: Cannot delete Kafka Receiver when there are Kafka Receiver Topic Bindings present.",
	}
	r := callKafka(t, mgr, "delete-kafka-receiver", map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx"})
	if !r.IsError {
		t.Fatal("expected an error")
	}
	if reply := callToolResultText(t, r, nil); !strings.Contains(reply, "Cannot delete Kafka Receiver when there are Kafka Receiver Topic Bindings present") {
		t.Errorf("reply = %q, want the broker's refusal verbatim so the agent knows to delete the bindings first", reply)
	}
}

func TestDeleteKafkaReceiver_AlreadyGone(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.errors["deleteMsgVpnKafkaReceiver"] = &sempv2.SEMPError{
		Operation: "deleteMsgVpnKafkaReceiver", StatusCode: 400, SEMPCode: 6, SEMPStatus: "NOT_FOUND",
		Description: "Could not find match for kafkaReceiver rx",
	}
	r := callKafka(t, mgr, "delete-kafka-receiver", map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx"})
	if m := structured(t, r); r.IsError || m["outcome"] != "already_absent" {
		t.Errorf("result = %v", r.StructuredContent)
	}
}

// D2: create-kafka-receiver is deliberately destructive so it is audited; the
// topic-binding create keeps the create-* convention.
func TestKafkaReceiverTools_Annotations(t *testing.T) {
	mgr, _ := kafkaReceiverManager(t, false)
	want := map[string]bool{
		"create-kafka-receiver":               true,
		"update-kafka-receiver":               true,
		"delete-kafka-receiver":               true,
		"create-kafka-receiver-topic-binding": false,
		"delete-kafka-receiver-topic-binding": true,
	}
	for name, destructive := range want {
		h, err := mgr.Route(name)
		if err != nil {
			t.Fatalf("Route(%s): %v", name, err)
		}
		a := h.Metadata().Annotations
		if a.ReadOnly {
			t.Errorf("%s must not be read-only", name)
		}
		if a.Destructive == nil || *a.Destructive != destructive {
			t.Errorf("%s: destructive = %v, want %v", name, a.Destructive, destructive)
		}
	}
}

func TestCreateKafkaReceiver_IsAudited_BindingCreateIsNot(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, true)
	client.responses["createMsgVpnKafkaReceiver"] = receiverResponse("none", false)
	client.responses["createMsgVpnKafkaReceiverTopicBinding"] = &sempv2.Result{StatusCode: 200, Data: map[string]any{
		"data": map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "topicName": "t"},
		"meta": map[string]any{"responseCode": float64(200)},
	}}

	records := captureAudit(t, slog.LevelDebug, func() {
		callKafka(t, mgr, "create-kafka-receiver", map[string]any{"msgVpnName": "vpn1", "kafkaReceiverName": "rx"})
		callKafka(t, mgr, "create-kafka-receiver-topic-binding", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "topicName": "t"})
	})
	ops := ofType(records, audit.EventOperation)
	if len(ops) != 1 || ops[0]["tool"] != "create-kafka-receiver" || ops[0]["outcome"] != string(audit.OutcomeSuccess) {
		t.Fatalf("operation records = %v, want exactly one, for create-kafka-receiver, outcome success", ops)
	}
}

// The create/update steps ask the broker for a small response with `select`. The
// client only forwards an argument the operation declares, so pin that each
// operation we send `select` to declares it as a query parameter; otherwise the
// postProcess handler would silently receive the full echo (or nothing useful).
func TestKafkaReceiverOperations_DeclareTheSelectQueryParameter(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	for _, id := range []string{"config/createMsgVpnKafkaReceiver", "config/updateMsgVpnKafkaReceiver"} {
		op, ok := operations[id]
		if !ok {
			t.Fatalf("%s missing from the embedded spec", id)
		}
		found := false
		for _, p := range op.Parameters {
			if p.Name == "select" && p.In == "query" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s does not declare a `select` query parameter, so the step's select: would never reach the broker", id)
		}
	}
}

// A receiver that already holds credentials must not be re-pointed by an update:
// the broker would present them to the new destination. Through the real manager,
// the call is rejected before any broker call and says how to do it properly.
func TestUpdateKafkaReceiver_CannotRetargetAnExistingReceiver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config map[string]any
		want   string
	}{
		{"bootstrap address", map[string]any{"bootstrapAddressList": "elsewhere:9092"}, `"bootstrapAddressList"`},
		{"oauth token endpoint", map[string]any{"authenticationOauthClientTokenEndpoint": "https://elsewhere/token"}, `"authenticationOauthClientTokenEndpoint"`},
		{"TLS off", map[string]any{"transportTlsEnabled": false}, "transportTlsEnabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, client := kafkaReceiverManager(t, false)
			r := callKafka(t, mgr, "update-kafka-receiver", map[string]any{
				"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "kafkaReceiverConfig": tc.config})
			if !r.IsError {
				t.Fatal("expected the update to be rejected")
			}
			reply := callToolResultText(t, r, nil)
			for _, want := range []string{tc.want, "stored credentials", "create it again"} {
				if !strings.Contains(reply, want) {
					t.Errorf("reply %q should contain %q", reply, want)
				}
			}
			if len(client.calls) != 0 {
				t.Errorf("SEMP calls = %v, want none", client.calls)
			}
		})
	}

	// TLS on, and the plain enable step, are still fine.
	mgr, client := kafkaReceiverManager(t, false)
	client.responses["updateMsgVpnKafkaReceiver"] = receiverResponse("none", true)
	for _, config := range []map[string]any{{"transportTlsEnabled": true}, {"enabled": true}} {
		if r := callKafka(t, mgr, "update-kafka-receiver", map[string]any{
			"msgVpnName": "vpn1", "kafkaReceiverName": "rx", "kafkaReceiverConfig": config}); r.IsError {
			t.Errorf("update with %v should be accepted: %v", config, r.StructuredContent)
		}
	}
}

// Creating a receiver is how a new destination is chosen, so the create tool still takes it.
func TestCreateKafkaReceiver_StillTakesTheBootstrapAddress(t *testing.T) {
	mgr, client := kafkaReceiverManager(t, false)
	client.responses["createMsgVpnKafkaReceiver"] = receiverResponse("none", false)
	r := callKafka(t, mgr, "create-kafka-receiver", map[string]any{
		"msgVpnName": "vpn1", "kafkaReceiverName": "rx",
		"kafkaReceiverConfig": map[string]any{"bootstrapAddressList": "kafka:9092", "transportTlsEnabled": false}})
	if r.IsError {
		t.Fatalf("create should be accepted: %v", r.StructuredContent)
	}
}

// alreadyExistsHints is keyed by tool name. A rename would silently drop the hint
// the agent relies on after a duplicate create (the postProcess handler is skipped
// then), so every key must be a real create tool whose step can produce
// already_exists, and every Kafka bridge create tool must have a hint.
func TestAlreadyExistsHints_AreKeyedByRealKafkaCreateTools(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	catalog, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	byName := map[string]composite.CompositeTool{}
	for _, tl := range catalog {
		byName[tl.Name] = tl
	}
	for name := range alreadyExistsHints {
		tl, ok := byName[name]
		if !ok {
			t.Errorf("alreadyExistsHints key %q is not a tool in the catalog", name)
			continue
		}
		if len(tl.Steps) == 0 || !strings.HasPrefix(operations[tl.Steps[0].Operation].ID, "create") {
			t.Errorf("%s: its first step is not a create operation, so it can never produce already_exists", name)
		}
	}
	for _, tl := range catalog {
		if (tl.Name == "create-kafka-receiver" || tl.Name == "create-kafka-sender") && alreadyExistsHints[tl.Name] == "" {
			t.Errorf("%s has no alreadyExistsHints entry; a duplicate create would lose its hand-off guidance", tl.Name)
		}
	}
}
