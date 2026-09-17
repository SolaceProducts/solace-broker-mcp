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
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/SolaceProducts/solace-broker-mcp/internal/auth"
	"github.com/SolaceProducts/solace-broker-mcp/internal/observability/metrics"
	"github.com/SolaceProducts/solace-broker-mcp/internal/semp/sempv2"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Registered outside ToolManager like list-brokers — spec content only, no
// broker state, so runtime authorization never wraps it.
const describeSempSchemaToolName = "describe-semp-schema"

type schemaOpInfo struct {
	specType string // "config" | "monitor" | "action"
	method   string // "POST" | "PATCH" | "PUT" | "GET" | "DELETE"
	defName  string // e.g., "MsgVpnQueue"; empty when the op has no body definition
}

type sempSchemaMap struct {
	specs map[string]map[string]any // specType -> parsed spec (generic map preserves x-* extensions)
	ops   map[string]schemaOpInfo   // "specType/operationId" -> info
}

// buildSempSchemaMap parses each *.json file in fsys as an OpenAPI 2.0 spec.
// Uses generic maps rather than the openapi2 struct so SEMP's x-* vendor
// extensions (which carry the writability signal) survive downstream.
func buildSempSchemaMap(fsys fs.FS) (*sempSchemaMap, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("reading specs directory: %w", err)
	}

	reg := &sempSchemaMap{
		specs: make(map[string]map[string]any),
		ops:   make(map[string]schemaOpInfo),
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("reading spec file %q: %w", entry.Name(), err)
		}
		var spec map[string]any
		if err := json.Unmarshal(data, &spec); err != nil {
			return nil, fmt.Errorf("parsing spec file %q: %w", entry.Name(), err)
		}
		basePath, _ := spec["basePath"].(string)
		specType, err := sempv2.DeriveSpecType(basePath)
		if err != nil {
			return nil, fmt.Errorf("spec file %q: %w", entry.Name(), err)
		}
		// Monitor operations are all GETs with no request bodies, so they have
		// no configurable attributes to describe. Skip the spec entirely rather
		// than indexing operations that would only ever return an empty result.
		if specType == "monitor" {
			continue
		}
		reg.specs[specType] = spec

		paths, _ := spec["paths"].(map[string]any)
		sharedParams, _ := spec["parameters"].(map[string]any)
		for _, pathItem := range paths {
			item, _ := pathItem.(map[string]any)
			for _, m := range []string{"get", "post", "put", "patch", "delete"} {
				opDef, _ := item[m].(map[string]any)
				if opDef == nil {
					continue
				}
				opID, _ := opDef["operationId"].(string)
				if opID == "" {
					continue
				}
				defName := resolveBodyDefinition(opDef, sharedParams)
				reg.ops[specType+"/"+opID] = schemaOpInfo{
					specType: specType,
					method:   strings.ToUpper(m),
					defName:  defName,
				}
			}
		}
	}
	if len(reg.ops) == 0 {
		return nil, fmt.Errorf("no operations found in spec files")
	}
	return reg, nil
}

// resolveBodyDefinition returns the definition name referenced by the
// operation's body parameter, or "" if there is none.
func resolveBodyDefinition(opDef, sharedParams map[string]any) string {
	params, _ := opDef["parameters"].([]any)
	for _, p := range params {
		param, _ := p.(map[string]any)
		if ref, _ := param["$ref"].(string); ref != "" {
			param, _ = sharedParams[strings.TrimPrefix(ref, "#/parameters/")].(map[string]any)
		}
		if in, _ := param["in"].(string); in != "body" {
			continue
		}
		schema, _ := param["schema"].(map[string]any)
		ref, _ := schema["$ref"].(string)
		return strings.TrimPrefix(ref, "#/definitions/")
	}
	return ""
}

