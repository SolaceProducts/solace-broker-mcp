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

package composite

import (
	"encoding/json"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
	"github.com/xeipuuv/gojsonschema"
)

func fakeOpWithResponseFields(fields map[string]string) *sempv2.Operation {
	return &sempv2.Operation{ResponseFields: fields}
}

func validateAgainst(t *testing.T, schema map[string]any, doc map[string]any) *gojsonschema.Result {
	t.Helper()
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	docJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal doc: %v", err)
	}
	result, err := gojsonschema.Validate(gojsonschema.NewBytesLoader(schemaJSON), gojsonschema.NewBytesLoader(docJSON))
	if err != nil {
		t.Fatalf("gojsonschema.Validate: %v", err)
	}
	return result
}

func TestBuildStrictOutputSchema_FlatStep(t *testing.T) {
	tool := CompositeTool{
		Steps:  []Step{{ID: "queue", Operation: "config/createMsgVpnQueue"}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	operations := map[string]*sempv2.Operation{
		"config/createMsgVpnQueue": fakeOpWithResponseFields(map[string]string{
			"queueName": "string",
			"bindCount": "integer",
		}),
	}
	schema := BuildStrictOutputSchema(tool, operations, map[string][]string{"queue": {"queueName"}})

	// A flat (non-paginated, non-fan-out) step's real runtime value is the
	// whole SEMP envelope verbatim — {"data": ..., "meta": {...}} — not the
	// item alone. See buildStepSchema's doc comment: this was confirmed
	// against a real broker via e2e-management, not assumed from the start.
	good := map[string]any{"queue": map[string]any{
		"data": map[string]any{"queueName": "q1", "bindCount": float64(3)},
		"meta": map[string]any{"responseCode": float64(200)},
	}}
	if r := validateAgainst(t, schema, good); !r.Valid() {
		t.Errorf("expected valid, got errors: %v", r.Errors())
	}

	missingEnvelopeData := map[string]any{"queue": map[string]any{"meta": map[string]any{}}}
	if r := validateAgainst(t, schema, missingEnvelopeData); r.Valid() {
		t.Error("expected rejection when the envelope's own required 'data' key is missing")
	}

	missingIdentifier := map[string]any{"queue": map[string]any{"data": map[string]any{"bindCount": float64(3)}}}
	if r := validateAgainst(t, schema, missingIdentifier); r.Valid() {
		t.Error("expected rejection when the required identifier field is missing")
	}

	unexpectedField := map[string]any{"queue": map[string]any{"data": map[string]any{"queueName": "q1", "somethingNew": "x"}}}
	if r := validateAgainst(t, schema, unexpectedField); r.Valid() {
		t.Error("expected rejection for a field not in ResponseFields (additionalProperties: false)")
	}

	typeChanged := map[string]any{"queue": map[string]any{"data": map[string]any{"queueName": "q1", "bindCount": "not-a-number"}}}
	if r := validateAgainst(t, schema, typeChanged); r.Valid() {
		t.Error("expected rejection when bindCount's type doesn't match the spec")
	}

	// Non-identifier fields are typed-if-present, not required — a broker
	// omitting bindCount must not fail validation.
	sparseButValid := map[string]any{"queue": map[string]any{"data": map[string]any{"queueName": "q1"}}}
	if r := validateAgainst(t, schema, sparseButValid); !r.Valid() {
		t.Errorf("expected valid when a non-identifier field is simply absent, got errors: %v", r.Errors())
	}
}

func TestBuildStrictOutputSchema_PaginatedStep(t *testing.T) {
	tool := CompositeTool{
		Steps:  []Step{{ID: "queues", Operation: "monitor/getMsgVpnQueues", FollowPages: true}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	operations := map[string]*sempv2.Operation{
		"monitor/getMsgVpnQueues": fakeOpWithResponseFields(map[string]string{"queueName": "string"}),
	}
	schema := BuildStrictOutputSchema(tool, operations, nil)

	good := map[string]any{"queues": map[string]any{
		"data":      []any{map[string]any{"queueName": "q1"}},
		"truncated": false,
	}}
	if r := validateAgainst(t, schema, good); !r.Valid() {
		t.Errorf("expected valid paginated envelope, got errors: %v", r.Errors())
	}

	badItem := map[string]any{"queues": map[string]any{
		"data":      []any{map[string]any{"queueName": "q1", "unexpected": true}},
		"truncated": false,
	}}
	if r := validateAgainst(t, schema, badItem); r.Valid() {
		t.Error("expected rejection for an unexpected field inside a paginated item")
	}

	missingTruncated := map[string]any{"queues": map[string]any{"data": []any{}}}
	if r := validateAgainst(t, schema, missingTruncated); r.Valid() {
		t.Error("expected rejection when the envelope's own required 'truncated' key is missing")
	}
}

func TestBuildStrictOutputSchema_FanOutStep(t *testing.T) {
	tool := CompositeTool{
		Steps:  []Step{{ID: "clients", Operation: "monitor/getMsgVpnClients", ForEach: "vpns", ForEachKey: "msgVpnName"}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	operations := map[string]*sempv2.Operation{
		"monitor/getMsgVpnClients": fakeOpWithResponseFields(map[string]string{"clientName": "string"}),
	}
	schema := BuildStrictOutputSchema(tool, operations, nil)

	// A fan-out step's per-key value is runSingle's return of result.Data
	// verbatim — the same raw SEMP envelope {"data": ..., "meta": {...}} the
	// flat (default) case wraps, not the item alone. This test originally
	// asserted the unwrapped item here and passed only because nothing
	// exercised the real shape; caught in review before any fan-out write
	// tool relied on it. See buildStepSchema's ForEach case comment.
	good := map[string]any{"clients": map[string]any{
		"byKey": map[string]any{"vpn-a": map[string]any{
			"data": map[string]any{"clientName": "c1"},
			"meta": map[string]any{"responseCode": float64(200)},
		}},
	}}
	if r := validateAgainst(t, schema, good); !r.Valid() {
		t.Errorf("expected valid fan-out envelope, got errors: %v", r.Errors())
	}

	badItem := map[string]any{"clients": map[string]any{
		"byKey": map[string]any{"vpn-a": map[string]any{
			"data": map[string]any{"clientName": "c1", "unexpected": true},
		}},
	}}
	if r := validateAgainst(t, schema, badItem); r.Valid() {
		t.Error("expected rejection for an unexpected field inside a fan-out item's data")
	}

	missingEnvelopeData := map[string]any{"clients": map[string]any{
		"byKey": map[string]any{"vpn-a": map[string]any{"meta": map[string]any{}}},
	}}
	if r := validateAgainst(t, schema, missingEnvelopeData); r.Valid() {
		t.Error("expected rejection when a fan-out item's envelope is missing its required 'data' key")
	}
}

// TestBuildStrictOutputSchema_ForEachCheckedBeforeFollowPages guards the
// switch order in buildStepSchema against a regression: runStep (executor.go)
// dispatches ForEach before FollowPages (fetchFanOut runs runSingle per row,
// which itself honors FollowPages), so a step with both flags set gets a
// byKey-wrapped result at runtime — the schema generator must check ForEach
// first too, or a doubly-flagged step gets the bare paginated shape instead.
// No shipped step sets both today; this only pins the ordering itself.
func TestBuildStrictOutputSchema_ForEachCheckedBeforeFollowPages(t *testing.T) {
	tool := CompositeTool{
		Steps: []Step{{
			ID: "clients", Operation: "monitor/getMsgVpnClients",
			ForEach: "vpns", ForEachKey: "msgVpnName", FollowPages: true,
		}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	operations := map[string]*sempv2.Operation{
		"monitor/getMsgVpnClients": fakeOpWithResponseFields(map[string]string{"clientName": "string"}),
	}
	schema := BuildStrictOutputSchema(tool, operations, nil)

	props := schema["properties"].(map[string]any)
	step := props["clients"].(map[string]any)
	stepProps := step["properties"].(map[string]any)
	if _, hasByKey := stepProps["byKey"]; !hasByKey {
		t.Errorf("expected the byKey wrapper (ForEach takes precedence, matching runStep's dispatch order), got properties: %v", stepProps)
	}
}

func TestBuildStrictOutputSchema_UnknownOperationFallsBackPermissive(t *testing.T) {
	tool := CompositeTool{
		Steps:  []Step{{ID: "mystery", Operation: "action/doSomethingUnresolvable"}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	schema := BuildStrictOutputSchema(tool, map[string]*sempv2.Operation{}, nil)

	anything := map[string]any{"mystery": map[string]any{"whatever": "goes", "here": 1}}
	if r := validateAgainst(t, schema, anything); !r.Valid() {
		t.Errorf("expected a step with no resolvable operation to stay fully permissive, got errors: %v", r.Errors())
	}
}

func TestBuildStrictOutputSchema_NilResponseFieldsFallsBackPermissive(t *testing.T) {
	tool := CompositeTool{
		Steps:  []Step{{ID: "del", Operation: "config/deleteMsgVpnQueue"}},
		Result: ResultStrategy{Strategy: "collect"},
	}
	operations := map[string]*sempv2.Operation{
		"config/deleteMsgVpnQueue": {ResponseFields: nil}, // meta-only response, exactly like a real delete op
	}
	schema := BuildStrictOutputSchema(tool, operations, nil)

	anything := map[string]any{"del": map[string]any{"meta": map[string]any{"responseCode": float64(200)}}}
	if r := validateAgainst(t, schema, anything); !r.Valid() {
		t.Errorf("expected a nil-ResponseFields step to stay fully permissive, got errors: %v", r.Errors())
	}
}

func TestBuildStrictOutputSchema_PostProcessAddsSummary(t *testing.T) {
	tool := CompositeTool{
		Steps:  []Step{{ID: "queues", Operation: "monitor/getMsgVpnQueues", FollowPages: true}},
		Result: ResultStrategy{Strategy: "postProcess", PostProcess: "listQueues"},
	}
	operations := map[string]*sempv2.Operation{
		"monitor/getMsgVpnQueues": fakeOpWithResponseFields(map[string]string{"queueName": "string"}),
	}
	schema := BuildStrictOutputSchema(tool, operations, nil)

	withoutSummary := map[string]any{"queues": map[string]any{"data": []any{}, "truncated": false}}
	if r := validateAgainst(t, schema, withoutSummary); r.Valid() {
		t.Error("expected rejection when a postProcess tool's response is missing 'summary'")
	}

	withSummary := map[string]any{
		"queues":  map[string]any{"data": []any{}, "truncated": false},
		"summary": map[string]any{"congestedCount": float64(0)},
	}
	if r := validateAgainst(t, schema, withSummary); !r.Valid() {
		t.Errorf("expected valid with summary present, got errors: %v", r.Errors())
	}
}

// TestBuildStrictOutputSchema_RealCreateQueue is an end-to-end smoke test
// against the real embedded spec and the real create-queue tool shape (one
// path param folded out of the body, the rest spread in) — proving the
// generator produces valid, usable JSON Schema from real data, not just the
// synthetic fixtures above.
func TestBuildStrictOutputSchema_RealCreateQueue(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	tools, err := LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	tool := findTool(tools, "create-queue")
	if tool == nil {
		t.Fatal("create-queue tool not found")
	}

	schema := BuildStrictOutputSchema(*tool, operations, map[string][]string{"createQueue": {"queueName"}})

	// bindCount is deliberately absent here: it's a runtime stat, not part of
	// config's MsgVpnQueue definition (confirmed directly — config's
	// create/update response echoes back configured state, not live stats,
	// unlike monitor's MsgVpnQueue which combines both). Asserting bindCount
	// as valid here would assert a real spec property that isn't actually
	// there — exactly the kind of wrong assumption this generator exists to
	// catch, and did catch while writing this test.
	// The runtime value is the whole SEMP envelope, not the item alone —
	// confirmed against a real broker via e2e-management. See
	// buildStepSchema's doc comment.
	good := map[string]any{"createQueue": map[string]any{
		"data": map[string]any{"queueName": "orders"},
	}}
	if r := validateAgainst(t, schema, good); !r.Valid() {
		t.Errorf("expected valid, got errors: %v", r.Errors())
	}

	driftedField := map[string]any{"createQueue": map[string]any{
		"data": map[string]any{"queueName": "orders", "notInTheSpec": "x"},
	}}
	if r := validateAgainst(t, schema, driftedField); r.Valid() {
		t.Error("expected rejection for a field the real spec doesn't declare")
	}
}

// TestFieldPropertiesSchema_DoesNotAliasCallerSlice is a regression test:
// fieldPropertiesSchema used to store the caller's "required" slice directly.
// Callers pass a slice sourced from a shared, reused value (e.g.
// writeToolIdentifierFields, a package-level map in internal/tools), so
// mutating a generated schema's "required" list used to corrupt that shared
// value for every future call to every handler — confirmed before fixing by
// mutating a real Metadata() call's returned schema and observing a
// brand-new handler instance return the corrupted value.
func TestFieldPropertiesSchema_DoesNotAliasCallerSlice(t *testing.T) {
	callerOwned := []string{"queueName"}
	schema := fieldPropertiesSchema(map[string]string{"queueName": "string"}, callerOwned)

	got := schema["required"].([]string)
	got[0] = "MUTATED"

	if callerOwned[0] != "queueName" {
		t.Fatalf("mutating the returned schema's required slice corrupted the caller's slice: %v", callerOwned)
	}
}
