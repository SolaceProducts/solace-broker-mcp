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
	"fmt"
	"strings"

	"github.com/xeipuuv/gojsonschema"
)

// ValidateParams validates tool input parameters against a JSON Schema (Draft 7).
// All validation errors are collected and returned in a single error message with
// schema paths, e.g.:
//
//	"parameter validation failed: (root).queueName: queueName is required;
//	 (root).maxResults: Invalid type. Expected: integer, given: string"
//
// Returns nil if validation passes.
func ValidateParams(params map[string]any, schema map[string]any) error {
	return validateAgainstSchema(params, schema, "parameter validation failed")
}

// ValidateOutput validates a tool's structured output against its output JSON
// Schema. Uses the same collection and formatting strategy as ValidateParams.
// Returns nil if validation passes.
func ValidateOutput(result map[string]any, schema map[string]any) error {
	return validateAgainstSchema(result, schema, "output validation failed")
}

// validateAgainstSchema validates a JSON document against a JSON Schema using
// gojsonschema. All errors are collected into a single message prefixed with
// the given label.
//
// This re-marshals and re-parses schema on every call, which is fine for the
// ad-hoc/test callers of ValidateParams and ValidateOutput above, but wrong
// for a tool's schema on the CallTool hot path — a tool's schema never
// changes after registration. That path uses compileSchema +
// validateAgainstCompiledSchema instead (SOL-153334), which compiles the
// schema once and reuses it.
func validateAgainstSchema(document map[string]any, schema map[string]any, errPrefix string) error {
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("%s: marshalling schema: %w", errPrefix, err)
	}

	docJSON, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("%s: marshalling document: %w", errPrefix, err)
	}

	schemaLoader := gojsonschema.NewBytesLoader(schemaJSON)
	docLoader := gojsonschema.NewBytesLoader(docJSON)

	result, err := gojsonschema.Validate(schemaLoader, docLoader)
	if err != nil {
		return fmt.Errorf("%s: schema validation error: %w", errPrefix, err)
	}

	return formatValidationResult(result, errPrefix)
}

// compileSchema compiles a JSON Schema once so it can be validated against
// many documents without re-marshaling and re-parsing the schema itself on
// every call (SOL-153334). Callers that validate the same schema repeatedly
// — ToolManager, once per registered tool — should compile it once (at
// registration) and reuse the result via validateAgainstCompiledSchema.
func compileSchema(schema map[string]any) (*gojsonschema.Schema, error) {
	schemaJSON, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshalling schema: %w", err)
	}
	compiled, err := gojsonschema.NewSchema(gojsonschema.NewBytesLoader(schemaJSON))
	if err != nil {
		return nil, fmt.Errorf("compiling schema: %w", err)
	}
	return compiled, nil
}

// validateAgainstCompiledSchema validates document against a schema compiled
// by compileSchema, without re-marshaling or re-parsing the schema itself —
// only document is marshaled, once. It also returns that marshaled JSON so a
// caller that needs the bytes anyway (ToolManager, for the tool result's text
// fallback) doesn't have to marshal document a second time. The bytes are
// still returned alongside a validation error, since a caller logging or
// inspecting the invalid document may still want them.
func validateAgainstCompiledSchema(document map[string]any, schema *gojsonschema.Schema, errPrefix string) ([]byte, error) {
	docJSON, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("%s: marshalling document: %w", errPrefix, err)
	}

	result, err := schema.Validate(gojsonschema.NewBytesLoader(docJSON))
	if err != nil {
		return docJSON, fmt.Errorf("%s: schema validation error: %w", errPrefix, err)
	}

	return docJSON, formatValidationResult(result, errPrefix)
}

// formatValidationResult turns a gojsonschema result into nil (valid) or a
// single error collecting every violation, prefixed with errPrefix. Shared by
// the uncompiled and compiled validation paths so both produce identical
// error text.
func formatValidationResult(result *gojsonschema.Result, errPrefix string) error {
	if result.Valid() {
		return nil
	}

	var msgs []string
	for _, desc := range result.Errors() {
		msgs = append(msgs, desc.String())
	}

	return fmt.Errorf("%s: %s", errPrefix, strings.Join(msgs, "; "))
}
