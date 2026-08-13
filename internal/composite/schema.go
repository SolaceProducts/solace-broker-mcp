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

import "github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"

// BuildStrictOutputSchema generates a step-keyed JSON Schema for a composite
// tool's output, deriving each step's per-field schema from its operation's
// resolved response fields (sempv2.Operation.ResponseFields) instead of the
// generic permissive envelope (StepKeyedEnvelopeSchema) every composite tool
// used before SOL-152947. Fields are typed if present but not required,
// except any field named in requiredFields for that step ID — a broker
// legitimately omits other optional/feature-gated attributes, but a response
// that doesn't name its own resource is broken, not sparse.
//
// A step whose operation isn't found in operations, or whose ResponseFields
// is nil (no response data resolved — e.g. a delete/action op, or a response
// shape extractResponseFields couldn't unwrap), falls back to a fully
// permissive step schema. An unknown shape must not be mistaken for an empty
// one: rejecting every field on a step this couldn't introspect would be
// worse than the generic envelope it replaces.
func BuildStrictOutputSchema(tool CompositeTool, operations map[string]*sempv2.Operation, requiredFields map[string][]string) map[string]any {
	properties := make(map[string]any, len(tool.Steps)+1)
	required := make([]string, 0, len(tool.Steps)+1)

	for _, step := range tool.Steps {
		op := operations[step.Operation]
		properties[step.ID] = buildStepSchema(step, op, requiredFields[step.ID])
		required = append(required, step.ID)
	}

	if tool.Result.Strategy == "postProcess" {
		// summary is handler-computed (internal/composite/postprocess), not
		// spec-derived — there's no field list to derive strictness from.
		properties["summary"] = map[string]any{"type": "object"}
		required = append(required, "summary")
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

// buildStepSchema generates one step's schema, honoring the FollowPages
// (paginated list envelope) and ForEach (fan-out byKey envelope) shapes
// documented on Step and produced by executor.go's fetchPaginated/fetchFanOut.
// required names response fields that must be present on each item.
func buildStepSchema(step Step, op *sempv2.Operation, required []string) map[string]any {
	if op == nil || op.ResponseFields == nil {
		return permissiveStepSchema()
	}
	item := fieldPropertiesSchema(op.ResponseFields, required)

	switch {
	case step.FollowPages:
		// fetchPaginated always sets data + truncated; truncatedMessage only
		// appears when truncated is true. See executor.go.
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"data":             map[string]any{"type": "array", "items": item},
				"truncated":        map[string]any{"type": "boolean"},
				"truncatedMessage": map[string]any{"type": "string"},
			},
			"required":             []string{"data", "truncated"},
			"additionalProperties": false,
		}
	case step.ForEach != "":
		// fetchFanOut always sets byKey; skipped only appears when nonzero.
		// See executor.go.
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"byKey":   map[string]any{"type": "object", "additionalProperties": item},
				"skipped": map[string]any{"type": "integer"},
			},
			"required":             []string{"byKey"},
			"additionalProperties": false,
		}
	default:
		return item
	}
}

// permissiveStepSchema is the fallback for a step whose response shape is
// unknown — an operation extractResponseFields couldn't resolve, or a step
// with no matching operation at all. Matches StepKeyedEnvelopeSchema's
// original per-step permissiveness so an unintrospectable step doesn't start
// rejecting every field the day this schema replaces the generic one.
func permissiveStepSchema() map[string]any {
	return map[string]any{"type": "object"}
}

// fieldPropertiesSchema builds a strict object schema — properties, optional
// required list, additionalProperties: false — from resolved response
// fields. Shared by the flat, paginated-item, and fan-out-item cases in
// buildStepSchema.
func fieldPropertiesSchema(fields map[string]string, required []string) map[string]any {
	properties := make(map[string]any, len(fields))
	for name, jsonType := range fields {
		properties[name] = map[string]any{"type": jsonType}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