// describe returns the schema slice for an operation. view "raw" returns the
// definition verbatim; "trimmed" returns an agent-friendly per-attribute list.
func (r *sempSchemaMap) describe(operation, view string) (map[string]any, error) {
	info, ok := r.ops[operation]
	if !ok {
		return nil, fmt.Errorf("unknown operation %q; expected form is \"<specType>/<operationId>\" (e.g., config/createMsgVpnQueue) — take the value from the description of the write tool you're planning to call", operation)
	}
	resp := map[string]any{"operation": operation, "method": info.method}
	if info.defName == "" {
		resp["note"] = "operation has no request body definition; no attributes to describe"
		resp["attributes"] = []any{}
		return resp, nil
	}
	defs, _ := r.specs[info.specType]["definitions"].(map[string]any)
	def, _ := defs[info.defName].(map[string]any)
	if def == nil {
		return nil, fmt.Errorf("definition %q not found in %q spec (spec inconsistency)", info.defName, info.specType)
	}
	resp["definition"] = info.defName
	if view == "raw" {
		resp["schema"] = def
	} else {
		resp["attributes"] = trimAttributes(def, defs)
	}
	return resp, nil
}

// omitWhenFalse maps SEMP x-* boolean extensions to the trimmed-view output
// key. Absence is treated as false, so we skip false values to keep output
// lean. writableOnCreate/writableOnUpdate use the inverse pattern (always
// emitted, see below) and are not in this table.
var omitWhenFalse = []struct{ out, x string }{
	{"identifying", "x-identifying"},
	{"requiredForCreate", "x-requiredPost"},
	{"writeOnly", "x-writeOnly"},
	{"sensitive", "x-sensitive"},
	{"deprecated", "x-deprecated"},
}

// trimAttributes flattens a definition's properties into a compact per-
// attribute list carrying only the fields an agent needs to plan a
// create/update: name, type, description, default, enum, constraints, and
// writability flags derived from SEMP's x-* extensions.
// defs is the spec's top-level definitions map, used to resolve $ref properties.
func trimAttributes(def map[string]any, defs map[string]any) []map[string]any {
	props, _ := def["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		prop, _ := props[name].(map[string]any)
		if prop == nil {
			continue
		}
		attr := map[string]any{"name": name}

		// Bare $ref: resolve one level and inline the sub-definition's properties.
		// The ref itself carries no x-* extensions, so fabricating writability
		// flags from absent fields is wrong — surface the nested shape instead.
		if ref, _ := prop["$ref"].(string); ref != "" {
			attr["type"] = "object"
			defName := strings.TrimPrefix(ref, "#/definitions/")
			if resolvedDef, _ := defs[defName].(map[string]any); resolvedDef != nil {
				if desc, _ := resolvedDef["description"].(string); desc != "" {
					attr["description"] = strings.TrimSpace(desc)
				}
				attr["properties"] = trimAttributes(resolvedDef, defs)
			}
			out = append(out, attr)
			continue
		}

		if t, ok := prop["type"].(string); ok {
			attr["type"] = t
		}
		// Keep the first paragraph (the semantic meaning) and the <pre> enum
		// block (the per-value descriptions). The middle paragraphs — access
		// scope levels and config-sync notes — are uniform boilerplate.
		if s, _ := prop["description"].(string); s != "" {
			desc := s
			if i := strings.Index(s, "\n\n"); i != -1 {
				first := s[:i]
				if preStart := strings.Index(s, "<pre>"); preStart != -1 {
					if preEnd := strings.LastIndex(s, "</pre>"); preEnd != -1 {
						first = first + "\n\n" + strings.TrimSpace(s[preStart:preEnd+len("</pre>")])
					}
				}
				desc = first
			}
			if desc = strings.TrimSpace(desc); desc != "" {
				attr["description"] = desc
			}
		}
		if e, ok := prop["enum"].([]any); ok && len(e) > 0 {
			attr["enum"] = e
		}
		if v, ok := prop["x-default"]; ok {
			attr["default"] = v
		}
		if s, ok := prop["pattern"].(string); ok && s != "" {
			attr["pattern"] = s
		}
		if v, ok := prop["maxLength"]; ok {
			attr["maxLength"] = v
		}
		if v, ok := prop["minimum"]; ok {
			attr["minimum"] = v
		}
		if v, ok := prop["maximum"]; ok {
			attr["maximum"] = v
		}
		// writableOnCreate/writableOnUpdate are always emitted, even when false —
		// their negative value is the signal the caller asked for, so omitting
		// them would let the agent read absence as "writable."
		readOnlyOnCreate, _ := prop["x-readOnlyPost"].(bool)
		readOnlyOnUpdate, _ := prop["x-readOnlyOther"].(bool)
		attr["writableOnCreate"] = !readOnlyOnCreate
		attr["writableOnUpdate"] = !readOnlyOnUpdate
		for _, f := range omitWhenFalse {
			if b, _ := prop[f.x].(bool); b {
				attr[f.out] = true
			}
		}
		if rd, ok := prop["x-requiresDisable"].([]any); ok && len(rd) > 0 {
			attr["requiresDisable"] = rd
		}
		// x-autoDisable lists attributes that are temporarily set to false when
		// this attribute changes on a live object (e.g. egressEnabled on permission
		// change). An agent must surface this to the user before invoking update.
		if ad, ok := prop["x-autoDisable"].([]any); ok && len(ad) > 0 {
			attr["autoDisable"] = ad
		}
		out = append(out, attr)
	}
	return out
}

