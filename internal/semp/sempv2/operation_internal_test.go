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

package sempv2

import (
	"testing"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi3"
)

// These test the unexported branches TestParseSpecs_ResponseFields (the
// external, real-spec-backed test) can't reach: real embedded specs never
// leave a $ref dangling or a "data" array with no items, so those defensive
// bail-out paths in extractResponseFields need synthetic input to exercise
// at all. A drift-guard's own foundation silently mishandling malformed
// input would be exactly the kind of unexercised branch this ticket exists
// to catch elsewhere.

func TestResolveSchemaRef_NotFoundReturnsUnchanged(t *testing.T) {
	ref := &openapi2.SchemaRef{Ref: "#/definitions/DoesNotExist"}
	got := resolveSchemaRef(ref, map[string]*openapi2.SchemaRef{})
	if got != ref {
		t.Errorf("resolveSchemaRef with an unresolvable ref should return the input unchanged, got a different pointer")
	}
}

func TestResolveSchemaRef_InlineUnchanged(t *testing.T) {
	ref := &openapi2.SchemaRef{Value: &openapi2.Schema{}}
	got := resolveSchemaRef(ref, nil)
	if got != ref {
		t.Error("resolveSchemaRef on an already-inline schema (no Ref) should return it unchanged")
	}
}

func TestSchemaTypeName(t *testing.T) {
	tests := []struct {
		name string
		s    *openapi2.Schema
		want string
	}{
		{"nil schema", nil, "object"},
		{"nil Type", &openapi2.Schema{}, "object"},
		{"empty Type slice", &openapi2.Schema{Type: &openapi3.Types{}}, "object"},
		{"string", &openapi2.Schema{Type: &openapi3.Types{"string"}}, "string"},
		{"integer", &openapi2.Schema{Type: &openapi3.Types{"integer"}}, "integer"},
		{"array", &openapi2.Schema{Type: &openapi3.Types{"array"}}, "array"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := schemaTypeName(tc.s); got != tc.want {
				t.Errorf("schemaTypeName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// msgVpnQueueResponseFixture returns a minimal, realistic envelope schema —
// {"data": $ref to item} — plus the definitions map it $refs into. Each test
// mutates a copy to break exactly one resolution step.
func msgVpnQueueResponseFixture() (*openapi2.SchemaRef, map[string]*openapi2.SchemaRef) {
	item := &openapi2.SchemaRef{
		Value: &openapi2.Schema{
			Properties: openapi2.Schemas{
				"queueName": {Value: &openapi2.Schema{Type: &openapi3.Types{"string"}}},
			},
		},
	}
	definitions := map[string]*openapi2.SchemaRef{
		"Item":     item,
		"Envelope": {Value: &openapi2.Schema{Properties: openapi2.Schemas{"data": {Ref: "#/definitions/Item"}}}},
	}
	envelopeRef := &openapi2.SchemaRef{Ref: "#/definitions/Envelope"}
	return envelopeRef, definitions
}

func opWith200Schema(schema *openapi2.SchemaRef) *openapi2.Operation {
	return &openapi2.Operation{
		Responses: map[string]*openapi2.Response{
			"200": {Schema: schema},
		},
	}
}

func TestExtractResponseFields_NoResponses(t *testing.T) {
	op := &openapi2.Operation{Responses: map[string]*openapi2.Response{}}
	if got := extractResponseFields(op, nil); got != nil {
		t.Errorf("extractResponseFields with no 200 response = %v, want nil", got)
	}
}

func TestExtractResponseFields_NilSchema(t *testing.T) {
	op := opWith200Schema(nil)
	if got := extractResponseFields(op, nil); got != nil {
		t.Errorf("extractResponseFields with a nil 200 schema = %v, want nil", got)
	}
}

func TestExtractResponseFields_DanglingEnvelopeRef(t *testing.T) {
	op := opWith200Schema(&openapi2.SchemaRef{Ref: "#/definitions/Missing"})
	if got := extractResponseFields(op, map[string]*openapi2.SchemaRef{}); got != nil {
		t.Errorf("extractResponseFields with an unresolvable envelope ref = %v, want nil", got)
	}
}

func TestExtractResponseFields_DataRefDangling(t *testing.T) {
	envelope := &openapi2.SchemaRef{
		Value: &openapi2.Schema{
			Properties: openapi2.Schemas{
				"data": {Ref: "#/definitions/Missing"},
			},
		},
	}
	op := opWith200Schema(envelope)
	if got := extractResponseFields(op, map[string]*openapi2.SchemaRef{}); got != nil {
		t.Errorf("extractResponseFields with an unresolvable data ref = %v, want nil", got)
	}
}

func TestExtractResponseFields_ArrayWithNoItemsSchema(t *testing.T) {
	envelope := &openapi2.SchemaRef{
		Value: &openapi2.Schema{
			Properties: openapi2.Schemas{
				"data": {Value: &openapi2.Schema{Type: &openapi3.Types{"array"}}}, // Items deliberately unset
			},
		},
	}
	op := opWith200Schema(envelope)
	if got := extractResponseFields(op, nil); got != nil {
		t.Errorf("extractResponseFields with an array data field but no Items schema = %v, want nil", got)
	}
}

func TestExtractResponseFields_ArrayItemsDanglingRef(t *testing.T) {
	envelope := &openapi2.SchemaRef{
		Value: &openapi2.Schema{
			Properties: openapi2.Schemas{
				"data": {Value: &openapi2.Schema{
					Type:  &openapi3.Types{"array"},
					Items: &openapi2.SchemaRef{Ref: "#/definitions/Missing"},
				}},
			},
		},
	}
	op := opWith200Schema(envelope)
	if got := extractResponseFields(op, map[string]*openapi2.SchemaRef{}); got != nil {
		t.Errorf("extractResponseFields with an unresolvable array-items ref = %v, want nil", got)
	}
}

func TestExtractResponseFields_SingleObjectAndArrayAgree(t *testing.T) {
	envelopeRef, definitions := msgVpnQueueResponseFixture()

	// Single-object case: data is a direct $ref to the item.
	op := opWith200Schema(envelopeRef)
	got := extractResponseFields(op, definitions)
	if got["queueName"] != "string" {
		t.Fatalf("single-object case: ResponseFields = %v, want queueName:string", got)
	}

	// List case: data is an array whose items $ref the same item — must
	// resolve to the identical field set as the single-object case.
	listEnvelope := &openapi2.SchemaRef{
		Value: &openapi2.Schema{
			Properties: openapi2.Schemas{
				"data": {Value: &openapi2.Schema{
					Type:  &openapi3.Types{"array"},
					Items: &openapi2.SchemaRef{Ref: "#/definitions/Item"},
				}},
			},
		},
	}
	listOp := opWith200Schema(listEnvelope)
	gotList := extractResponseFields(listOp, definitions)
	if gotList["queueName"] != "string" {
		t.Errorf("list case: ResponseFields = %v, want queueName:string", gotList)
	}
}
