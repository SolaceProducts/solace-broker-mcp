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
	"slices"

	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
)

// BuildStrictOutputSchema generates a step-keyed JSON Schema for a composite
// tool's output, deriving each step's per-field schema from its operation's
// resolved response fields (sempv2.Operation.ResponseFields) instead of the
// generic permissive envelope (StepKeyedEnvelopeSchema) every composite tool
// used before SOL-152947. Fields are typed if present but not required,
// except any field named in requiredFields for that step ID — a broker
// legitimately omits other optional/feature-gated attributes, but a response
// that doesn't name its own resource is broken, not sparse.
//
// Strictness stops where the payload stops being ours (SOL-154164). The levels
// this server assembles — this step-keyed top level, and buildStepSchema's
// paginated/fan-out wrappers — reject unknown keys, because an unexpected key
// there means the executor's own result assembly is wrong. The levels the
// broker supplies — the SEMP response envelope and the resource inside it —
// tolerate keys the embedded spec doesn't declare; see fieldPropertiesSchema
// for why rejecting them is worse than accepting them.
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

// buildStepSchema generates one step's schema, honoring the ForEach (fan-out
// byKey envelope) and FollowPages (paginated list envelope) shapes documented
// on Step and produced by executor.go's fetchFanOut/fetchPaginated. required
// names response fields that must be present on each item.
//
// Checked in the same order runStep dispatches (ForEach first: see
// runStep/runSingle in executor.go) so a step with both flags set — fetchFanOut
// calling runSingle per row, which itself honors FollowPages — gets the byKey
// wrapper its runtime result actually has, not the bare paginated shape. No
// step sets both today, but getting the order wrong here would silently
// mismatch runtime behavior the day one does.
func buildStepSchema(step Step, op *sempv2.Operation, required []string) map[string]any {
	if op == nil || op.ResponseFields == nil {
		// NOTE: this guard runs before the ForEach/FollowPages switch below, so
		// a fan-out or paginated step whose ResponseFields didn't resolve gets
		// the bare permissive fallback rather than at least the known envelope
		// shape (byKey, or data/truncated) with permissive items inside.
		// Unreached today — every write tool (SOL-152947's scope) is a
		// single-step "collect" strategy with neither flag set — but worth
		// tightening if this generator is ever extended to monitor tools,
		// which do have fan-out/paginated steps.
		return permissiveStepSchema()
	}
	item := fieldPropertiesSchema(op.ResponseFields, required)

	switch {
	case step.ForEach != "":
		// fetchFanOut runs runSingle per row and keys the result by row —
		// runSingle's non-paginated return is result.Data verbatim, the same
		// raw SEMP envelope {"data": ..., "meta": {...}, "links": {...}} the
		// default case below wraps, not the unwrapped item. A first version of
		// this case used item directly, which every real fan-out call would
		// have rejected the same way an early version of the default case did
		// ("Additional property data/meta/links is not allowed") — caught in
		// review before any fan-out write tool exercised it (none exist yet;
		// list-vpns, the only ForEach tool in the catalog today, is
		// monitor-only and never reaches this function). fetchFanOut always
		// sets byKey; skipped only appears when nonzero. See executor.go.
		return map[string]any{
			"type": "object",
			"properties": map[string]any{
				"byKey":   map[string]any{"type": "object", "additionalProperties": envelopeSchema(item)},
				"skipped": map[string]any{"type": "integer"},
			},
			"required":             []string{"byKey"},
			"additionalProperties": false,
		}
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
	default:
		// A non-paginated, non-fan-out step's result is runSingle's return of
		// result.Data verbatim — the RAW parsed HTTP response body
		// (json.Unmarshal(body, &data) in client.go), i.e. the whole SEMP
		// envelope {"data": ..., "meta": {...}, "links": {...}}, not
		// pre-unwrapped to the item alone. Confirmed against a real broker
		// via the e2e-management suite: a first version of this schema that
		// used item directly here rejected every real create/update response
		// ("Additional property data/links/meta is not allowed").
		return envelopeSchema(item)
	}
}

// envelopeSchema wraps an item schema in the SEMP response envelope shape —
// {"data": item, "meta": {...}, "links": {...}}, only "data" required. Shared
// by the flat (default) and fan-out-item cases in buildStepSchema, both of
// which receive the raw envelope at runtime (see buildStepSchema's case
// comments). Only "data" is required — the swagger envelope schemas mark
// "meta" as the sole required envelope field, but a create/update tool
// returning no "data" would be meaningless to the caller regardless of what
// the spec technically permits.
//
// Additional envelope keys are tolerated for the same reason
// fieldPropertiesSchema tolerates additional attributes (SOL-154164): this
// object's key set is the broker's, not ours.
func envelopeSchema(item map[string]any) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"data":  item,
			"meta":  map[string]any{"type": "object"},
			"links": map[string]any{"type": "object"},
		},
		"required": []string{"data"},
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

// fieldPropertiesSchema builds an object schema — properties, plus an optional
// required list — from resolved response fields. Shared by the flat,
// paginated-item, and fan-out-item cases in buildStepSchema.
//
// Deliberately NOT additionalProperties: false (SOL-154164). This object is a
// SEMP resource as the live broker returned it, and the field list comes from
// the SEMPv2 spec embedded at build time. A broker newer than that spec
// legitimately echoes attributes its own release added, and every attribute
// SEMP has ever added to an existing object has been additive. Rejecting them
// here fails the tool call AFTER the handler has already applied the mutation
// (ToolManager.CallTool validates output post-execution), so a create/update
// that genuinely succeeded is reported to the agent as IsError — the one
// outcome a destructive tool must never produce, and the same spec/broker
// mismatch the request side already tolerates (see constructRequestBody's
// versioned-server-aware handling in executor.go).
//
// What the caller still gets is the part that catches client-breaking drift:
// "properties" types every field the spec does declare (a retyped field is
// rejected), and "required" pins the resource's own identifier (a response
// that doesn't name what it just created is rejected). Strictness is retained
// only on the levels this server assembles itself — the step-keyed top level in
// BuildStrictOutputSchema, and the paginated/fan-out wrappers in
// buildStepSchema — where an unexpected key means our bug, not broker drift.
//
// required is cloned before being stored, not stored as-is: callers (e.g.
// CompositeToolHandler.outputSchema) pass the same slice value straight
// through from the package-level writeToolIdentifierFields map, and a caller
// of the generated schema mutating the returned "required" slice would
// otherwise corrupt that map for every future call to every handler —
// confirmed by mutating one Metadata() call's returned schema and observing
// a brand-new handler instance return the corrupted value on its next call.
func fieldPropertiesSchema(fields map[string]string, required []string) map[string]any {
	properties := make(map[string]any, len(fields))
	for name, jsonType := range fields {
		properties[name] = map[string]any{"type": jsonType}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = slices.Clone(required)
	}
	return schema
}