// describeSempSchemaOutputSchema declares the shape of this tool's
// structuredContent (SOL-153694). Tools on the ToolManager registration path
// inherit an output schema from it; this one registers directly on the server
// (see RegisterDescribeSempSchema), as list-brokers does, so — like that one —
// its schema is written out by hand here.
//
// Only operation and method are required, and which of the rest appear depends
// on the view AND on whether the operation has a request body. describe()
// returns early for a bodyless operation, before the view is consulted, so
// that branch emits note and an empty attributes for BOTH views and never
// definition or schema. With a body: definition always, then attributes in the
// trimmed view or schema in the raw one. So attributes is absent only from the
// raw view of an operation that has a request body — 287 of the 633 indexed
// operations. TestDescribeSempSchema_RawViewOfBodylessOperationEmitsAttributes
// pins that asymmetry, since it is the easiest thing here to state backwards.
//
// Closed at the root and in the attribute definition — and since the nesting
// recursion reuses that one definition, every depth is closed by it. describe()
// and trimAttributes are the only producers of these documents, so a field
// added there without a matching entry here should fail loudly rather than ship
// undocumented. Two tests stand in for the runtime validation this tool does
// not get, since it bypasses ToolManager:
// TestDescribeSempSchema_OutputMatchesDeclaredSchema validates real output for
// every indexed operation in both views, and
// TestDescribeSempSchema_OutputSchemaRejectsUndeclaredFields proves the closure
// is real at all three instance levels.
//
// This describes the SUCCESS shape only. A panic unwinding through
// withRecovery (internal/tools/register.go) is answered with a
// structuredContent of {error, retryable}, which this schema rejects — it
// requires operation and method and is closed. That costs nothing today, since
// nothing validates this tool's output at runtime, but a client that
// validates would reject a panic result. Describing both shapes would mean a
// oneOf whose error arm is the generic local-error envelope every tool shares,
// which belongs with that envelope rather than here.
//
// "definitions"/"#/definitions/..." is the draft-04/07 spelling, chosen
// deliberately: gojsonschema — the validator this repo compiles schemas with
// (internal/tools/validation.go) — is a draft-04/06/07 implementation, and
// "$defs" is the 2020-12 rename, which it does not resolve. The consequence
// is that only gojsonschema is exercised against this schema; a client on a
// 2020-12-only validator that does not accept the older keyword would not
// resolve the recursion. Reconsider the spelling if this repo's validator
// moves to 2020-12.
func describeSempSchemaOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "The operation that was described, echoed back in the '<specType>/<operationId>' form it was requested as.",
			},
			"method": map[string]any{
				"type":        "string",
				"description": "The HTTP method the operation uses, e.g. POST for a create or PATCH for an update.",
			},
			"definition": map[string]any{
				"type":        "string",
				"description": "Name of the SEMPv2 request-body definition backing the operation. Absent when the operation takes no request body.",
			},
			"note": map[string]any{
				"type":        "string",
				"description": "Present only when the operation has no request-body definition, stating that there are no attributes to describe.",
			},
			"attributes": map[string]any{
				"type":        "array",
				"description": "One entry per configurable attribute, sorted by name. Present in the trimmed view, and in the raw view when the operation takes no request body (then always empty, because the no-body path returns before the view is consulted). Absent only from the raw view of an operation that has a request body.",
				"items":       map[string]any{"$ref": "#/definitions/attribute"},
			},
			"schema": map[string]any{
				"type":        "object",
				"description": "Raw view only: the OpenAPI definition object verbatim, including every SEMP x-* extension. Deliberately unconstrained, being a passthrough of the spec.",
			},
		},
		"required":             []string{"operation", "method"},
		"additionalProperties": false,
		"definitions": map[string]any{
			"attribute": map[string]any{
				"type":        "object",
				"description": "One attribute of the request body, as produced by trimAttributes.",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string"},
					"type":        map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					// enum values and defaults carry the spec's own types
					// (string, number, boolean), so their items and value are
					// left unconstrained.
					"enum":      map[string]any{"type": "array"},
					"default":   map[string]any{},
					"pattern":   map[string]any{"type": "string"},
					"maxLength": map[string]any{"type": "number"},
					"minimum":   map[string]any{"type": "number"},
					"maximum":   map[string]any{"type": "number"},
					// Always emitted for a plain attribute and always absent
					// for a $ref-backed one, which carries nested properties
					// instead — so neither can be required here.
					"writableOnCreate": map[string]any{"type": "boolean"},
					"writableOnUpdate": map[string]any{"type": "boolean"},
					// Emitted only when true (see omitWhenFalse).
					"identifying":       map[string]any{"type": "boolean"},
					"requiredForCreate": map[string]any{"type": "boolean"},
					"writeOnly":         map[string]any{"type": "boolean"},
					"sensitive":         map[string]any{"type": "boolean"},
					"deprecated":        map[string]any{"type": "boolean"},
					"requiresDisable":   map[string]any{"type": "array"},
					"autoDisable":       map[string]any{"type": "array"},
					"properties": map[string]any{
						"type":        "array",
						"description": "Nested attribute list for a $ref-backed object attribute, the same shape as the top-level attributes list.",
						"items":       map[string]any{"$ref": "#/definitions/attribute"},
					},
				},
				"required":             []string{"name"},
				"additionalProperties": false,
			},
		},
	}
}

