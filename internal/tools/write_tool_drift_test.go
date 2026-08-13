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
