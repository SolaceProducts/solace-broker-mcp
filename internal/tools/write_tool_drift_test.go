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
	"sort"
	"strings"
	"testing"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite"
	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/definitions"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2/specs"
)

// SOL-152947 (A2 for write tools): drift negatives. Feeds mocked create-queue
// responses with an unexpected field, a missing (required) field, and a
// type-changed field through ValidateOutput against the real, wired schema —
// the same schema CompositeToolHandler.Metadata() actually returns for this
// tool — and asserts each is rejected. Complements the positive e2e-management
// round-trip (which proves real responses validate) by proving the negative
// side: that this schema isn't accidentally permissive.
func writeToolDriftTestSetup(t *testing.T) map[string]any {
	t.Helper()
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	var tool composite.CompositeTool
	found := false
	for _, tl := range realTools {
		if tl.Name == "create-queue" {
			tool = tl
			found = true
			break
		}
	}
	if !found {
		t.Fatal("create-queue not found in the real catalog")
	}
	executor := composite.NewCompositeExecutor(operations)
	handler := NewCompositeToolHandler(tool, executor)
	return handler.Metadata().OutputSchema
}

func TestWriteToolDrift_ValidResponseAccepted(t *testing.T) {
	schema := writeToolDriftTestSetup(t)
	result := map[string]any{
		"createQueue": map[string]any{
			"data": map[string]any{
				"msgVpnName":       "default",
				"queueName":        "orders",
				"accessType":       "exclusive",
				"egressEnabled":    true,
				"maxMsgSpoolUsage": float64(2000),
			},
			"meta": map[string]any{"responseCode": float64(200)},
		},
	}
	if err := ValidateOutput(result, schema); err != nil {
		t.Errorf("expected a real-shaped response to validate, got: %v", err)
	}
}

func TestWriteToolDrift_UnexpectedFieldRejected(t *testing.T) {
	schema := writeToolDriftTestSetup(t)
	result := map[string]any{
		"createQueue": map[string]any{
			"data": map[string]any{
				"msgVpnName": "default",
				"queueName":  "orders",
				// aBrandNewAttribute simulates a future SEMP release adding an
				// attribute the embedded spec (and this generated schema)
				// doesn't know about yet.
				"aBrandNewAttribute": "surprise",
			},
			"meta": map[string]any{},
		},
	}
	if err := ValidateOutput(result, schema); err == nil {
		t.Error("expected rejection for a field not in the spec's response schema, got nil error")
	}
}

func TestWriteToolDrift_MissingIdentifierRejected(t *testing.T) {
	schema := writeToolDriftTestSetup(t)
	result := map[string]any{
		"createQueue": map[string]any{
			// queueName (the resource's own identifier) is missing — simulates
			// a spec/broker regression where a create response no longer names
			// the resource it just created.
			"data": map[string]any{
				"msgVpnName": "default",
				"accessType": "exclusive",
			},
			"meta": map[string]any{},
		},
	}
	if err := ValidateOutput(result, schema); err == nil {
		t.Error("expected rejection when the required identifier field is missing, got nil error")
	}
}

func TestWriteToolDrift_TypeChangedFieldRejected(t *testing.T) {
	schema := writeToolDriftTestSetup(t)
	result := map[string]any{
		"createQueue": map[string]any{
			"data": map[string]any{
				"msgVpnName": "default",
				"queueName":  "orders",
				// egressEnabled is a boolean per the spec; simulate a spec
				// change that reshapes it to a string enum instead.
				"egressEnabled": "enabled",
			},
			"meta": map[string]any{},
		},
	}
	if err := ValidateOutput(result, schema); err == nil {
		t.Error("expected rejection when a field's type no longer matches the spec, got nil error")
	}
}

// TestWriteToolDrift_MissingEnvelopeDataRejected covers the envelope-level
// drift this ticket's e2e checkpoint actually found during development: a
// create/update response with no "data" key at all (e.g. the broker only
// returned meta, or a future SEMP change drops the echoed resource).
func TestWriteToolDrift_MissingEnvelopeDataRejected(t *testing.T) {
	schema := writeToolDriftTestSetup(t)
	result := map[string]any{
		"createQueue": map[string]any{
			"meta": map[string]any{"responseCode": float64(200)},
		},
	}
	if err := ValidateOutput(result, schema); err == nil {
		t.Error("expected rejection when the envelope's own required 'data' key is missing, got nil error")
	}
}