// RegisterDescribeSempSchema registers describe-semp-schema as a standalone tool —
// same shape as RegisterListBrokers, no broker resolution, no policy wrapping.
func RegisterDescribeSempSchema(server *mcp.Server, fsys fs.FS, tm *metrics.ToolMetrics) error {
	reg, err := buildSempSchemaMap(fsys)
	if err != nil {
		return fmt.Errorf("building semp schema map: %w", err)
	}

	description := strings.TrimSpace(`
Return the SEMPv2 schema slice for a given operation's request-body definition,
so the caller can enumerate every configurable attribute (with types, defaults,
enum values, and writability flags) before invoking a create, update, or action
tool.

Scope: request-body definitions across the config and action SEMPv2 APIs. It
does not describe response payloads, monitor-API objects, message/topic
schemas, or SEMPv1.

Use this to plan a write: call describe-semp-schema first with the operation the
target write tool wraps (see the write tool's description for the operation
identifier, e.g. config/createMsgVpnQueue), then supply attributes that are
writable for that operation. Response has two views: 'trimmed' (default;
per-attribute {name, type, description, enum, default, pattern, maxLength,
minimum, maximum, writableOnCreate, writableOnUpdate, requiredForCreate,
identifying, writeOnly, sensitive, deprecated, autoDisable, requiresDisable};
object-typed attributes that are $ref-backed carry a nested properties list
instead of writability flags) and 'raw' (the definition verbatim, larger).
`)

	tool := &mcp.Tool{
		Name:        describeSempSchemaToolName,
		Description: description,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type":        "string",
					"description": "The SEMPv2 operation identifier in the form '<specType>/<operationId>', e.g. 'config/createMsgVpnQueue'. specType must be 'config' or 'action' — monitor operations are not indexed. Take the value from the description of the write tool you're planning to call.",
				},
				"view": map[string]any{
					"type":        "string",
					"enum":        []string{"trimmed", "raw"},
					"default":     "trimmed",
					"description": "Response shape. 'trimmed' is an attribute list with just the fields an agent needs to plan a write. 'raw' returns the OpenAPI definition object verbatim (larger; includes every SEMP x-* extension).",
				},
			},
			"required": []string{"operation"},
		},
		OutputSchema: describeSempSchemaOutputSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint: true,
		},
	}

	server.AddTool(tool, withRecovery(describeSempSchemaToolName, func(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, err error) {
		// This handler bypasses ToolManager.CallTool, so it emits its own
		// audit line. Panic contract: both result and toolErr nil at defer
		// time means a panic is unwinding.
		start := time.Now()
		var brokerAlias string
		var errorType metrics.ErrorType
		var toolErr error
		id := NewIdentityFromPrincipal(auth.PrincipalFrom(ctx))

		// ...and its own dispatch span, for the same reason (SOL-152421):
		// without it neither this tool nor the `not_found` error_type it
		// raises ever appears in a trace, though both appear in the metrics.
		ctx, span := tracer.Start(ctx, dispatchSpanName)

		// Registered BEFORE the emission defer below, so LIFO runs it AFTER
		// that one — once a recovered panic has been reclassified, so the span
		// reports the same cause the log line and the metric do. Reversed, the
		// span reports nothing while both of them say `panic`. See
		// endDispatchSpan for the full rationale.
		defer func() {
			endDispatchSpan(ctx, span, describeSempSchemaToolName, brokerLabelNone, errorType, toolErr)
		}()

		defer func() {
			if toolErr == nil && result == nil {
				errorType = metrics.ErrorTypePanic
				toolErr = panicError{}
			}
			logToolResult(ctx, describeSempSchemaToolName, &brokerAlias, start, &errorType, &toolErr, nil, id)
			recordToolInvocation(ctx, tm, describeSempSchemaToolName, brokerLabelNone, start, errorType, toolErr)
		}()

		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if uErr := json.Unmarshal(req.Params.Arguments, &args); uErr != nil {
				errorType = metrics.ErrorTypeBadRequest
				toolErr = fmt.Errorf("parsing tool arguments: %w", uErr)
				// This and the next three returns are the caller's mistake, so
				// they leave as -32602 (Invalid Params) rather than the -32603
				// withRecovery would otherwise assign (SOL-153692). Only the
				// returned value is coded: toolErr stays bare so the audit,
				// metric, and span defers above see the error they always
				// have. Still a JSON-RPC error rather than an isError result;
				// that classification is SOL-153693's to change.
				return nil, withJSONRPCCode(toolErr, jsonrpc.CodeInvalidParams)
			}
		}
		operation, _ := args["operation"].(string)
		if operation == "" {
			errorType = metrics.ErrorTypeBadRequest
			toolErr = fmt.Errorf("missing required parameter 'operation'")
			return nil, withJSONRPCCode(toolErr, jsonrpc.CodeInvalidParams)
		}
		view, _ := args["view"].(string)
		if view == "" {
			view = "trimmed"
		}
		if view != "trimmed" && view != "raw" {
			errorType = metrics.ErrorTypeBadRequest
			toolErr = fmt.Errorf("invalid view %q; expected 'trimmed' or 'raw'", view)
			return nil, withJSONRPCCode(toolErr, jsonrpc.CodeInvalidParams)
		}

		structured, dErr := reg.describe(operation, view)
		if dErr != nil {
			errorType = metrics.ErrorTypeNotFound
			toolErr = dErr
			return nil, withJSONRPCCode(toolErr, jsonrpc.CodeInvalidParams)
		}
		resultJSON, mErr := json.MarshalIndent(structured, "", "  ")
		if mErr != nil {
			errorType = metrics.ErrorTypeMarshalError
			toolErr = fmt.Errorf("marshalling schema slice: %w", mErr)
			// Server-side failure: left to withRecovery's -32603 default.
			return nil, toolErr
		}
		return &mcp.CallToolResult{
			StructuredContent: structured,
			Content:           []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}},
		}, nil
	}))
	return nil
}