// TestWriteToolIdentifierFields_ResolveAgainstRealResponseFields guards an
// invariant composite.BuildStrictOutputSchema relies on but never checks
// itself: every field name listed in writeToolIdentifierFields must actually
// appear in its operation's resolved ResponseFields.
//
// composite.fieldPropertiesSchema builds "properties" from ResponseFields and
// "required" from this map independently. If the two ever disagree — a typo
// here, a renamed field in a future spec bump, or a new write tool added with
// the wrong identifier name — the generated schema ends up requiring a field
// that additionalProperties:false simultaneously forbids from appearing
// outside "properties". That schema is self-contradictory: no real broker
// response could ever satisfy it, and the tool's output validation breaks for
// every caller, silently, until someone notices in production. This test
// catches that at CI time instead, against the real embedded catalog.
func TestWriteToolIdentifierFields_ResolveAgainstRealResponseFields(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	toolByName := make(map[string]composite.CompositeTool, len(realTools))
	for _, tl := range realTools {
		toolByName[tl.Name] = tl
	}

	for name, identifierFields := range writeToolIdentifierFields {
		tool, ok := toolByName[name]
		if !ok {
			t.Errorf("%s: listed in writeToolIdentifierFields but not found in the real catalog", name)
			continue
		}
		if len(tool.Steps) == 0 {
			t.Errorf("%s: no steps", name)
			continue
		}
		op, ok := operations[tool.Steps[0].Operation]
		if !ok {
			t.Errorf("%s: operation %q not found in the real specs", name, tool.Steps[0].Operation)
			continue
		}
		for _, field := range identifierFields {
			if _, present := op.ResponseFields[field]; !present {
				t.Errorf("%s: identifier field %q is not in operation %q's resolved ResponseFields (%v) — "+
					"this would make BuildStrictOutputSchema produce a schema that requires %q while "+
					"additionalProperties:false forbids it, which no real response could ever satisfy",
					name, field, op.ID, op.ResponseFields, field)
			}
		}
	}
}

// TestWriteToolIdentifierFields_CoversEveryEligibleWriteTool is the reverse of
// the test above: every write tool whose first step's operation resolves
// real ResponseFields (i.e. isn't a delete/action op falling back to
// SempMetaOnlyResponse) must have an entry in writeToolIdentifierFields, or
// its generated output schema silently drops the "the response must name its
// own resource" requirement every other create/update tool gets.
//
// Verified by mutation during review (SOL-153868 PR #374, bczoma): deleting
// create-queue-subscription's entry left `go test ./cmd/server/...
// ./internal/tools/... ./internal/composite/...` fully green — nothing else
// catches a missing entry. Mirrors the pattern in
// cmd/server/tool_authorization_coverage_test.go's
// TestEveryRegisteredToolIsGatedOrExempt for the authorization registry.
func TestWriteToolIdentifierFields_CoversEveryEligibleWriteTool(t *testing.T) {
	operations, err := sempv2.ParseSpecs(specs.FS)
	if err != nil {
		t.Fatalf("ParseSpecs: %v", err)
	}
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}

	var missing []string
	for _, tool := range realTools {
		if !callsConfigOrActionOperation(tool) || len(tool.Steps) == 0 {
			continue
		}
		op, ok := operations[tool.Steps[0].Operation]
		if !ok || op.ResponseFields == nil {
			continue // delete/action ops resolve no ResponseFields (SempMetaOnlyResponse) — correctly excluded
		}
		if _, ok := writeToolIdentifierFields[tool.Name]; !ok {
			missing = append(missing, tool.Name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("write tool(s) whose response resolves real data but have no writeToolIdentifierFields "+
			"entry: %v — add one, matching every other create/update tool, or their output schema silently "+
			"drops the requirement that the response names its own resource", missing)
	}
}

// TestWriteToolIdentifierFields_TargetsStepZero pins an assumption
// composite_handler.go's outputSchema() hard-codes: every writeToolIdentifierFields
// entry is attached to Steps[0]. True for every write tool today (none has a
// preflight step ahead of its write step), but SOL-153868 introduced the
// first preflight-then-list *shape* in this catalog (list-queue-subscriptions
// — read-only, not itself in this map). A future write tool built the same
// way would attach its identifier fields to the preflight step's schema
// instead of the write step's, silently. This fails loudly instead, the day
// a writeToolIdentifierFields entry's tool has its config/action step
// anywhere but index 0.
func TestWriteToolIdentifierFields_TargetsStepZero(t *testing.T) {
	realTools, err := composite.LoadTools(definitions.FS, "tools.yaml")
	if err != nil {
		t.Fatalf("LoadTools: %v", err)
	}
	byName := make(map[string]composite.CompositeTool, len(realTools))
	for _, tl := range realTools {
		byName[tl.Name] = tl
	}

	for name := range writeToolIdentifierFields {
		tool, ok := byName[name]
		if !ok || len(tool.Steps) == 0 {
			continue // reported by TestWriteToolIdentifierFields_ResolveAgainstRealResponseFields above
		}
		step := tool.Steps[0]
		if !strings.HasPrefix(step.Operation, "config/") && !strings.HasPrefix(step.Operation, "action/") {
			t.Errorf("%s: writeToolIdentifierFields attaches to step %q (operation %q), which "+
				"composite_handler.go's outputSchema() assumes is the write step — but it isn't a "+
				"config/action operation. If this tool has a preflight step ahead of its write step, "+
				"the identifier fields are being attached to the wrong step's schema",
				name, step.ID, step.Operation)
		}
	}
}
